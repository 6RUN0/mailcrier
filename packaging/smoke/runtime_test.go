//go:build smoke

package smoke

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSmokeRuntime installs the package while cron and atd run and sends
// through it the way the jobs of a host do: a message of another user that
// only the queue run of the package can deliver, the command line of
// cronie as root, the crontab of a user with job output, an attachment and
// a hook, an at job, and a call under no_new_privs. Each step waits for
// the request of its own marker at the receiver, never for a time.
func TestSmokeRuntime(t *testing.T) {
	d := currentDistro(t)
	recv := newReceiver(t)
	dir := fixtures(t)
	alice, queuer := credential(t, "alice"), credential(t, "queuer")
	cronLog := startDaemon(t, d.cron...)
	atdLog := startDaemon(t, "atd", "-f")
	report := func() string {
		return fmt.Sprintf("%s\ncron:\n%s\natd:\n%s\nspool:\n%s", recv.report(), cronLog, atdLog, listSpool(t))
	}

	if !t.Run("T-PKG-01/install-and-modes", func(t *testing.T) {
		if d.mta == "" {
			if _, err := os.Lstat("/usr/sbin/sendmail"); err == nil {
				t.Fatal("image contains an MTA")
			}
		} else if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != d.mta {
			t.Fatalf("/usr/sbin/sendmail before the install = %q, %v, want %s", target, err, d.mta)
		}
		if _, err := os.Stat("/run/systemd/system"); err == nil {
			t.Fatal("/run/systemd/system exists: the cron line of the package would leave the queue to a timer that does not run")
		}
		install(t, d)
		gid := serviceGID(t)
		for _, link := range mtaLinks {
			if target, err := filepath.EvalSymlinks(link); err != nil || target != binary {
				t.Errorf("%s = %q, %v, want %s", link, target, err, binary)
			}
		}
		checkMode(t, config, 0, gid, 0o640)
		checkMode(t, "/etc/slendmail.d", 0, gid, os.ModeDir|0o750)
		checkMode(t, spoolDir, 0, gid, os.ModeDir|0o750)
		for _, area := range spoolAreas {
			checkMode(t, filepath.Join(spoolDir, area), 0, gid, os.ModeDir|os.ModeSetgid|0o770)
		}
		if _, err := os.Stat(d.cronFile); err != nil {
			t.Error(err)
		}
		if d.format == "rpm" {
			got := run(t, nil, "", "rpm", "-V", "slendmail").out
			if fields := strings.Fields(got); len(fields) != 2 || fields[0] != ".M....G.." || fields[1] != binary {
				t.Errorf("rpm -V slendmail =\n%s\nwant .M....G.. %s alone", got, binary)
			}
		}
		for _, link := range []string{"/usr/bin/newaliases", "/usr/bin/mailq"} {
			if r := run(t, nil, "", link); r.code != 0 {
				t.Errorf("%s exited %d:\n%s", link, r.code, r.out)
			}
		}
	}) {
		t.FailNow()
	}
	writeConfig(t, workingConfig(recv.URL, filepath.Join(dir, "hook")))

	// Only a queue run as root or slendmail delivers the entry of queuer;
	// nothing in this test runs one but the cron job of the package. The
	// receiver cannot tell which run delivered it, so the step relies on
	// that: a call that took the entries of other users would pass too.
	const queuedMarker = "smoke-queued-3b9e"
	recv.failOnce(queuedMarker)
	queued := recv.expect(queuedMarker)
	queuedAt := time.Now()
	if !t.Run("T-PKG-04/queued-on-failure", func(t *testing.T) {
		r := run(t, queuer, "Subject: "+queuedMarker+"\n\nbody\n", "/usr/sbin/sendmail", "-i", "ops@example.org")
		if r.code != 0 {
			t.Fatalf("call with the receiver failing exited %d, want 0 for a queued message:\n%s", r.code, r.out)
		}
		if got := spoolMessages(t, "queue"); len(got) != 1 {
			t.Fatalf("queue/ = %v, want one message\n%s", got, report())
		}
	}) {
		t.FailNow()
	}

	t.Run("T-PKG-09/cronie-argv-as-root", func(t *testing.T) {
		argvFile, err := os.ReadFile("/callers/cronie.argv")
		if err != nil {
			t.Fatal(err)
		}
		message, err := os.ReadFile("/callers/cronie.eml")
		if err != nil {
			t.Fatal(err)
		}
		const marker = "backup: 3 files copied to /srv/backup"
		got := recv.expect(marker)
		argv := strings.Split(strings.TrimSpace(string(argvFile)), "\n")
		if r := run(t, nil, string(message), argv...); r.code != 0 {
			t.Fatalf("%v exited %d:\n%s", argv, r.code, r.out)
		}
		waitEvent(t, got, time.Now().Add(10*time.Second), "the cronie message", report)
	})

	// The crontab of alice: the output of a job, mailed by cron itself with
	// its own command line; a message with an attachment; a message to the
	// hook. No line names the address of the hook, so the mail of cron,
	// whose subject repeats the line, stays out of its route. BusyBox crond
	// mails output only with MAILTO.
	const cronMarker = "smoke-cron-output-5a2c"
	cronOutput, attachment := recv.expect(cronMarker), recv.expect(attachmentMarker)
	hookLines := openFIFO(t, filepath.Join(dir, "hook.fifo"))
	crontab := fmt.Sprintf("MAILTO=alice\n* * * * * echo %s\n* * * * * /usr/sbin/sendmail -t < %s\n* * * * * /usr/sbin/sendmail -t < %s\n",
		cronMarker, filepath.Join(dir, "attachment.eml"), filepath.Join(dir, "hook.eml"))
	removeCrontab := func() { run(t, nil, "", "crontab", "-r", "-u", "alice") }
	t.Cleanup(removeCrontab)
	mustRun(t, crontab, "crontab", "-u", "alice", "-")
	cronDeadline := time.Now().Add(150 * time.Second)

	t.Run("T-PKG-02/user-crontab-with-attachment", func(t *testing.T) {
		if ev := waitEvent(t, cronOutput, cronDeadline, "the output of the cron job of alice", report); ev.method != "POST" {
			t.Errorf("cron output arrived as %+v", ev)
		}
		seen := map[string]bool{}
		for !seen["POST"] || !seen["PUT"] {
			ev := waitEvent(t, attachment, cronDeadline, "the message with an attachment and its file", report)
			seen[ev.method] = true
			if ev.method == "PUT" && ev.filename != attachmentName {
				t.Errorf("attachment arrived as %q, want %q", ev.filename, attachmentName)
			}
		}
	})

	t.Run("T-PKG-03/hook-without-group", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), cronDeadline)
		defer cancel()
		var line string
		select {
		case line = <-hookLines:
		case <-ctx.Done():
			t.Fatalf("the hook did not run by %s\n%s", cronDeadline.Format(time.TimeOnly), report())
		}
		groups, group, _ := strings.Cut(line, " / ")
		if slices.Contains(strings.Fields(groups), strconv.Itoa(serviceGID(t))) {
			t.Errorf("the hook runs with the group slendmail: groups %s", groups)
		}
		if group != strconv.Itoa(int(alice.Gid)) {
			t.Errorf("the hook runs with group %s, want %d of alice", group, alice.Gid)
		}
	})
	removeCrontab()

	t.Run("T-PKG-10/at-job-output", func(t *testing.T) {
		const marker = "smoke-at-output-8f17"
		got := recv.expect(marker)
		if r := run(t, alice, "echo "+marker+"\n", "at", "now"); r.code != 0 {
			t.Fatalf("at now exited %d:\n%s", r.code, r.out)
		}
		waitEvent(t, got, time.Now().Add(60*time.Second), "the output of the at job", report)
	})

	t.Run("T-PKG-11/no-new-privs", func(t *testing.T) {
		const marker = "smoke-nnp-61d0"
		got := recv.expect(marker)
		r := run(t, alice, "Subject: "+marker+"\n\nbody\n", "setpriv", "--no-new-privs", "/usr/sbin/sendmail", "-i", "ops@example.org")
		if r.code != 78 {
			t.Errorf("call under no_new_privs exited %d, want 78:\n%s", r.code, r.out)
		}
		if !isEmpty(got) {
			t.Error("the message arrived without the setgid bit taking effect")
		}
		// Cron mail of the earlier jobs may still pass through the spool, so
		// the check looks for this message only.
		for _, area := range spoolAreas {
			files, _ := filepath.Glob(filepath.Join(spoolDir, area, "*"))
			for _, file := range files {
				if data, err := os.ReadFile(file); err == nil && strings.Contains(string(data), marker) {
					t.Errorf("%s holds the message\n%s", file, report())
				}
			}
		}
	})

	// The first retry is due 60 seconds after the call, the cron job of the
	// package runs every 5 minutes.
	t.Run("T-PKG-04/cron-runs-queue", func(t *testing.T) {
		waitEvent(t, queued, queuedAt.Add(7*time.Minute), "the queued message", report)
	})
}
