//go:build smoke

package smoke

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSmokeLifecycle reinstalls and removes the package with and without
// messages in the spool. No cron or atd runs, so only the test changes the
// spool.
func TestSmokeLifecycle(t *testing.T) {
	d := currentDistro(t)
	recv := newReceiver(t)
	dir := fixtures(t)
	alice, queuer := credential(t, "alice"), credential(t, "queuer")
	working := workingConfig(recv.URL, filepath.Join(dir, "hook"))
	report := func() string { return recv.report() + "\nspool:\n" + listSpool(t) }
	if !t.Run("T-PKG-01/install", func(t *testing.T) { install(t, d) }) {
		t.FailNow()
	}

	// send runs a call of cred with a message whose subject is marker and
	// checks its exit status.
	send := func(t *testing.T, cred *syscall.Credential, marker string, want int) {
		t.Helper()
		r := run(t, cred, "Subject: "+marker+"\n\nbody\n", "/usr/sbin/sendmail", "-i", "ops@example.org")
		if r.code != want {
			t.Fatalf("call with %s exited %d, want %d:\n%s", marker, r.code, want, r.out)
		}
	}
	// queueRun runs -q as mailcrier.
	queueRun := func(t *testing.T) {
		t.Helper()
		if r := run(t, credential(t, "mailcrier"), "", binary, "-q"); r.code != 0 {
			t.Fatalf("-q as mailcrier exited %d:\n%s\n%s", r.code, r.out, report())
		}
	}

	if !t.Run("T-PKG-05/reinstall-with-mail", func(t *testing.T) {
		const queuedMarker, heldMarker = "smoke-reinstall-queued-0c5a", "smoke-reinstall-held-91e3"
		recv.failOnce(queuedMarker)
		queued, held := recv.expect(queuedMarker), recv.expect(heldMarker)
		writeConfig(t, working)
		send(t, queuer, queuedMarker, 0)
		writeConfig(t, heldConfig)
		send(t, alice, heldMarker, 78)
		if len(spoolMessages(t, "queue")) != 1 || len(spoolMessages(t, "hold")) != 1 {
			t.Fatalf("want one message in queue/ and one in hold/\n%s", report())
		}
		before, err := os.Stat(config)
		if err != nil {
			t.Fatal(err)
		}

		out := reinstall(t, d)
		t.Logf("reinstall:\n%s", out)
		checkInstallerOutput(t, out)
		checkMode(t, binary, 0, serviceGID(t), 0o755|os.ModeSetgid)
		after, err := os.Stat(config)
		if content, _ := os.ReadFile(config); err != nil || string(content) != heldConfig || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("configuration changed by the reinstall: %q %v %v, %v", content, after.Mode(), after.ModTime(), err)
		}
		if d.format == "deb" {
			if overrides := mustRun(t, "", "dpkg-statoverride", "--list", binary); overrides != "root mailcrier 2755 "+binary+"\n" {
				t.Errorf("dpkg-statoverride --list %s =\n%s\nwant one entry root mailcrier 2755", binary, overrides)
			}
		}
		if len(spoolMessages(t, "queue")) != 1 || len(spoolMessages(t, "hold")) != 1 {
			t.Fatalf("the reinstall changed the spool\n%s", report())
		}

		writeConfig(t, working)
		makeDue(t)
		queueRun(t)
		waitEvent(t, queued, time.Now().Add(10*time.Second), "the queued message", report)
		waitEvent(t, held, time.Now().Add(10*time.Second), "the held message", report)
		if got := allSpoolMessages(t); len(got) != 0 {
			t.Errorf("spool holds %v after the queue run", got)
		}
	}) {
		t.FailNow()
	}

	if d.format == "rpm" {
		t.Run("T-PKG-12/rpm-setperms", func(t *testing.T) {
			const lostMarker, sentMarker = "smoke-setperms-lost-27b8", "smoke-setperms-sent-d40f"
			lost, sent := recv.expect(lostMarker), recv.expect(sentMarker)
			mustRun(t, "", "rpm", "--setperms", "mailcrier")
			send(t, alice, lostMarker, 78)
			if !isEmpty(lost) || len(allSpoolMessages(t)) != 0 {
				t.Errorf("a call without the setgid bit was delivered or spooled\n%s", report())
			}
			mustRun(t, "", "chgrp", "mailcrier", binary)
			mustRun(t, "", "chmod", "2755", binary)
			send(t, alice, sentMarker, 0)
			waitEvent(t, sent, time.Now().Add(10*time.Second), "the message after chmod 2755", report)
		})
	}

	// A message of queuer held under a configuration without targets, then
	// a delivered call of alice, which leaves her queue run lock.
	const removedMarker, aliceMarker = "smoke-removed-held-5e62", "smoke-removed-alice-b81c"
	removedHeld := recv.expect(removedMarker)
	if !t.Run("T-PKG-06/remove-with-mail", func(t *testing.T) {
		alicePresent := recv.expect(aliceMarker)
		writeConfig(t, heldConfig)
		send(t, queuer, removedMarker, 78)
		writeConfig(t, working)
		send(t, alice, aliceMarker, 0)
		waitEvent(t, alicePresent, time.Now().Add(10*time.Second), "the message of alice", report)
		locks, _ := filepath.Glob(filepath.Join(spoolDir, "locks", "*.lock"))
		if len(locks) == 0 || len(spoolMessages(t, "hold")) != 1 {
			t.Fatalf("want a held message and queue run locks\n%s", report())
		}

		out := mustRun(t, "", d.remove...)
		t.Logf("remove:\n%s", out)
		if !strings.Contains(out, spoolNote) {
			t.Errorf("removal did not name the spool:\n%s", out)
		}
		if got := spoolMessages(t, "hold"); len(got) != 1 {
			t.Errorf("hold/ = %v after the removal, want the message", got)
		}
		if got, _ := filepath.Glob(filepath.Join(spoolDir, "locks", "*.lock")); !slices.Equal(got, locks) {
			t.Errorf("locks/ = %v after the removal, want %v", got, locks)
		}
		for _, database := range []string{"passwd", "group"} {
			if r := run(t, nil, "", "getent", database, "mailcrier"); r.code != 0 {
				t.Errorf("mailcrier is gone from %s after the removal", database)
			}
		}
		switch d.format {
		case "deb":
			for _, link := range mtaLinks {
				if _, err := os.Lstat(link); err == nil {
					t.Errorf("%s stays after dpkg -r", link)
				}
			}
			if content, err := os.ReadFile(config); err != nil || string(content) != working {
				t.Errorf("configuration after dpkg -r: %q, %v", content, err)
			}
		case "rpm":
			if content, err := os.ReadFile(config + ".rpmsave"); err != nil || string(content) != working {
				t.Errorf("configuration after rpm -e: %q, %v, want it in .rpmsave", content, err)
			}
		case "apk":
			if content, err := os.ReadFile(config); err != nil || string(content) != working {
				t.Errorf("configuration after apk del: %q, %v", content, err)
			}
		}
		if d.mta != "" {
			if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != d.mta {
				t.Errorf("/usr/sbin/sendmail after the removal = %q, %v, want %s back", target, err, d.mta)
			}
		}
	}) {
		t.FailNow()
	}
	t.Run("T-PKG-14/cron-job-after-removal", func(t *testing.T) {
		switch d.format {
		case "deb":
			command := cronCommand(t, d.cronFile)
			if r := run(t, nil, "", "runuser", "-u", "mailcrier", "--", "sh", "-c", command); r.code != 0 || r.out != "" {
				t.Errorf("cron job %q without the binary exited %d:\n%s", command, r.code, r.out)
			}
		case "rpm":
			for _, path := range []string{d.cronFile, d.cronFile + ".rpmsave"} {
				if _, err := os.Lstat(path); err == nil {
					t.Errorf("%s stays after rpm -e", path)
				}
			}
		case "apk":
			if _, err := os.Lstat(d.cronFile); err == nil {
				t.Errorf("%s stays after apk del", d.cronFile)
			}
		}
	})

	if d.format == "deb" {
		t.Run("T-PKG-13/purge-with-mail", func(t *testing.T) {
			out := mustRun(t, "", "dpkg", "-P", "mailcrier")
			t.Logf("purge:\n%s", out)
			if !strings.Contains(out, spoolNote) {
				t.Errorf("purge did not name the spool:\n%s", out)
			}
			for _, path := range []string{config, d.cronFile} {
				if _, err := os.Lstat(path); err == nil {
					t.Errorf("%s stays after dpkg -P", path)
				}
			}
			if r := run(t, nil, "", "dpkg-statoverride", "--list", binary); r.code != 1 {
				t.Errorf("dpkg-statoverride --list %s exited %d, want 1:\n%s", binary, r.code, r.out)
			}
			if got := spoolMessages(t, "hold"); len(got) != 1 {
				t.Errorf("hold/ = %v after dpkg -P, want the message", got)
			}
		})
	}

	if !t.Run("T-PKG-07/remove-empty-spool", func(t *testing.T) {
		alicePresent := recv.expect("smoke-empty-alice-4a19")
		out := mustRun(t, "", append(slices.Clone(d.install), d.packageFile(t))...)
		t.Logf("install:\n%s", out)
		writeConfig(t, working)
		queueRun(t)
		waitEvent(t, removedHeld, time.Now().Add(10*time.Second), "the message held before the removal", report)
		send(t, alice, "smoke-empty-alice-4a19", 0)
		waitEvent(t, alicePresent, time.Now().Add(10*time.Second), "the message of alice", report)
		if locks, _ := filepath.Glob(filepath.Join(spoolDir, "locks", "*.lock")); len(locks) == 0 || len(allSpoolMessages(t)) != 0 {
			t.Fatalf("want queue run locks and no message\n%s", report())
		}

		out = mustRun(t, "", d.remove...)
		t.Logf("remove:\n%s", out)
		checkRemovalOutput(t, out)
		if _, err := os.Lstat(spoolDir); err == nil {
			t.Errorf("%s stays after the removal\n%s", spoolDir, listSpool(t))
		}
		if d.format == "deb" {
			out := mustRun(t, "", "dpkg", "-P", "mailcrier")
			t.Logf("purge:\n%s", out)
			checkRemovalOutput(t, out)
			if r := run(t, nil, "", "dpkg-statoverride", "--list", binary); r.code != 1 {
				t.Errorf("dpkg-statoverride --list %s exited %d, want 1", binary, r.code)
			}
		}
	}) {
		t.FailNow()
	}

	if len(d.postfix) > 0 {
		t.Run("T-PKG-16/alternatives-without-postfix", func(t *testing.T) {
			install(t, d)
			mustRun(t, "", append([]string{"rpm", "-e"}, d.postfix...)...)
			if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != binary {
				t.Errorf("/usr/sbin/sendmail without postfix = %q, %v, want %s", target, err, binary)
			}
			mustRun(t, "", d.remove...)
			if _, err := os.Lstat("/usr/sbin/sendmail"); err == nil {
				t.Error("/usr/sbin/sendmail stays after the last MTA is removed")
			}
		})
	}

	if d.format == "apk" {
		t.Run("T-PKG-15/ssmtp-conflict", func(t *testing.T) {
			ssmtp, err := filepath.Glob("/opt/ssmtp/*.apk")
			if err != nil || len(ssmtp) == 0 {
				t.Fatalf("/opt/ssmtp = %v, %v", ssmtp, err)
			}
			mustRun(t, "", append([]string{"apk", "add", "--allow-untrusted", "--no-network"}, ssmtp...)...)
			r := run(t, nil, "", append(slices.Clone(d.install), d.packageFile(t))...)
			t.Logf("apk add with ssmtp exited %d:\n%s", r.code, r.out)
			if r.code == 0 {
				t.Error("apk add with ssmtp installed exited 0")
			}
			if !strings.Contains(r.out, "trying to overwrite usr/sbin/sendmail owned by ssmtp") {
				t.Errorf("apk add did not report the conflict with ssmtp")
			}
			if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != "/usr/sbin/ssmtp" {
				t.Errorf("/usr/sbin/sendmail = %q, %v, want ssmtp", target, err)
			}
			// apk 3.0 records the package as installed all the same and runs
			// its post-install; that state is logged, not required.
			info := run(t, nil, "", "apk", "info", "-e", "mailcrier")
			t.Logf("apk info -e mailcrier exited %d: %s", info.code, info.out)
		})
	}
}

// reinstall installs the same version of the package over itself. apk
// runs the upgrade scripts only for a package from a repository, so the
// file goes into a local one under the name apk expects.
func reinstall(t *testing.T, d distro) string {
	t.Helper()
	pkg := d.packageFile(t)
	switch d.format {
	case "deb":
		return mustRun(t, "", "dpkg", "-i", pkg)
	case "rpm":
		return mustRun(t, "", "rpm", "-Uvh", "--replacepkgs", pkg)
	}
	name, _, _ := strings.Cut(mustRun(t, "", "apk", "list", "-I", "mailcrier"), " ")
	repo := t.TempDir()
	arch := filepath.Join(repo, "x86_64")
	if err := os.Mkdir(arch, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(arch, name+".apk"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "", "apk", "index", "-q", "--allow-untrusted", "-o", filepath.Join(arch, "APKINDEX.tar.gz"), filepath.Join(arch, name+".apk"))
	out := mustRun(t, "", "apk", "fix", "--reinstall", "--allow-untrusted", "--no-network", "--repository", repo, "mailcrier")
	if !strings.Contains(out, "post-upgrade") {
		t.Errorf("apk fix --reinstall ran no post-upgrade:\n%s", out)
	}
	return out
}

// checkRemovalOutput fails when a removal of an empty spool reports
// messages or a directory it could not remove.
func checkRemovalOutput(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{spoolNote, "not empty", "No such file"} {
		if strings.Contains(out, bad) {
			t.Errorf("removal output contains %q:\n%s", bad, out)
		}
	}
}

// makeDue moves the next attempt of every target of the entries in
// queue/ into the past, which a queue run has no option for. It edits the
// sidecar of spool format version 1 and stops on any other.
func makeDue(t *testing.T) {
	t.Helper()
	sidecars, err := filepath.Glob(filepath.Join(spoolDir, "queue", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range sidecars {
		data, err := os.ReadFile(sidecar)
		if err != nil {
			t.Fatal(err)
		}
		var entry map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["version"] != json.Number("1") {
			t.Fatalf("%s: spool format version %v, the test knows 1", sidecar, entry["version"])
		}
		targets, _ := entry["targets"].(map[string]any)
		for _, state := range targets {
			if state, ok := state.(map[string]any); ok {
				state["next_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			}
		}
		data, err = json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sidecar, data, 0); err != nil {
			t.Fatal(err)
		}
	}
}
