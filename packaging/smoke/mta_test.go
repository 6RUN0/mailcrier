//go:build smoke

package smoke

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mtaUser is the control file of a package that needs an MTA the way the
// mail programs of Debian declare it.
const mtaUser = `Package: smoke-mta-user
Version: 1
Architecture: all
Maintainer: smoke <smoke@example.org>
Depends: default-mta | mail-transport-agent
Description: needs an MTA
`

// TestSmokeMTA installs the package on Debian over postfix, an MTA of the
// archive: apt removes postfix for it, and afterwards a package that
// depends on mail-transport-agent installs without pulling in an MTA, with
// mailcrier the only one installed. The image holds postfix and the apt
// lists, the container has no network.
func TestSmokeMTA(t *testing.T) {
	d := currentDistro(t)
	switch d.format {
	case "rpm":
		t.Skip("rpm has no mail-transport-agent; the mta alternative is T-PKG-16 of TestSmokeLifecycle")
	case "apk":
		t.Skip("apk has no mail-transport-agent; the conflict with ssmtp is T-PKG-15 of TestSmokeLifecycle")
	}
	user := buildMTAUser(t)

	// Without an MTA bsd-mailx pulls one in and the package of mtaUser does
	// not install, so the checks after the install can tell the difference.
	mtas := simulatedMTAs(t)
	if len(mtas) == 0 {
		t.Fatal("apt-get -s install bsd-mailx installs no MTA before mailcrier")
	}
	t.Logf("MTA bsd-mailx pulls in without mailcrier: %v", mtas)
	if r := run(t, nil, "", "dpkg", "-i", user); r.code == 0 {
		t.Fatalf("dpkg -i of a package that needs an MTA exited 0 without one:\n%s", r.out)
	}
	mustRun(t, "", "dpkg", "-r", "smoke-mta-user")

	if !t.Run("T-PKG-20/replaces-mta", func(t *testing.T) {
		postfix, err := filepath.Glob("/opt/postfix/*.deb")
		if err != nil || len(postfix) == 0 {
			t.Fatalf("/opt/postfix = %v, %v", postfix, err)
		}
		// Upgrading a package of the image would test another system than
		// the image; a rebuilt image fetches a closure that fits it.
		for _, file := range postfix {
			name := strings.TrimSpace(mustRun(t, "", "dpkg-deb", "-f", file, "Package"))
			if r := run(t, nil, "", "dpkg-query", "-W", "-f", "${db:Status-Abbrev}", name); r.code == 0 && strings.HasPrefix(r.out, "ii") {
				t.Fatalf("%s of the postfix closure is installed, the closure upgrades installed packages: rebuild the image", name)
			}
		}
		// apt-get install --no-download of these files refuses to take
		// them from the archive it lists them in; dpkg installs them in one go.
		mustRun(t, "", append([]string{"env", "DEBIAN_FRONTEND=noninteractive", "dpkg", "-i"}, postfix...)...)
		if owner := mustRun(t, "", "dpkg", "-S", "/usr/sbin/sendmail"); owner != "postfix: /usr/sbin/sendmail\n" {
			t.Fatalf("dpkg -S /usr/sbin/sendmail = %q, want postfix", owner)
		}

		// dpkg -i refuses the package next to postfix, which conflicts with
		// mail-transport-agent; apt removes postfix first.
		out := mustRun(t, "", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", d.packageFile(t, pkgsDir))
		t.Logf("apt-get install:\n%s", out)
		checkInstallerOutput(t, out)
		checkMode(t, binary, 0, serviceGID(t), 0o755|os.ModeSetgid)
		if status := mustRun(t, "", "dpkg-query", "-W", "-f", "${Status}", "postfix"); status != "deinstall ok config-files" {
			t.Errorf("postfix is %q after the install, want removed", status)
		}
		if target, err := filepath.EvalSymlinks("/usr/sbin/sendmail"); err != nil || target != binary {
			t.Errorf("/usr/sbin/sendmail = %q, %v, want %s", target, err, binary)
		}
		if owner := mustRun(t, "", "dpkg", "-S", "/usr/sbin/sendmail"); owner != "mailcrier: /usr/sbin/sendmail\n" {
			t.Errorf("dpkg -S /usr/sbin/sendmail = %q, want mailcrier", owner)
		}
	}) {
		t.FailNow()
	}

	t.Run("T-PKG-21/satisfies-dependents", func(t *testing.T) {
		simulated := mustRun(t, "", "apt-get", "-s", "install", "bsd-mailx")
		for _, name := range instPackages(simulated) {
			if slices.Contains(mtas, name) || name == "postfix" || strings.HasPrefix(name, "exim4") {
				t.Errorf("apt-get -s install bsd-mailx installs the MTA %s next to mailcrier:\n%s", name, simulated)
			}
		}
		if r := run(t, nil, "", "dpkg", "-i", user); r.code != 0 {
			t.Errorf("dpkg -i of a package that needs an MTA exited %d with mailcrier:\n%s", r.code, r.out)
		}
		if got := installedMTAs(t); !slices.Equal(got, []string{"mailcrier"}) {
			t.Errorf("installed packages providing mail-transport-agent = %v, want mailcrier alone", got)
		}
	})
}

// installedMTAs returns the installed packages that provide
// mail-transport-agent.
func installedMTAs(t *testing.T) []string {
	t.Helper()
	var mtas []string
	out := mustRun(t, "", "dpkg-query", "-W", "-f", "${db:Status-Abbrev}\t${Package}\t${Provides}\n")
	for line := range strings.Lines(out) {
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) == 3 && strings.HasPrefix(fields[0], "ii") && strings.Contains(fields[2], "mail-transport-agent") {
			mtas = append(mtas, fields[1])
		}
	}
	return mtas
}

// buildMTAUser builds the package of mtaUser and returns its file.
func buildMTAUser(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	control := filepath.Join(dir, "smoke-mta-user", "DEBIAN")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "control"), []byte(mtaUser), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "smoke-mta-user.deb")
	mustRun(t, "", "dpkg-deb", "--build", filepath.Dir(control), file)
	return file
}

// simulatedMTAs returns the packages providing mail-transport-agent that
// apt-get -s install bsd-mailx would install.
func simulatedMTAs(t *testing.T) []string {
	t.Helper()
	simulated := instPackages(mustRun(t, "", "apt-get", "-s", "install", "bsd-mailx"))
	// The lines after "Reverse Provides:" name a provider and its version.
	_, providers, _ := strings.Cut(mustRun(t, "", "apt-cache", "showpkg", "mail-transport-agent"), "Reverse Provides:")
	var mtas []string
	for line := range strings.Lines(providers) {
		if fields := strings.Fields(line); len(fields) > 0 && slices.Contains(simulated, fields[0]) && !slices.Contains(mtas, fields[0]) {
			mtas = append(mtas, fields[0])
		}
	}
	return mtas
}

// instPackages returns the names of the "Inst" lines of apt-get -s.
func instPackages(out string) []string {
	var names []string
	for line := range strings.Lines(out) {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "Inst" {
			names = append(names, fields[1])
		}
	}
	return names
}
