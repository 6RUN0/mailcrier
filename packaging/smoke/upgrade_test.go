//go:build smoke

package smoke

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// unorderablePrevious maps a package format to the version of the previous
// release its package manager cannot order before the snapshot of HEAD,
// such as a dotted rc: dpkg sorts 0.1.0~rc.2 above 0.1.0~rc<time>, apk
// cannot parse 0.1.0_rc.2. The commit that bumps SMOKE_PREVIOUS past such
// a version drops its entry, docs/releasing.md says so.
var unorderablePrevious = map[string]string{}

// TestSmokeUpgrade installs the release SMOKE_PREVIOUS of the Makefile,
// leaves messages in queue/, hold/ and failed/ with its binary and upgrades
// to the package of HEAD, whose binary must deliver them. It pins that a new
// version reads the spool of the previous one: a change of the spool format
// needs a migration or a deliberate change of this test.
func TestSmokeUpgrade(t *testing.T) {
	d := currentDistro(t)
	previous := previousPackage(t, d)
	recv := newReceiver(t)
	dir := fixtures(t)
	alice, queuer := credential(t, "alice"), credential(t, "queuer")
	working := workingConfig(recv.URL, filepath.Join(dir, "hook"))
	report := func() string { return recv.report() + "\nspool:\n" + listSpool(t) }
	send := func(t *testing.T, cred *syscall.Credential, marker string, want int) {
		t.Helper()
		r := run(t, cred, "Subject: "+marker+"\n\nbody\n", "/usr/sbin/sendmail", "-i", "ops@example.org")
		if r.code != want {
			t.Fatalf("call with %s exited %d, want %d:\n%s", marker, r.code, want, r.out)
		}
	}
	queueRun := func(t *testing.T) {
		t.Helper()
		if r := run(t, credential(t, "mailcrier"), "", binary, "-q"); r.code != 0 {
			t.Fatalf("-q as mailcrier exited %d:\n%s\n%s", r.code, r.out, report())
		}
	}

	if !t.Run("T-PKG-19/install-previous", func(t *testing.T) {
		// The output of a published package is only logged: a fix of it
		// needs a new release.
		out := mustRun(t, "", append(slices.Clone(d.install), previous)...)
		t.Logf("install of %s:\n%s", previous, out)
		checkMode(t, binary, 0, serviceGID(t), 0o755|os.ModeSetgid)
	}) {
		t.FailNow()
	}

	const failedMarker, queuedMarker, heldMarker = "smoke-upgrade-failed-3b17", "smoke-upgrade-queued-a6d2", "smoke-upgrade-held-e840"
	failed, queued, held := recv.expect(failedMarker), recv.expect(queuedMarker), recv.expect(heldMarker)
	if !t.Run("T-PKG-19/upgrade-with-mail", func(t *testing.T) {
		// The binary of the previous release fails an expired message,
		// queues one that failed once and holds one without targets. No
		// call delivers after the held one: its queue run would release it.
		writeConfig(t, working)
		recv.failOnce(failedMarker)
		send(t, queuer, failedMarker, 0)
		makeExpired(t)
		queueRun(t)
		recv.failOnce(queuedMarker)
		send(t, queuer, queuedMarker, 0)
		writeConfig(t, heldConfig)
		send(t, alice, heldMarker, 78)
		for _, area := range []string{"queue", "hold", "failed"} {
			if got := spoolMessages(t, area); len(got) != 1 {
				t.Fatalf("%s/ = %v before the upgrade, want one message\n%s", area, got, report())
			}
		}
		locks, _ := filepath.Glob(filepath.Join(spoolDir, "locks", "*.lock"))
		if len(locks) == 0 {
			t.Fatalf("no queue run lock before the upgrade\n%s", report())
		}
		// An unchanged conffile whose mode the administrator changed: rpm
		// sets the mode of the package again, dpkg leaves it.
		if d.format != "apk" {
			if err := os.Chmod(d.cronFile, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.Stat(config)
		if err != nil {
			t.Fatal(err)
		}
		oldVersion, newVersion := packageVersion(t, d, previous), packageVersion(t, d, d.packageFile(t, pkgsDir))
		if got := installedVersion(t, d); got != oldVersion {
			t.Fatalf("installed version %s before the upgrade, want %s", got, oldVersion)
		}

		out := upgrade(t, d, d.packageFile(t, pkgsDir), oldVersion, newVersion)
		t.Logf("upgrade:\n%s", out)
		checkInstallerOutput(t, out)
		if got := installedVersion(t, d); got != newVersion {
			t.Errorf("installed version %s after the upgrade, want %s", got, newVersion)
		}
		checkUpgraded(t, d, heldConfig, before)
		switch d.format {
		case "rpm":
			display := mustRun(t, "", "alternatives", "--display", "mta")
			if n := strings.Count(display, binary+" - priority 100\n"); n != 1 {
				t.Errorf("alternatives --display mta lists %s with priority 100 %d times:\n%s", binary, n, display)
			}
			if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != binary {
				t.Errorf("/usr/sbin/sendmail = %q, %v after the upgrade, want %s", target, err, binary)
			}
			checkMode(t, d.cronFile, 0, 0, 0o644)
		default:
			for _, link := range mtaLinks {
				if target, err := filepath.EvalSymlinks(link); err != nil || target != binary {
					t.Errorf("%s = %q, %v after the upgrade, want %s", link, target, err, binary)
				}
			}
			if d.format == "deb" {
				checkMode(t, d.cronFile, 0, 0, 0o600)
			}
		}
		for _, area := range []string{"queue", "hold", "failed"} {
			if got := spoolMessages(t, area); len(got) != 1 {
				t.Errorf("%s/ = %v after the upgrade, want one message", area, got)
			}
		}
		if got, _ := filepath.Glob(filepath.Join(spoolDir, "locks", "*.lock")); !slices.Equal(got, locks) {
			t.Errorf("locks/ = %v after the upgrade, want %v", got, locks)
		}
	}) {
		t.FailNow()
	}

	t.Run("T-PKG-19/deliver-after-upgrade", func(t *testing.T) {
		writeConfig(t, working)
		makeDue(t)
		queueRun(t)
		waitEvent(t, queued, time.Now().Add(10*time.Second), "the queued message", report)
		waitEvent(t, held, time.Now().Add(10*time.Second), "the held message", report)
		if !isEmpty(failed) {
			t.Errorf("the queue run sent the failed message again\n%s", report())
		}
		if got := spoolMessages(t, "failed"); len(got) != 1 {
			t.Errorf("failed/ = %v after the queue run, want the message", got)
		}
		if got := append(spoolMessages(t, "queue"), spoolMessages(t, "hold")...); len(got) != 0 {
			t.Errorf("spool holds %v after the queue run\n%s", got, report())
		}
	})
}

// previousPackage returns the package of the previous release in
// previousDir. It skips the test when the package manager cannot order its
// version before the one of HEAD and unorderablePrevious lists it, and
// fails when the list is wrong either way or names another version. rpm
// orders both by itself and refuses a downgrade without --oldpackage.
func previousPackage(t *testing.T, d distro) string {
	t.Helper()
	previous := d.packageFile(t, previousDir)
	if d.format == "rpm" {
		return previous
	}
	oldVersion, newVersion := packageVersion(t, d, previous), packageVersion(t, d, d.packageFile(t, pkgsDir))
	var isOrdered bool
	var tool string
	switch d.format {
	case "deb":
		tool = "dpkg"
		isOrdered = run(t, nil, "", "dpkg", "--compare-versions", oldVersion, "lt", newVersion).code == 0
	case "apk":
		tool = "apk"
		isOrdered = run(t, nil, "", "apk", "version", "-c", oldVersion).code == 0 &&
			strings.TrimSpace(run(t, nil, "", "apk", "version", "-t", oldVersion, newVersion).out) == "<"
	}
	listed, hasEntry := unorderablePrevious[d.format]
	isListed := hasEntry && listed == oldVersion
	switch {
	case hasEntry && !isListed:
		t.Fatalf("unorderablePrevious lists %s, not the previous release %s: drop it", listed, oldVersion)
	case isOrdered && isListed:
		t.Fatalf("unorderablePrevious lists %s, which %s orders before %s: drop it", oldVersion, tool, newVersion)
	case !isOrdered && !isListed:
		t.Fatalf("%s cannot order the previous release %s before %s", tool, oldVersion, newVersion)
	case !isOrdered:
		t.Skipf("%s cannot order the previous release %s before %s; the upgrade runs once SMOKE_PREVIOUS names a release it orders", tool, oldVersion, newVersion)
	}
	return previous
}

// packageVersion returns the version a package file declares, without the
// release of rpm.
func packageVersion(t *testing.T, d distro, pkg string) string {
	t.Helper()
	switch d.format {
	case "deb":
		return strings.TrimSpace(mustRun(t, "", "dpkg-deb", "-f", pkg, "Version"))
	case "rpm":
		return mustRun(t, "", "rpm", "-qp", "--qf", "%{VERSION}", pkg)
	}
	return apkPackageVersion(t, pkg)
}

// installedVersion returns the version of the installed package mailcrier,
// without the release of rpm.
func installedVersion(t *testing.T, d distro) string {
	t.Helper()
	switch d.format {
	case "deb":
		return mustRun(t, "", "dpkg-query", "-W", "-f", "${Version}", "mailcrier")
	case "rpm":
		return mustRun(t, "", "rpm", "-q", "--qf", "%{VERSION}", "mailcrier")
	}
	return installedAPKVersion(t, "mailcrier")
}

// apkPackageVersion returns pkgver of the .PKGINFO of an apk file: gzip
// streams of tar segments whose end blocks are cut, so one reader runs
// through them.
func apkPackageVersion(t *testing.T, pkg string) string {
	t.Helper()
	file, err := os.Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	stream, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(stream)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		if header.Name != ".PKGINFO" {
			continue
		}
		info, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(info)) {
			if version, ok := strings.CutPrefix(strings.TrimSpace(line), "pkgver = "); ok {
				return version
			}
		}
	}
	t.Fatalf("%s has no pkgver in .PKGINFO", pkg)
	return ""
}

// upgrade installs pkg of newVersion over oldVersion as the package manager
// does it on an upgrade and returns its output; apk takes the package from a
// local repository, see localAPKRepository.
func upgrade(t *testing.T, d distro, pkg, oldVersion, newVersion string) string {
	t.Helper()
	switch d.format {
	case "deb":
		return mustRun(t, "", "dpkg", "-i", pkg)
	case "rpm":
		return mustRun(t, "", "rpm", "-Uvh", pkg)
	}
	repo := localAPKRepository(t, pkg, newVersion)
	out := mustRun(t, "", "apk", "add", "-u", "--allow-untrusted", "--no-network", "--repository", repo, "mailcrier")
	if want := fmt.Sprintf("Upgrading mailcrier (%s -> %s)", oldVersion, newVersion); !strings.Contains(out, want) {
		t.Errorf("apk add -u did not print %q:\n%s", want, out)
	}
	if !strings.Contains(out, "post-upgrade") {
		t.Errorf("apk add -u ran no post-upgrade:\n%s", out)
	}
	return out
}
