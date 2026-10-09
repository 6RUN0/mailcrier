package spool

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/message"
)

// openSpool returns a spool in a fresh directory.
func openSpool(t *testing.T) *Spool {
	t.Helper()
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// create queues an entry of owner with raw and returns it unlocked.
func create(t *testing.T, sp *Spool, area string, owner int, raw string) *Entry {
	t.Helper()
	e := NewEntry(NewID(testNow), owner, testNow, testNow, message.Envelope{}, []string{"a"})
	rec, err := sp.Create(area, e, []byte(raw), Quota{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return e
}

// names lists the files of an area.
func names(t *testing.T, sp *Spool, area string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(sp.dir, area))
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, entry := range entries {
		all = append(all, entry.Name())
	}
	return all
}

// TestCreateLayout pins where and with which modes an entry lands.
func TestCreateLayout(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "Subject: s\n\nb\n")
	t.Run("T-ADJ-23/renamed-from-tmp-into-queue", func(t *testing.T) {
		if got := names(t, sp, QueueDir); !slices.Equal(got, []string{e.ID + ".eml", e.ID + ".json"}) {
			t.Errorf("queue/ = %v", got)
		}
		if got := names(t, sp, TmpDir); len(got) != 0 {
			t.Errorf("tmp/ = %v, want empty", got)
		}
	})
	t.Run("T-ADJ-23/group-only-modes", func(t *testing.T) {
		for _, area := range []string{TmpDir, QueueDir, HoldDir, FailedDir, LocksDir} {
			info, err := os.Stat(filepath.Join(sp.dir, area))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != DirMode {
				t.Errorf("%s/ mode = %v, want %v", area, got, DirMode)
			}
		}
		for _, name := range names(t, sp, QueueDir) {
			info, err := os.Stat(filepath.Join(sp.dir, QueueDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != FileMode {
				t.Errorf("%s mode = %v, want %v", name, got, os.FileMode(FileMode))
			}
		}
	})
	t.Run("message-stored-unchanged", func(t *testing.T) {
		rec, err := sp.Lock(QueueDir, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rec.Close() }()
		raw, err := rec.Message()
		if err != nil || string(raw) != "Subject: s\n\nb\n" || rec.Entry.OwnerUID != 1000 {
			t.Errorf("Message() = %q, %v; owner %d", raw, err, rec.Entry.OwnerUID)
		}
	})
}

func TestOpen(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrCreate) {
		t.Errorf("Open(missing) error = %v, want ErrCreate", err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, QueueDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("Open() with an existing area error = %v", err)
	}
}

// TestCreateFailures pins the two error classes the exit status tells
// apart: 73 when nothing could be created, 74 when writing failed.
func TestCreateFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	e := func() *Entry {
		return NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	}
	t.Run("T-MTA-36/tmp-not-writable", func(t *testing.T) {
		sp := openSpool(t)
		lockDir(t, filepath.Join(sp.dir, TmpDir))
		if _, err := sp.Create(QueueDir, e(), []byte("x"), Quota{}); !errors.Is(err, ErrCreate) || errors.Is(err, ErrWrite) {
			t.Errorf("Create() error = %v, want ErrCreate", err)
		}
	})
	t.Run("T-MTA-36/queue-not-writable", func(t *testing.T) {
		sp := openSpool(t)
		lockDir(t, filepath.Join(sp.dir, QueueDir))
		if _, err := sp.Create(QueueDir, e(), []byte("x"), Quota{}); !errors.Is(err, ErrWrite) {
			t.Errorf("Create() error = %v, want ErrWrite", err)
		}
		if got := names(t, sp, TmpDir); len(got) != 0 {
			t.Errorf("tmp/ = %v, want the files removed", got)
		}
	})
	t.Run("quota", func(t *testing.T) {
		sp := openSpool(t)
		create(t, sp, QueueDir, 1000, "x")
		if _, err := sp.Create(QueueDir, e(), []byte("x"), Quota{Limits: Limits{MessagesPerUID: 1}}); !errors.Is(err, ErrQuota) || !errors.Is(err, ErrCreate) {
			t.Errorf("Create() error = %v, want ErrQuota", err)
		}
	})
}

// TestCreateSidecarNotWritten pins that a sidecar the file system refuses
// to take is a write error, exit status 74, as for the message: a full
// disk must not read as a missing directory. RLIMIT_FSIZE below the size
// of the sidecar fails its write with EFBIG, as ENOSPC would; the Go
// runtime ignores the SIGXFSZ that comes with it.
func TestCreateSidecarNotWritten(t *testing.T) {
	sp := openSpool(t)
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	small := limit
	small.Cur = 64
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &small); err != nil {
		t.Fatal(err)
	}
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	_, err := sp.Create(QueueDir, e, []byte("x"), Quota{})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, ErrWrite) || errors.Is(err, ErrCreate) || !errors.Is(err, syscall.EFBIG) {
		t.Errorf("Create() error = %v, want ErrWrite with EFBIG", err)
	}
	if got := names(t, sp, TmpDir); len(got) != 0 {
		t.Errorf("tmp/ = %v, want the files removed", got)
	}
}

// lockDir makes dir read-only for the test and restores it afterwards,
// so that TempDir can remove it.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o770) })
}

// TestLock pins the entry lock: exclusive between open files, which is
// how two processes see it, and followed by a fresh read of the sidecar.
func TestLock(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "x")
	first, err := sp.Lock(QueueDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("second Lock() error = %v, want ErrBusy", err)
	}
	first.Entry.MarkDone("a")
	first.Entry.Targets["b"] = &TargetState{State: Pending}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := sp.Lock(QueueDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Entry.Targets["a"].State != Done {
		t.Errorf("sidecar read after the lock = %+v, want the saved state", second.Entry.Targets)
	}
	if err := second.Remove(); err != nil {
		t.Fatal(err)
	}
	// The queue removes an entry after its last result and again when it
	// releases it.
	if err := second.Remove(); err != nil {
		t.Errorf("second Remove() error = %v, want nil", err)
	}
	_ = second.Close()
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrGone) {
		t.Errorf("Lock() after Remove error = %v, want ErrGone", err)
	}
}

// TestRemoveWithoutSidecar pins that Remove deletes the message of an
// entry whose sidecar a previous Remove deleted before its unlink of the
// message failed, rather than failing on the missing sidecar.
func TestRemoveWithoutSidecar(t *testing.T) {
	sp := openSpool(t)
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	rec, err := sp.Create(QueueDir, e, []byte("Subject: s\n\nb\n"), Quota{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	if err := os.Remove(filepath.Join(sp.dir, QueueDir, e.ID+entrySuffix)); err != nil {
		t.Fatal(err)
	}
	if err := rec.Remove(); err != nil {
		t.Errorf("Remove() error = %v, want nil", err)
	}
	if got := names(t, sp, QueueDir); len(got) != 0 {
		t.Errorf("queue/ = %v, want empty", got)
	}
}

// TestLockAfterRemovalByHolder pins that an entry opened before its
// holder removed it is recognized as gone once the lock comes free.
func TestLockAfterRemovalByHolder(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "x")
	holder, err := sp.Lock(QueueDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(filepath.Join(sp.dir, QueueDir, e.ID+".eml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := holder.Move(FailedDir); err != nil {
		t.Fatal(err)
	}
	_ = holder.Close()
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrGone) {
		t.Errorf("Lock() after Move error = %v, want ErrGone", err)
	}
	if got := names(t, sp, FailedDir); len(got) != 2 {
		t.Errorf("failed/ = %v, want the moved entry", got)
	}
}

// TestLockOrphanMessage pins that a message file whose sidecar is gone,
// the rest of a run killed between the two removals, is deleted.
func TestLockOrphanMessage(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "x")
	if err := os.Remove(filepath.Join(sp.dir, QueueDir, e.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrGone) {
		t.Errorf("Lock() error = %v, want ErrGone", err)
	}
	if got := names(t, sp, QueueDir); len(got) != 0 {
		t.Errorf("queue/ = %v, want empty", got)
	}
}

// TestLockCorrupt pins the record of an entry whose sidecar is corrupt:
// owner and time come from the message file, and Move gives it a valid
// sidecar in failed/ beside the unchanged message.
func TestLockCorrupt(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "Subject: s\n\nb\n")
	path := filepath.Join(sp.dir, QueueDir, e.ID)
	if _, err := sp.LockCorrupt(QueueDir, e.ID); !errors.Is(err, ErrGone) {
		t.Errorf("LockCorrupt() of a valid entry error = %v, want ErrGone", err)
	}
	if err := os.WriteFile(path+".json", []byte("{"), 0o660); err != nil {
		t.Fatal(err)
	}
	written := testNow.Add(-time.Hour)
	if err := os.Chtimes(path+".eml", written, written); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Lock() error = %v, want ErrCorrupt", err)
	}
	rec, err := sp.LockCorrupt(QueueDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Lock() of the held entry error = %v, want ErrBusy", err)
	}
	if got := rec.Entry; got.ID != e.ID || got.OwnerUID != os.Getuid() || !got.CreatedAt.Equal(written) || got.Targets != nil {
		t.Errorf("entry = %+v, want id, owner and time of the message file", got)
	}
	rec.Entry.Reason = "corrupt sidecar"
	if err := rec.Move(FailedDir); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()
	moved, err := sp.Lock(FailedDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = moved.Close() }()
	if raw, _ := moved.Message(); string(raw) != "Subject: s\n\nb\n" || moved.Entry.Reason != "corrupt sidecar" {
		t.Errorf("failed/ entry = %+v with message %q", moved.Entry, raw)
	}
	if got := names(t, sp, QueueDir); len(got) != 0 {
		t.Errorf("queue/ = %v, want empty", got)
	}
}

func TestLockUnknownVersion(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "x")
	path := filepath.Join(sp.dir, QueueDir, e.ID+".json")
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("Lock() error = %v, want ErrUnknownVersion", err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"version":2}` {
		t.Errorf("sidecar = %s, want it untouched", data)
	}
	if _, err := sp.Lock(QueueDir, e.ID); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("Lock() kept the entry locked: %v", err)
	}
}

func TestList(t *testing.T) {
	sp := openSpool(t)
	var want []string
	for i := range 3 {
		e := NewEntry(NewID(testNow.Add(time.Duration(i)*time.Second)), 0, testNow, testNow, message.Envelope{}, nil)
		rec, err := sp.Create(QueueDir, e, nil, Quota{})
		if err != nil {
			t.Fatal(err)
		}
		_ = rec.Close()
		want = append(want, e.ID)
	}
	if err := os.WriteFile(filepath.Join(sp.dir, QueueDir, "stray.eml"), nil, 0o660); err != nil {
		t.Fatal(err)
	}
	got, err := sp.List(QueueDir)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("List() = %v, %v, want %v", got, err, want)
	}
}

func TestLockRun(t *testing.T) {
	sp := openSpool(t)
	run, err := sp.LockRun(1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.LockRun(1000); !errors.Is(err, ErrBusy) {
		t.Errorf("second LockRun(1000) error = %v, want ErrBusy", err)
	}
	other, err := sp.LockRun(1001)
	if err != nil {
		t.Errorf("LockRun(1001) error = %v, want another user's lock to be free", err)
	} else {
		_ = other.Close()
	}
	_ = run.Close()
	again, err := sp.LockRun(1000)
	if err != nil {
		t.Errorf("LockRun after Close error = %v", err)
	} else {
		_ = again.Close()
	}
	if got := names(t, sp, LocksDir); !slices.Equal(got, []string{"drain-1000.lock", "drain-1001.lock"}) {
		t.Errorf("locks/ = %v", got)
	}
}

func TestRemoveStale(t *testing.T) {
	sp := openSpool(t)
	for name, age := range map[string]time.Duration{"old.eml": 2 * time.Hour, "new.eml": 30 * time.Minute} {
		path := filepath.Join(sp.dir, TmpDir, name)
		if err := os.WriteFile(path, nil, 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, testNow.Add(-age), testNow.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sp.RemoveStale(testNow, time.Hour)
	if err != nil || removed != 1 {
		t.Errorf("RemoveStale() = %d, %v, want 1", removed, err)
	}
	if got := names(t, sp, TmpDir); !slices.Equal(got, []string{"new.eml"}) {
		t.Errorf("tmp/ = %v, want only new.eml", got)
	}
}

func TestUsage(t *testing.T) {
	sp := openSpool(t)
	create(t, sp, QueueDir, 1000, "12345")
	create(t, sp, HoldDir, 1000, "123")
	create(t, sp, QueueDir, 0, "1")
	create(t, sp, FailedDir, 1000, "not counted")
	if err := os.WriteFile(filepath.Join(sp.dir, TmpDir, "1-0000000000000000.eml"), []byte("1234567"), 0o660); err != nil {
		t.Fatal(err)
	}
	usage, err := sp.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.Messages != 4 || usage.PerUID[1000].Messages != 2 || usage.PerUID[0].Messages != 1 {
		t.Errorf("Usage() = %+v", usage)
	}
	sidecars := usage.Bytes - 5 - 3 - 1 - 7
	if sidecars <= 0 || usage.PerUID[1000].Bytes <= 8 {
		t.Errorf("Usage().Bytes = %d, want messages and sidecars counted", usage.Bytes)
	}
}

func TestQuotaAdmit(t *testing.T) {
	limits := Limits{Messages: 10, Bytes: 1000, MessagesPerUID: 3, BytesPerUID: 300}
	usage := func(total int, bytes int64, own Count) Usage {
		return Usage{Messages: total, Bytes: bytes, PerUID: map[int]Count{1000: own}}
	}
	cases := []struct {
		name       string
		usage      Usage
		size       int64
		isReserved bool
		wantOK     bool
	}{
		{"empty", usage(0, 0, Count{}), 100, false, true},
		{"own-messages-full", usage(3, 30, Count{3, 30}), 1, false, false},
		{"own-bytes-full", usage(1, 250, Count{1, 250}), 51, false, false},
		{"own-bytes-exactly", usage(1, 250, Count{1, 250}), 50, false, true},
		{"others-take-the-share", usage(8, 80, Count{}), 1, false, false},
		{"reserve-for-root", usage(8, 80, Count{}), 1, true, true},
		{"reserve-exhausted", usage(10, 100, Count{}), 1, true, false},
		{"bytes-share", usage(1, 800, Count{}), 1, false, false},
		{"bytes-reserve", usage(1, 800, Count{}), 200, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Quota{Limits: limits, IsReserved: tc.isReserved}.Admit(tc.usage, 1000, tc.size)
			if (err == nil) != tc.wantOK {
				t.Errorf("Admit() error = %v, want ok %v", err, tc.wantOK)
			}
		})
	}
	if err := (Quota{}).Admit(usage(1<<20, 1<<40, Count{1 << 20, 1 << 40}), 1000, 1); err != nil {
		t.Errorf("zero limits Admit() error = %v, want no bound", err)
	}
}

// TestCreateQuotaParallel pins that the quota holds when calls race:
// parallel calls never take more than the limit, though near it they may
// all be refused; calls one after another then fill it exactly.
func TestCreateQuotaParallel(t *testing.T) {
	createAll := func(sp *Spool, n, uid int, quota Quota) int {
		var mu sync.Mutex
		var wg sync.WaitGroup
		created := 0
		for range n {
			wg.Go(func() {
				now := time.Now()
				e := NewEntry(NewID(now), uid, now, now, message.Envelope{}, []string{"a"})
				rec, err := sp.Create(QueueDir, e, []byte("Subject: x\n\nbody\n"), quota)
				if err != nil {
					if !errors.Is(err, ErrQuota) {
						t.Errorf("Create() error = %v, want ErrQuota", err)
					}
					return
				}
				_ = rec.Close()
				mu.Lock()
				created++
				mu.Unlock()
			})
		}
		wg.Wait()
		return created
	}
	fill := func(sp *Spool, uid int, quota Quota) int {
		created := 0
		for createAll(sp, 1, uid, quota) == 1 {
			created++
		}
		return created
	}
	t.Run("per-uid-limit", func(t *testing.T) {
		sp := openSpool(t)
		quota := Quota{Limits: Limits{MessagesPerUID: 2}}
		parallel := createAll(sp, 32, 1000, quota)
		if parallel > 2 {
			t.Fatalf("%d of 32 parallel entries created, want at most 2", parallel)
		}
		if n := parallel + fill(sp, 1000, quota); n != 2 {
			t.Errorf("%d entries in the end, want 2", n)
		}
	})
	t.Run("reserve-for-root", func(t *testing.T) {
		sp := openSpool(t)
		limits := Limits{Messages: 10, MessagesPerUID: 2}
		for uid := 1000; uid < 1004; uid++ {
			if n := fill(sp, uid, Quota{Limits: limits}); n != 2 {
				t.Fatalf("uid %d: %d entries created, want 2", uid, n)
			}
		}
		if n := createAll(sp, 8, 1004, Quota{Limits: limits}); n != 0 {
			t.Errorf("another user got %d entries past 80%% of the spool", n)
		}
		root := Quota{Limits: limits, IsReserved: true}
		parallel := createAll(sp, 8, 0, root)
		if parallel > 2 {
			t.Fatalf("root got %d of 8 parallel entries, want at most the 2 of the reserve", parallel)
		}
		if n := parallel + fill(sp, 0, root); n != 2 {
			t.Errorf("root got %d entries in the end, want the 2 of the reserve", n)
		}
	})
}

// TestCreateNotHeldUp pins that no lock of another process delays
// Create: a stopped call must not stop the mail of the host. Another
// open file holds the lock of a reserved entry in tmp/, a run lock and
// a lock on locks/quota.lock, a name a shared quota lock would take.
func TestCreateNotHeldUp(t *testing.T) {
	sp := openSpool(t)
	for _, path := range []string{
		filepath.Join(sp.dir, TmpDir, "1-00000000000000aa.eml"),
		filepath.Join(sp.dir, LocksDir, "drain-1000.lock"),
		filepath.Join(sp.dir, LocksDir, "quota.lock"),
	} {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o660)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		e := NewEntry(NewID(testNow), 0, testNow, testNow, message.Envelope{}, []string{"a"})
		rec, err := sp.Create(QueueDir, e, []byte("x"), Quota{Limits: Limits{Messages: 10}, IsReserved: true})
		if err == nil {
			_ = rec.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Create() error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Create waits for a lock of another process")
	}
}

// TestSaveOverRemnant pins that a sidecar rewrite left in tmp/ by a
// killed holder does not block the next holder.
func TestSaveOverRemnant(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, QueueDir, 1000, "x")
	if err := os.WriteFile(filepath.Join(sp.dir, TmpDir, e.ID+".next.json"), []byte("{"), 0o660); err != nil {
		t.Fatal(err)
	}
	rec, err := sp.Lock(QueueDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	rec.Entry.MarkDone("a")
	if err := rec.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got, _ := sp.Peek(QueueDir, e.ID); got.Targets["a"].State != Done {
		t.Errorf("saved state = %+v", got.Targets["a"])
	}
}

// TestMove pins the files after a Move: the entry complete in the new
// area, nothing left in the old one.
func TestMove(t *testing.T) {
	sp := openSpool(t)
	e := create(t, sp, HoldDir, 1000, "x")
	rec, err := sp.Lock(HoldDir, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	rec.Entry.Reason = ""
	if err := rec.Move(QueueDir); err != nil {
		t.Fatal(err)
	}
	if got := names(t, sp, QueueDir); !slices.Equal(got, []string{e.ID + ".eml", e.ID + ".json"}) {
		t.Errorf("queue/ = %v", got)
	}
	if got := names(t, sp, HoldDir); len(got) != 0 {
		t.Errorf("hold/ = %v, want empty", got)
	}
	if got := names(t, sp, TmpDir); len(got) != 0 {
		t.Errorf("tmp/ = %v, want empty", got)
	}
}

// TestRemoveStaleOrphans covers the rest of crashed processes outside
// tmp/, and files in tmp/ that a live process still holds.
func TestRemoveStaleOrphans(t *testing.T) {
	sp := openSpool(t)
	old := testNow.Add(-2 * time.Hour)
	write := func(area, name string, mtime time.Time) string {
		path := filepath.Join(sp.dir, area, name)
		if err := os.WriteFile(path, []byte("{}"), 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return path
	}
	kept := create(t, sp, QueueDir, 1000, "x")
	for _, area := range []string{QueueDir, HoldDir, FailedDir} {
		write(area, "1-00000000000000aa.json", old)
		write(area, "1-00000000000000bb.json", testNow)
	}
	queueSidecar := filepath.Join(sp.dir, QueueDir, kept.ID+".json")
	if err := os.Chtimes(queueSidecar, old, old); err != nil {
		t.Fatal(err)
	}
	held := write(TmpDir, "1-00000000000000cc.eml", old)
	write(TmpDir, "1-00000000000000cc.json", old)
	write(TmpDir, "1-00000000000000dd.next.json", old)
	file, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	removed, err := sp.RemoveStale(testNow, time.Hour)
	if err != nil || removed != 4 {
		t.Errorf("RemoveStale() = %d, %v, want 4", removed, err)
	}
	for _, area := range []string{HoldDir, FailedDir} {
		if got := names(t, sp, area); !slices.Equal(got, []string{"1-00000000000000bb.json"}) {
			t.Errorf("%s/ = %v, want only the fresh sidecar", area, got)
		}
	}
	if got := names(t, sp, QueueDir); !slices.Equal(got, []string{"1-00000000000000bb.json", kept.ID + ".eml", kept.ID + ".json"}) {
		t.Errorf("queue/ = %v, want the fresh sidecar and the complete entry", got)
	}
	if got := names(t, sp, TmpDir); !slices.Equal(got, []string{"1-00000000000000cc.eml", "1-00000000000000cc.json"}) {
		t.Errorf("tmp/ = %v, want the locked message and its sidecar", got)
	}
}

// TestOpenExisting pins that the modes that only read the spool create
// nothing and read missing areas as empty.
func TestOpenExisting(t *testing.T) {
	dir := t.TempDir()
	sp, err := OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := sp.List(QueueDir); err != nil || len(ids) != 0 {
		t.Errorf("List() = %v, %v, want empty", ids, err)
	}
	if usage, err := sp.Usage(); err != nil || usage.Messages != 0 {
		t.Errorf("Usage() = %+v, %v", usage, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("OpenExisting created %v", entries)
	}
	if _, err := OpenExisting(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenExisting(missing) error = %v, want fs.ErrNotExist", err)
	}
}

// TestLockNoFollow pins that a symbolic link in place of an entry or run
// lock is not followed.
func TestLockNoFollow(t *testing.T) {
	sp := openSpool(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "1-00000000000000ee"
	for _, link := range []string{filepath.Join(sp.dir, QueueDir, id+".eml"), filepath.Join(sp.dir, LocksDir, "drain-1000.lock")} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sp.dir, QueueDir, id+".json"), []byte(`{"version":1,"id":"`+id+`"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if rec, err := sp.Lock(QueueDir, id); err == nil {
		_ = rec.Close()
		t.Error("Lock() followed a symbolic link")
	}
	if run, err := sp.LockRun(1000); err == nil {
		_ = run.Close()
		t.Error("LockRun() followed a symbolic link")
	}
}

// fillQueue writes 1000 entries of five owners into queue/, unsynced.
func fillQueue(b *testing.B) *Spool {
	b.Helper()
	sp, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := range 1000 {
		e := NewEntry(NewID(testNow.Add(time.Duration(i))), 1000+i%5, testNow, testNow, message.Envelope{Recipients: []string{"root@example.org"}}, []string{"a", "b"})
		data, err := Encode(e)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sp.dir, QueueDir, e.ID+".eml"), benchMessage, 0o660); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sp.dir, QueueDir, e.ID+".json"), data, 0o660); err != nil {
			b.Fatal(err)
		}
	}
	return sp
}

var benchMessage = []byte("Subject: disk full\n\n/dev/sda1 99%\n")

// BenchmarkCreateFullQueue measures one Create, usage scan and fsyncs
// included, with 1000 entries in queue/ and limits that admit it:
// go test -run '^$' -bench FullQueue ./internal/spool
func BenchmarkCreateFullQueue(b *testing.B) {
	sp := fillQueue(b)
	quota := Quota{Limits: Limits{Messages: 2000, Bytes: 256 << 20, MessagesPerUID: 2000, BytesPerUID: 64 << 20}}
	for b.Loop() {
		e := NewEntry(NewID(time.Now()), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
		rec, err := sp.Create(QueueDir, e, benchMessage, quota)
		if err != nil {
			b.Fatal(err)
		}
		if err := rec.Remove(); err != nil {
			b.Fatal(err)
		}
		_ = rec.Close()
	}
}

// BenchmarkUsageFullQueue measures the usage scan alone, which every
// Create runs once.
func BenchmarkUsageFullQueue(b *testing.B) {
	sp := fillQueue(b)
	for b.Loop() {
		if _, err := sp.Usage(); err != nil {
			b.Fatal(err)
		}
	}
}

// TestUsageSeesMovingEntries pins the order in which Usage reads the
// areas: an entry moved from hold/ to queue/ during the scan is still
// counted, so a quota check next to a queue run releasing held messages
// does not admit an entry over the limit.
func TestUsageSeesMovingEntries(t *testing.T) {
	for range 2 {
		sp := openSpool(t)
		const n = 8
		var ids []string
		for i := range n {
			e := NewEntry(NewID(testNow.Add(time.Duration(i))), 1000, testNow, testNow, message.Envelope{}, nil)
			data, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			// Written without Create: its fsyncs would make the test slow.
			for name, content := range map[string][]byte{e.ID + ".json": data, e.ID + ".eml": []byte("x")} {
				if err := os.WriteFile(filepath.Join(sp.dir, HoldDir, name), content, 0o660); err != nil {
					t.Fatal(err)
				}
			}
			ids = append(ids, e.ID)
		}
		stop := make(chan struct{})
		var least atomic.Int64
		least.Store(n)
		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				usage, err := sp.Usage()
				if err != nil {
					t.Error(err)
					return
				}
				if int64(usage.Messages) < least.Load() {
					least.Store(int64(usage.Messages))
				}
			}
		})
		for _, id := range ids {
			rec, err := sp.Lock(HoldDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := rec.Move(QueueDir); err != nil {
				t.Fatal(err)
			}
			_ = rec.Close()
		}
		close(stop)
		wg.Wait()
		if got := least.Load(); got < n {
			t.Fatalf("Usage saw %d of %d entries while they moved from hold/ to queue/", got, n)
		}
	}
}

// TestCreateTotalLimitManyUIDs pins the total limit against parallel
// calls of many users and root: never more entries than the limit, never
// more than the 80% share for the users.
func TestCreateTotalLimitManyUIDs(t *testing.T) {
	for round := range 4 {
		sp := openSpool(t)
		limits := Limits{Messages: 10}
		var wg sync.WaitGroup
		for i := range 40 {
			wg.Go(func() {
				now := time.Now()
				uid, quota := 1000+i, Quota{Limits: limits}
				if i%4 == 0 {
					uid, quota = 0, Quota{Limits: limits, IsReserved: true}
				}
				e := NewEntry(NewID(now), uid, now, now, message.Envelope{}, []string{"a"})
				rec, err := sp.Create(QueueDir, e, []byte("Subject: x\n\nbody\n"), quota)
				if err != nil {
					if !errors.Is(err, ErrQuota) {
						t.Errorf("Create() error = %v, want ErrQuota", err)
					}
					return
				}
				_ = rec.Close()
			})
		}
		wg.Wait()
		usage, err := sp.Usage()
		if err != nil {
			t.Fatal(err)
		}
		users := usage.Messages - usage.PerUID[0].Messages
		if usage.Messages > 10 || users > 8 {
			t.Fatalf("round %d: %d entries, %d of users, want at most 10 and 8", round, usage.Messages, users)
		}
	}
}

// TestCreateStepsKeepOwner pins that while Create moves an entry into its
// area, its message file always has the sidecar beside it, so that Usage
// counts the entry for its owner at every step and a per-uid limit holds.
func TestCreateStepsKeepOwner(t *testing.T) {
	sp := openSpool(t)
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	var steps []string
	afterCreateStep = func(step string) {
		steps = append(steps, step)
		for _, area := range []string{TmpDir, QueueDir} {
			if sp.exists(area, e.ID+".eml") && !sp.exists(area, e.ID+".json") {
				t.Errorf("after step %s: %s/ holds the message without its sidecar", step, area)
			}
		}
		usage, err := sp.Usage()
		if err != nil {
			t.Fatal(err)
		}
		if usage.Messages != 1 || usage.PerUID[1000].Messages != 1 {
			t.Errorf("after step %s: Usage = %d messages, %d of uid 1000, want 1 and 1", step, usage.Messages, usage.PerUID[1000].Messages)
		}
	}
	t.Cleanup(func() { afterCreateStep = nil })
	rec, err := sp.Create(QueueDir, e, []byte("x"), Quota{})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()
	if !slices.Equal(steps, []string{"sidecar", "message"}) {
		t.Errorf("steps = %v", steps)
	}
	if got := names(t, sp, TmpDir); len(got) != 0 {
		t.Errorf("tmp/ = %v, want empty", got)
	}
}

// TestCreateFailureRemovesMessage pins that a Create failing after the
// message is renamed into its area removes it there, so that no queue run
// delivers an entry whose caller saw it fail and only Remove leaves a
// message file without its sidecar.
func TestCreateFailureRemovesMessage(t *testing.T) {
	sp := openSpool(t)
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	sync := syncDir
	syncDir = func(string) error { return errors.New("sync failed") }
	t.Cleanup(func() { syncDir = sync })
	if _, err := sp.Create(QueueDir, e, []byte("x"), Quota{}); !errors.Is(err, ErrWrite) {
		t.Fatalf("Create() error = %v, want ErrWrite", err)
	}
	for _, area := range []string{TmpDir, QueueDir} {
		if got := names(t, sp, area); len(got) != 0 {
			t.Errorf("%s/ = %v, want empty", area, got)
		}
	}
}

// TestCreateReplacesOrphanSidecar pins that a sidecar a crash left in the
// area without its message does not keep a new entry of the same id out.
func TestCreateReplacesOrphanSidecar(t *testing.T) {
	sp := openSpool(t)
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a"})
	if err := os.WriteFile(filepath.Join(sp.dir, QueueDir, e.ID+".json"), []byte("{}"), 0o660); err != nil {
		t.Fatal(err)
	}
	rec, err := sp.Create(QueueDir, e, []byte("x"), Quota{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_ = rec.Close()
	if got, err := sp.Peek(QueueDir, e.ID); err != nil || got.OwnerUID != 1000 {
		t.Errorf("Peek() = %+v, %v, want the new sidecar", got, err)
	}
}
