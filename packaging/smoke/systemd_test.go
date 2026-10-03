//go:build smoke

package smoke

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const timer = "mailcrier-queue.timer"

// TestSmokeSystemd runs in a container whose PID 1 is systemd: the install
// enables and starts the queue timer, the cron line of the package stands
// back, the queue service delivers as mailcrier under its sandbox options,
// a reinstall keeps the timer running and the removal stops and disables
// it. It does not wait for the timer to fire: that is the work of systemd.
func TestSmokeSystemd(t *testing.T) {
	d := currentDistro(t)
	waitSystemd(t)
	recv := newReceiver(t)
	queuer := credential(t, "queuer")
	// The service has a private /tmp, so the configuration names no file
	// of the test; its messages go to the receiver alone.
	working := workingConfig(recv.URL, "/bin/true")
	report := func() string { return recv.report() + "\nspool:\n" + listSpool(t) }
	systemctl := func(t *testing.T, args ...string) string {
		t.Helper()
		return strings.TrimSpace(run(t, nil, "", append([]string{"systemctl"}, args...)...).out)
	}

	if !t.Run("T-PKG-08/timer-started-on-install", func(t *testing.T) {
		install(t, d)
		if got := systemctl(t, "is-enabled", timer); got != "enabled" {
			t.Errorf("is-enabled %s = %q, want enabled", timer, got)
		}
		if got := systemctl(t, "is-active", timer); got != "active" {
			t.Errorf("is-active %s = %q, want active", timer, got)
		}
		if got := systemctl(t, "show", "-p", "NextElapseUSecMonotonic", "--value", timer); got == "" {
			t.Errorf("%s has no next elapse", timer)
		}
	}) {
		t.FailNow()
	}

	// The timer stops first: its first run 2 minutes after boot could
	// release the held message before the cron line is checked.
	const marker = "smoke-systemd-held-6c3d"
	held := recv.expect(marker)
	if !t.Run("T-PKG-08/cron-line-stands-back", func(t *testing.T) {
		mustRun(t, "", "systemctl", "stop", timer)
		writeConfig(t, heldConfig)
		r := run(t, queuer, "Subject: "+marker+"\n\nbody\n", "/usr/sbin/sendmail", "-i", "ops@example.org")
		if r.code != 78 {
			t.Fatalf("call without targets exited %d, want 78:\n%s", r.code, r.out)
		}
		writeConfig(t, working)
		command := cronCommand(t, d.cronFile)
		if r := run(t, nil, "", "runuser", "-u", "mailcrier", "--", "sh", "-c", command); r.code != 0 || r.out != "" {
			t.Errorf("cron job %q under systemd exited %d:\n%s", command, r.code, r.out)
		}
		if got := spoolMessages(t, "hold"); len(got) != 1 {
			t.Fatalf("hold/ = %v after the cron line, want the message untouched\n%s", got, report())
		}
	}) {
		t.FailNow()
	}

	t.Run("T-PKG-08/service-delivers", func(t *testing.T) {
		// The unit as systemd loaded it; the run itself does not report the
		// user or the sandbox it had.
		for property, want := range map[string]string{
			"User": "mailcrier", "Group": "mailcrier", "NoNewPrivileges": "yes",
			"ProtectSystem": "full", "ProtectHome": "yes", "PrivateTmp": "yes",
		} {
			if got := systemctl(t, "show", "-p", property, "--value", "mailcrier-queue.service"); got != want {
				t.Errorf("mailcrier-queue.service %s=%q, want %q", property, got, want)
			}
		}
		if r := run(t, nil, "", "systemctl", "start", "mailcrier-queue.service"); r.code != 0 {
			t.Errorf("systemctl start mailcrier-queue.service exited %d:\n%s\n%s", r.code, r.out,
				run(t, nil, "", "journalctl", "-u", "mailcrier-queue.service", "--no-pager").out)
		}
		if got := systemctl(t, "show", "-p", "Result", "--value", "mailcrier-queue.service"); got != "success" {
			t.Errorf("mailcrier-queue.service result %q, want success", got)
		}
		waitEvent(t, held, time.Now().Add(10*time.Second), "the held message", report)
		if got := allSpoolMessages(t); len(got) != 0 {
			t.Errorf("spool holds %v after the service ran", got)
		}
		mustRun(t, "", "systemctl", "start", timer)
		if got := systemctl(t, "is-active", timer); got != "active" {
			t.Errorf("is-active %s = %q after start, want active", timer, got)
		}
	})

	t.Run("T-PKG-08/reinstall-keeps-timer", func(t *testing.T) {
		out := reinstall(t, d)
		t.Logf("reinstall:\n%s", out)
		checkInstallerOutput(t, out)
		if got := systemctl(t, "is-active", timer); got != "active" {
			t.Errorf("is-active %s = %q after the reinstall, want active", timer, got)
		}
	})

	t.Run("T-PKG-08/removal-stops-timer", func(t *testing.T) {
		out := mustRun(t, "", d.remove...)
		t.Logf("remove:\n%s", out)
		if got := systemctl(t, "is-active", timer); got != "inactive" {
			t.Errorf("is-active %s = %q after the removal, want inactive", timer, got)
		}
		if got := systemctl(t, "is-enabled", timer); got == "enabled" {
			t.Errorf("%s stays enabled after the removal", timer)
		}
		if d.format != "deb" {
			return
		}
		mustRun(t, "", "dpkg", "-P", "mailcrier")
		if _, err := os.Lstat("/etc/systemd/system/timers.target.wants/" + timer); err == nil {
			t.Errorf("the link of %s stays after dpkg -P", timer)
		}
		state, _ := filepath.Glob("/var/lib/systemd/deb-systemd-helper-enabled/*" + timer + "*")
		nested, _ := filepath.Glob("/var/lib/systemd/deb-systemd-helper-enabled/*/" + timer)
		if found := append(state, nested...); len(found) != 0 {
			t.Errorf("deb-systemd-helper keeps %v after dpkg -P", found)
		}
	})
}

// cronCommand returns the command of the line of a cron.d file, after the
// five time fields and the user.
func cronCommand(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(data)) {
		if fields := strings.Fields(line); len(fields) > 6 && strings.HasPrefix(line, "*/5 ") {
			return strings.Join(fields[6:], " ")
		}
	}
	t.Fatalf("%s has no */5 line", path)
	return ""
}

// waitSystemd waits for the private socket of systemd, which systemctl
// needs and which does not exist yet right after the container starts, and
// then for the end of the boot; running and degraded both count, a
// container lacks some devices.
func waitSystemd(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	waitCreated(t, "/run", "systemd", deadline)
	waitCreated(t, "/run/systemd", "private", deadline)
	out := run(t, nil, "", "systemctl", "is-system-running", "--wait").out
	if state := strings.TrimSpace(out); state != "running" && state != "degraded" {
		t.Fatalf("systemd is %q, want running or degraded\n%s", state, run(t, nil, "", "systemctl", "--failed", "--no-pager").out)
	}
}

// waitCreated returns once dir holds name, watching dir with inotify.
func waitCreated(t *testing.T, dir, name string, deadline time.Time) {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	// A non-blocking descriptor goes to the poller, so the read deadline
	// applies.
	watcher := os.NewFile(uintptr(fd), "inotify")
	defer func() { _ = watcher.Close() }()
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_CREATE|syscall.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
		return
	}
	if err := watcher.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := watcher.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s did not appear in %s by %s", name, dir, deadline.Format(time.TimeOnly))
		}
		if err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset+syscall.SizeofInotifyEvent <= n; {
			event := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameBytes := buf[offset+syscall.SizeofInotifyEvent : offset+syscall.SizeofInotifyEvent+int(event.Len)]
			if strings.TrimRight(string(nameBytes), "\x00") == name {
				return
			}
			offset += syscall.SizeofInotifyEvent + int(event.Len)
		}
	}
}
