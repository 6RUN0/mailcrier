package spool

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Areas of the spool directory. An entry lives in one of queue, hold and
// failed as <id>.eml, the message, and <id>.json, the sidecar; tmp holds
// files being written, locks the queue run locks.
const (
	TmpDir    = "tmp"
	QueueDir  = "queue"
	HoldDir   = "hold"
	FailedDir = "failed"
	LocksDir  = "locks"
)

// Modes of what the spool creates. Only the owner and the group get in:
// with a setgid install the group is the service group, and the caller
// is not in it.
const (
	DirMode  = 0o770 | fs.ModeSetgid
	FileMode = 0o660
)

// Suffixes of the files of an entry; nextSuffix marks a sidecar being
// rewritten in tmp/.
const (
	messageSuffix = ".eml"
	entrySuffix   = ".json"
	nextSuffix    = ".next.json"
)

// Errors of Create; the caller exits 73 for ErrCreate and 74 for ErrWrite.
var (
	// ErrCreate means no entry file could be created: the directory is
	// missing or not writable, or a quota is exhausted.
	ErrCreate = errors.New("spool entry not created")
	// ErrWrite means writing, syncing or renaming an entry file failed.
	ErrWrite = errors.New("spool entry not written")
	// ErrQuota wraps ErrCreate when a limit keeps the entry out.
	ErrQuota = fmt.Errorf("%w: quota exceeded", ErrCreate)
)

// Errors of Lock and LockRun.
var (
	// ErrBusy means another process holds the lock.
	ErrBusy = errors.New("locked by another process")
	// ErrGone means the entry was finished or moved meanwhile.
	ErrGone = errors.New("entry gone")
	// ErrOrphanRemoved wraps ErrGone when Lock found the message file
	// without its sidecar, the trace of an interrupted Remove, and
	// deleted it.
	ErrOrphanRemoved = fmt.Errorf("%w: message without sidecar removed", ErrGone)
)

// afterMoveStep and afterCreateStep, when set by a test, run after each
// rename of Move and each step of Create that puts the entry in its area,
// so that a test can look at the files or die between them.
var afterMoveStep, afterCreateStep func(step string)

// Spool is a spool directory. Its areas must be on a local file system:
// flock does not reliably exclude processes over NFS.
type Spool struct {
	dir string
}

// Open returns the spool at dir, which must exist, and creates the areas
// that are missing, for a container that uses any directory its user can
// write. With a setgid install the packages create them: made here, they
// would belong to the calling user. The error wraps ErrCreate.
func Open(dir string) (*Spool, error) {
	sp, err := OpenExisting(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCreate, err)
	}
	for _, area := range []string{TmpDir, QueueDir, HoldDir, FailedDir, LocksDir} {
		path := filepath.Join(dir, area)
		if err := os.Mkdir(path, DirMode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("%w: %w", ErrCreate, err)
		}
		// Mkdir applies the umask and may drop the setgid bit.
		if err := os.Chmod(path, DirMode); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCreate, err)
		}
	}
	return sp, nil
}

// OpenExisting returns the spool at dir without creating anything, for
// the modes that only read it; a missing area reads as empty. The error
// wraps fs.ErrNotExist when dir does not exist.
func OpenExisting(dir string) (*Spool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return &Spool{dir: dir}, nil
}

// CheckWritable returns an error naming the first directory a new entry
// needs that this process cannot write: an area, the spool directory in
// place of a missing area, or the file system when it is mounted
// read-only. It creates nothing, so the listing modes may call it. The
// check uses the effective ids, those of the group of a setgid binary.
func (s *Spool) CheckWritable() error {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.dir, &stat); err != nil {
		return &fs.PathError{Op: "statfs", Path: s.dir, Err: err}
	}
	// faccessat without faccessat2, before Linux 5.8 or under an old
	// seccomp profile, checks the mode bits only.
	if stat.Flags&unix.ST_RDONLY != 0 {
		return &fs.PathError{Op: "access", Path: s.dir, Err: unix.EROFS}
	}
	for _, area := range []string{TmpDir, QueueDir, HoldDir, FailedDir, LocksDir} {
		path := s.path(area, "")
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			path = s.dir
		}
		if err := unix.Faccessat(unix.AT_FDCWD, path, unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
			return &fs.PathError{Op: "access", Path: path, Err: err}
		}
	}
	return nil
}

func (s *Spool) path(area, name string) string {
	return filepath.Join(s.dir, area, name)
}

// Record is an entry locked by this process: its message file is held
// with an exclusive flock, which the kernel drops when the process dies,
// so a crashed run never leaves an entry stuck.
type Record struct {
	sp *Spool
	// Area is where the entry lives now.
	Area string
	// Entry is the sidecar as read after the lock was taken, with the
	// changes of this process.
	Entry *Entry
	file  *os.File
	// isRemoved is set once Remove succeeded.
	isRemoved bool
}

// Create writes a new entry into area, locked, and returns it; the caller
// closes it. It reserves the entry in tmp/ at its full size first and
// checks quota afterwards, see reserve. The sidecar is linked into area
// before the message is renamed there and leaves tmp/ only after it, so
// that a message file has its sidecar beside it in tmp/ as well as in
// area: an entry becomes visible to a queue run complete or not at all,
// and Usage always finds its owner.
func (s *Spool) Create(area string, e *Entry, raw []byte, quota Quota) (*Record, error) {
	sidecar, err := Encode(e)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCreate, err)
	}
	tmpMessage, tmpEntry := s.path(TmpDir, e.ID+messageSuffix), s.path(TmpDir, e.ID+entrySuffix)
	file, entryFile, err := s.reserve(e, int64(len(raw)), sidecar, quota)
	if err != nil {
		return nil, err
	}
	rec := &Record{sp: s, Area: area, Entry: e, file: file}
	renamed := false
	fail := func(err error) (*Record, error) {
		_ = entryFile.Close()
		if renamed {
			// Under the lock, so no queue run takes the entry; before the
			// sidecar, so a crash leaves only a sidecar without a message.
			_ = os.Remove(s.path(area, e.ID+messageSuffix))
		}
		_ = rec.Close()
		_ = os.Remove(tmpMessage)
		_ = os.Remove(tmpEntry)
		_ = os.Remove(s.path(area, e.ID+entrySuffix))
		return nil, fmt.Errorf("%w: %w", ErrWrite, err)
	}
	if _, err := file.WriteAt(raw, 0); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := entryFile.Sync(); err != nil {
		return fail(err)
	}
	if err := entryFile.Close(); err != nil {
		return fail(err)
	}
	if err := linkReplacing(tmpEntry, s.path(area, e.ID+entrySuffix)); err != nil {
		return fail(err)
	}
	createStep("sidecar")
	if err := os.Rename(tmpMessage, s.path(area, e.ID+messageSuffix)); err != nil {
		return fail(err)
	}
	renamed = true
	createStep("message")
	if err := syncDir(s.path(area, "")); err != nil {
		return fail(err)
	}
	// What a crash leaves here is a sidecar without a message, which
	// RemoveStale deletes.
	_ = os.Remove(tmpEntry)
	return rec, nil
}

// linkReplacing links the file at oldname as newname, replacing a file
// there: a sidecar that a crash left without its message.
func linkReplacing(oldname, newname string) error {
	err := os.Link(oldname, newname)
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := os.Remove(newname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Link(oldname, newname)
}

func createStep(step string) {
	if afterCreateStep != nil {
		afterCreateStep(step)
	}
}

// reserve creates the entry files in tmp/, the message file locked and
// truncated to size, which Usage counts, and the sidecar with its content,
// from which Usage takes the owner; then it checks quota against the usage
// without this entry, and removes the files when the entry does not fit.
// Reserving before checking keeps parallel calls within the limits without
// a lock that every call would wait for, which a stopped process could
// hold forever: the last call admitted counted after every other admitted
// entry was reserved. Near a limit two parallel calls may both be refused
// where one would fit. Errors wrap ErrCreate, ErrQuota, or ErrWrite when
// the sidecar cannot be written.
func (s *Spool) reserve(e *Entry, size int64, sidecar []byte, quota Quota) (message, entry *os.File, err error) {
	tmpMessage, tmpEntry := s.path(TmpDir, e.ID+messageSuffix), s.path(TmpDir, e.ID+entrySuffix)
	message, err = createFile(tmpMessage, os.O_EXCL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrCreate, err)
	}
	fail := func(kind, err error) (*os.File, *os.File, error) {
		if entry != nil {
			_ = entry.Close()
		}
		_ = message.Close()
		_ = os.Remove(tmpMessage)
		_ = os.Remove(tmpEntry)
		return nil, nil, fmt.Errorf("%w: %w", kind, err)
	}
	if err := flock(message, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(ErrCreate, err)
	}
	if err := message.Truncate(size); err != nil {
		return fail(ErrCreate, err)
	}
	entry, err = createFile(tmpEntry, os.O_EXCL)
	if err != nil {
		return fail(ErrCreate, err)
	}
	// A full disk fails this write as it fails that of the message.
	if _, err := entry.Write(sidecar); err != nil {
		return fail(ErrWrite, err)
	}
	usage, err := s.Usage()
	if err != nil {
		return fail(ErrCreate, err)
	}
	own := size + int64(len(sidecar))
	usage.Messages--
	usage.Bytes -= own
	if count, ok := usage.PerUID[e.OwnerUID]; ok {
		count.Messages--
		count.Bytes -= own
		usage.PerUID[e.OwnerUID] = count
	}
	if err := quota.Admit(usage, e.OwnerUID, own); err != nil {
		return fail(ErrQuota, err)
	}
	return message, entry, nil
}

// createFile opens path for writing with FileMode, whatever the umask,
// and flag: os.O_EXCL for a new file, os.O_TRUNC to replace one.
func createFile(path string, flag int) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|flag, FileMode)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(FileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

// replaceFile writes data to path, replacing what is there, synced. Only
// the holder of the entry lock writes the file of an entry, so a file
// left behind by a killed holder is its own to overwrite.
func replaceFile(path string, data []byte) error {
	file, err := createFile(path, os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// syncDir makes the renames and removals in dir durable; a test replaces
// it to make the sync fail.
var syncDir = func(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func flock(file *os.File, how int) error {
	for {
		err := syscall.Flock(int(file.Fd()), how)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrBusy
		}
		return err
	}
}

// Lock takes the entry id in area without waiting and reads its sidecar
// afterwards, so that the state is the one the previous holder left. It
// returns ErrBusy when another process holds the entry, ErrGone when the
// entry was finished or moved away, and a Decode error, with the entry
// unlocked and untouched, when the sidecar is unreadable.
func (s *Spool) Lock(area, id string) (*Record, error) {
	rec, data, err := s.lockMessage(area, id)
	if err != nil {
		return nil, err
	}
	e, err := decodeSidecar(data, id)
	if err != nil {
		_ = rec.Close()
		return nil, err
	}
	rec.Entry = e
	return rec, nil
}

// LockCorrupt takes the entry id in area, as Lock does, when its sidecar
// is corrupt, and returns it with an Entry made from what the message file
// tells: OwnerUID is the owner of the file, and CreatedAt and ReceivedAt
// are its modification time, which is when the entry was written, since
// the message file is never rewritten. Both are approximate: a copy that
// a run of another uid made into this spool, from the hold/ of another
// directory, is owned by that uid and dated by the copy, and an entry
// released from hold/ keeps the time it was held. The entry has no
// targets: the record serves to move it to failed/, where Move writes a
// valid sidecar, or to remove it. It returns ErrBusy and ErrGone as Lock does, ErrGone
// also when the sidecar decodes meanwhile, and the error of Decode for a
// sidecar of another version.
func (s *Spool) LockCorrupt(area, id string) (*Record, error) {
	rec, data, err := s.lockMessage(area, id)
	if err != nil {
		return nil, err
	}
	_, err = decodeSidecar(data, id)
	if !errors.Is(err, ErrCorrupt) {
		_ = rec.Close()
		if err == nil {
			err = ErrGone
		}
		return nil, err
	}
	info, err := rec.file.Stat()
	if err != nil {
		_ = rec.Close()
		return nil, err
	}
	owner := -1
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		owner = int(stat.Uid)
	}
	rec.Entry = &Entry{Version: Version, ID: id, OwnerUID: owner, CreatedAt: info.ModTime(), ReceivedAt: info.ModTime()}
	return rec, nil
}

// lockMessage takes the message file of the entry id in area without
// waiting and returns it as a record without Entry, with the content of
// the sidecar read after the lock; see Lock for the errors.
func (s *Spool) lockMessage(area, id string) (*Record, []byte, error) {
	path := s.path(area, id+messageSuffix)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrGone
	}
	if err != nil {
		return nil, nil, err
	}
	rec := &Record{sp: s, Area: area, file: file}
	if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = rec.Close()
		return nil, nil, err
	}
	// The previous holder may have finished or moved the entry: then the
	// open file is no longer the one at path.
	held, err := file.Stat()
	if err != nil {
		_ = rec.Close()
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(held, current) {
		_ = rec.Close()
		return nil, nil, ErrGone
	}
	data, err := readFile(s.path(area, id+entrySuffix))
	if errors.Is(err, fs.ErrNotExist) {
		// Only Remove takes the sidecar of an entry away while its
		// message stays: Create and Move put the sidecar in place first
		// and take the old one last. A message file without a sidecar is
		// what a run left that died in the middle of Remove.
		err := ErrGone
		if os.Remove(path) == nil {
			err = ErrOrphanRemoved
		}
		_ = rec.Close()
		return nil, nil, err
	}
	if err != nil {
		_ = rec.Close()
		return nil, nil, err
	}
	return rec, data, nil
}

// decodeSidecar decodes the sidecar data of the entry id; a valid entry
// of another id is corrupt as well.
func decodeSidecar(data []byte, id string) (*Entry, error) {
	e, err := Decode(data)
	if err == nil && e.ID != id {
		err = fmt.Errorf("%w: id does not match the file name", ErrCorrupt)
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Peek reads the sidecar of id in area without locking it: enough for
// fields that never change, such as OwnerUID, and for listing.
func (s *Spool) Peek(area, id string) (*Entry, error) {
	data, err := readFile(s.path(area, id+entrySuffix))
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Has reports whether area holds a message file of id.
func (s *Spool) Has(area, id string) bool {
	return s.exists(area, id+messageSuffix)
}

// readFile is os.ReadFile that does not follow a symbolic link.
func readFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

// ID returns the id of the entry.
func (r *Record) ID() string {
	return r.Entry.ID
}

// Message returns the stored message.
func (r *Record) Message() ([]byte, error) {
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(r.file)
}

// writeNext writes the sidecar to tmp/<id>.next.json, synced, and returns
// its path.
func (r *Record) writeNext() (string, error) {
	data, err := Encode(r.Entry)
	if err != nil {
		return "", err
	}
	next := r.sp.path(TmpDir, r.Entry.ID+nextSuffix)
	return next, replaceFile(next, data)
}

// Save writes the sidecar: a new file in tmp/, synced, renamed over the
// old one, so that a crash leaves either state, never a torn file. The
// directory is not synced: a rename lost in a crash of the system brings
// back the previous state, and at worst a delivery is repeated.
func (r *Record) Save() error {
	next, err := r.writeNext()
	if err != nil {
		return err
	}
	if err := os.Rename(next, r.sp.path(r.Area, r.Entry.ID+entrySuffix)); err != nil {
		_ = os.Remove(next)
		return err
	}
	return nil
}

// Remove deletes the entry: the sidecar first, so that a crash in
// between leaves a message file that Lock recognizes as finished. A
// missing sidecar is the trace of such a removal whose second step
// failed, so Remove goes on to the message. Like Save it does not sync
// the directory. Once it succeeded, Remove does nothing.
func (r *Record) Remove() error {
	if r.isRemoved {
		return nil
	}
	if err := os.Remove(r.sp.path(r.Area, r.Entry.ID+entrySuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(r.sp.path(r.Area, r.Entry.ID+messageSuffix)); err != nil {
		return err
	}
	r.isRemoved = true
	return nil
}

// Move moves the entry to area with its current state. The new sidecar
// goes to area first, then the message, and the old sidecar is removed
// last, so that at every step one area holds the complete entry: a crash
// leaves at most a sidecar without a message, which a queue run ignores
// and RemoveStale deletes. The lock stays with the record.
func (r *Record) Move(area string) error {
	next, err := r.writeNext()
	if err != nil {
		return err
	}
	id, from := r.Entry.ID, r.Area
	if err := os.Rename(next, r.sp.path(area, id+entrySuffix)); err != nil {
		_ = os.Remove(next)
		return err
	}
	moveStep("sidecar")
	if err := os.Rename(r.sp.path(from, id+messageSuffix), r.sp.path(area, id+messageSuffix)); err != nil {
		return err
	}
	r.Area = area
	moveStep("message")
	if err := syncDir(r.sp.path(area, "")); err != nil {
		return err
	}
	if err := syncDir(r.sp.path(from, "")); err != nil {
		return err
	}
	if err := os.Remove(r.sp.path(from, id+entrySuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func moveStep(step string) {
	if afterMoveStep != nil {
		afterMoveStep(step)
	}
}

// Close releases the lock.
func (r *Record) Close() error {
	return r.file.Close()
}

// List returns the ids of the entries in area, oldest first; an area that
// does not exist has none.
func (s *Spool) List(area string) ([]string, error) {
	entries, err := readDir(s.path(area, ""))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), messageSuffix); ok && validID.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, compareIDs)
	return ids, nil
}

// readDir is os.ReadDir with a missing directory read as empty.
func readDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// compareIDs orders ids by time, then by the random part.
func compareIDs(a, b string) int {
	aTime, aRest, _ := strings.Cut(a, "-")
	bTime, bRest, _ := strings.Cut(b, "-")
	if len(aTime) != len(bTime) {
		return len(aTime) - len(bTime)
	}
	if c := strings.Compare(aTime, bTime); c != 0 {
		return c
	}
	return strings.Compare(aRest, bRest)
}

// Run is a queue run lock; Close releases it.
type Run struct {
	file *os.File
}

// Close releases the lock.
func (r *Run) Close() error {
	return r.file.Close()
}

// LockRun takes the queue run lock of uid without waiting; ErrBusy means
// another run of the same user is under way. Runs of different users do
// not exclude each other: the entry locks keep them apart, and a stopped
// process of one user must not hold up the queue of the others.
func (s *Spool) LockRun(uid int) (*Run, error) {
	path := s.path(LocksDir, "drain-"+strconv.Itoa(uid)+".lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, FileMode)
	if err != nil {
		return nil, err
	}
	if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Run{file: file}, nil
}

// RemoveStale deletes what processes that died left behind, last
// modified more than age before now: files in tmp/, except a message file
// a live process holds locked, even a stopped one, and its sidecar; and
// in queue/, hold/ and failed/ sidecars without a message, left by a
// crash inside Create or Move. It returns how many files it removed.
func (s *Spool) RemoveStale(now time.Time, age time.Duration) (int, error) {
	removed := 0
	isStale := func(entry os.DirEntry) bool {
		info, err := entry.Info()
		return err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > age
	}
	remove := func(path string) {
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	entries, err := readDir(s.path(TmpDir, ""))
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), messageSuffix) && isStale(entry) {
			s.removeUnlocked(s.path(TmpDir, entry.Name()), remove)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, messageSuffix) || !isStale(entry) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(name, nextSuffix), entrySuffix)
		if !s.exists(TmpDir, id+messageSuffix) {
			remove(s.path(TmpDir, name))
		}
	}
	for _, area := range []string{QueueDir, HoldDir, FailedDir} {
		entries, err := readDir(s.path(area, ""))
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), entrySuffix)
			if ok && isStale(entry) && !s.exists(area, id+messageSuffix) {
				remove(s.path(area, entry.Name()))
			}
		}
	}
	return removed, nil
}

// removeUnlocked removes the file at path unless another process holds
// its lock.
func (s *Spool) removeUnlocked(path string, remove func(string)) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	if flock(file, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		remove(path)
	}
}

func (s *Spool) exists(area, name string) bool {
	_, err := os.Lstat(s.path(area, name))
	return err == nil
}
