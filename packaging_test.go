package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/6RUN0/mailcrier/internal/app"
	"github.com/6RUN0/mailcrier/internal/config"
)

// TestQueueRunnerFiles pins the periodic queue run of the packages: every
// 5 minutes as the mailcrier user, whose run takes the entries of all
// users; cron with MAILTO empty, because -q reports to syslog and a mail
// from cron would come back through this very program, and without quotes
// in the BusyBox crontab, whose crond takes MAILTO="" for an address and
// mails the output through sendmail; the cron.d line only where systemd is
// not running, so that the timer and cron do not both run the queue; both
// cron lines only while the binary is installed, since a removed deb keeps
// /etc/cron.d/mailcrier as a conffile and Debian Policy wants a cron job
// to check for its program; the service with a time limit, so that a run
// that hangs does not keep the timer from starting the next one.
func TestQueueRunnerFiles(t *testing.T) {
	cases := []struct {
		path  string
		lines []string
	}{
		{"packaging/systemd/mailcrier-queue.service", []string{"Type=oneshot", "User=mailcrier", "Group=mailcrier", "ExecStart=/usr/sbin/mailcrier -q", "TimeoutStartSec=3min"}},
		{"packaging/systemd/mailcrier-queue.timer", []string{"OnBootSec=2min", "OnUnitActiveSec=5min", "WantedBy=timers.target"}},
		{"packaging/cron/mailcrier", []string{`MAILTO=""`, "*/5 * * * * mailcrier if [ -x /usr/sbin/mailcrier ] && [ ! -d /run/systemd/system ]; then /usr/sbin/mailcrier -q; fi"}},
		{"packaging/cron/crontabs-mailcrier", []string{"MAILTO=", "*/5 * * * * if [ -x /usr/sbin/mailcrier ]; then /usr/sbin/mailcrier -q; fi"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			for _, want := range tc.lines {
				found := false
				for _, line := range lines {
					found = found || line == want
				}
				if !found {
					t.Errorf("%s lacks the line %q", tc.path, want)
				}
			}
			if !strings.HasSuffix(string(data), "\n") {
				t.Errorf("%s does not end with a newline, which cron requires", tc.path)
			}
		})
	}
}

// nfpmScripts are the script paths of one format or of all formats.
type nfpmScripts struct {
	PreInstall  string `yaml:"preinstall"`
	PostInstall string `yaml:"postinstall"`
	PreRemove   string `yaml:"preremove"`
	PostRemove  string `yaml:"postremove"`
}

// nfpmContent is one entry of contents in .goreleaser.yaml.
type nfpmContent struct {
	Src      string `yaml:"src"`
	Dst      string `yaml:"dst"`
	Type     string `yaml:"type"`
	Packager string `yaml:"packager"`
	FileInfo struct {
		Owner string `yaml:"owner"`
		Group string `yaml:"group"`
		Mode  uint32 `yaml:"mode"`
	} `yaml:"file_info"`
}

// nfpmOverride holds the fields .goreleaser.yaml sets per format.
type nfpmOverride struct {
	Provides     []string    `yaml:"provides"`
	Conflicts    []string    `yaml:"conflicts"`
	Replaces     []string    `yaml:"replaces"`
	Dependencies []string    `yaml:"dependencies"`
	Scripts      nfpmScripts `yaml:"scripts"`
}

// nfpmSection is the part of a goreleaser nfpms entry the package tests
// check; goreleaser itself validates the rest.
type nfpmSection struct {
	IDs        []string    `yaml:"ids"`
	Formats    []string    `yaml:"formats"`
	Maintainer string      `yaml:"maintainer"`
	License    string      `yaml:"license"`
	Section    string      `yaml:"section"`
	Homepage   string      `yaml:"homepage"`
	Bindir     string      `yaml:"bindir"`
	MTime      string      `yaml:"mtime"`
	Umask      uint32      `yaml:"umask"`
	Scripts    nfpmScripts `yaml:"scripts"`
	Deb        struct {
		Predepends []string `yaml:"predepends"`
	} `yaml:"deb"`
	RPM struct {
		Scripts struct {
			PostTrans string `yaml:"posttrans"`
		} `yaml:"scripts"`
	} `yaml:"rpm"`
	APK struct {
		Scripts struct {
			PreUpgrade  string `yaml:"preupgrade"`
			PostUpgrade string `yaml:"postupgrade"`
		} `yaml:"scripts"`
	} `yaml:"apk"`
	Overrides map[string]nfpmOverride `yaml:"overrides"`
	Contents  []nfpmContent           `yaml:"contents"`
}

// readNFPM returns the only nfpms entry of .goreleaser.yaml.
func readNFPM(t *testing.T) nfpmSection {
	t.Helper()
	data, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		NFPMs []nfpmSection `yaml:"nfpms"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.NFPMs) != 1 {
		t.Fatalf(".goreleaser.yaml has %d nfpms entries, want 1", len(file.NFPMs))
	}
	return file.NFPMs[0]
}

// packageScripts maps "<format>-<event>" to the script the package runs,
// with the overrides of the format applied over the common scripts.
func packageScripts(section nfpmSection) map[string]string {
	scripts := map[string]string{}
	for _, format := range section.Formats {
		override := section.Overrides[format].Scripts
		pick := func(own, common string) string {
			if own != "" {
				return own
			}
			return common
		}
		for event, path := range map[string]string{
			"preinstall":  pick(override.PreInstall, section.Scripts.PreInstall),
			"postinstall": pick(override.PostInstall, section.Scripts.PostInstall),
			"preremove":   pick(override.PreRemove, section.Scripts.PreRemove),
			"postremove":  pick(override.PostRemove, section.Scripts.PostRemove),
		} {
			if path != "" {
				scripts[format+"-"+event] = path
			}
		}
	}
	if section.RPM.Scripts.PostTrans != "" {
		scripts["rpm-posttrans"] = section.RPM.Scripts.PostTrans
	}
	if section.APK.Scripts.PreUpgrade != "" {
		scripts["apk-preupgrade"] = section.APK.Scripts.PreUpgrade
	}
	if section.APK.Scripts.PostUpgrade != "" {
		scripts["apk-postupgrade"] = section.APK.Scripts.PostUpgrade
	}
	return scripts
}

// packageEntry is the expected type, owner, group and mode of a package path.
type packageEntry struct {
	typ, owner, group string
	mode              uint32
}

// expectedContents lists every contents entry of the packages by
// destination and packager (empty for all formats). The binary and its
// mode are not here: goreleaser adds it as root:root 0755, and the scripts
// set the group and the setgid bit.
func expectedContents() map[[2]string]packageEntry {
	contents := map[[2]string]packageEntry{
		{"/etc/mailcrier.conf", ""}:                                    {"config|noreplace", "root", "mailcrier", 0o640},
		{"/etc/mailcrier.d", ""}:                                       {"dir", "root", "mailcrier", 0o750},
		{"/var/spool/mailcrier", ""}:                                   {"dir", "root", "mailcrier", 0o750},
		{"/usr/share/man/man8/mailcrier.8", ""}:                        {"", "root", "root", 0o644},
		{"/etc/crontabs/mailcrier", "apk"}:                             {"", "root", "root", 0o600},
		{"/usr/share/doc/mailcrier/copyright", "deb"}:                  {"", "root", "root", 0o644},
		{"/usr/share/licenses/mailcrier/LICENSE", "rpm"}:               {"license", "root", "root", 0o644},
		{"/usr/share/selinux/packages/mailcrier/mailcrier.cil", "rpm"}: {"", "root", "root", 0o644},
		{"/usr/share/licenses/mailcrier/LICENSE", "apk"}:               {"", "root", "root", 0o644},
	}
	for _, area := range []string{"tmp", "queue", "hold", "failed", "locks"} {
		contents[[2]string{"/var/spool/mailcrier/" + area, ""}] = packageEntry{"dir", "root", "mailcrier", 0o2770}
	}
	for _, link := range []string{"/usr/sbin/sendmail", "/usr/lib/sendmail", "/usr/bin/mailq", "/usr/bin/newaliases"} {
		contents[[2]string{link, "deb"}] = packageEntry{"symlink", "root", "root", 0o777}
		contents[[2]string{link, "apk"}] = packageEntry{"symlink", "root", "root", 0o777}
		contents[[2]string{link, "rpm"}] = packageEntry{"ghost", "root", "root", 0}
	}
	for _, packager := range []string{"deb", "rpm"} {
		contents[[2]string{"/usr/lib/systemd/system/mailcrier-queue.service", packager}] = packageEntry{"", "root", "root", 0o644}
		contents[[2]string{"/usr/lib/systemd/system/mailcrier-queue.timer", packager}] = packageEntry{"", "root", "root", 0o644}
		contents[[2]string{"/etc/cron.d/mailcrier", packager}] = packageEntry{"config|noreplace", "root", "root", 0o644}
	}
	for _, name := range configExamples {
		contents[[2]string{"/usr/share/doc/mailcrier/examples/" + name, ""}] = packageEntry{"", "root", "root", 0o644}
	}
	return contents
}

// TestPackageContents pins the package section of .goreleaser.yaml: one
// entry for the mailcrier build in deb, rpm and apk, the metadata the
// scripts depend on, and every contents entry with type, owner, group and
// mode. An explicit mode keeps the packages of every checkout equal: nFPM
// takes the mode of the source file minus its umask otherwise, and stats
// the target of a symlink relative to the working directory.
func TestPackageContents(t *testing.T) {
	t.Run("T-ADJ-54/contents", func(t *testing.T) {
		section := readNFPM(t)
		for _, check := range []struct {
			name      string
			got, want any
		}{
			{"ids", section.IDs, []string{"mailcrier"}},
			{"formats", section.Formats, []string{"deb", "rpm", "apk"}},
			{"maintainer", section.Maintainer, "Boris Talovikov <boris.t.66@gmail.com>"},
			{"license", section.License, "BSD-3-Clause"},
			{"section", section.Section, "mail"},
			{"homepage", section.Homepage, "https://github.com/6RUN0/mailcrier"},
			{"bindir", section.Bindir, "/usr/sbin"},
			{"mtime", section.MTime, "{{ .CommitDate }}"},
			{"umask", section.Umask, uint32(0o022)},
			{"deb.predepends", section.Deb.Predepends, []string{"init-system-helpers (>= 1.54~)", "passwd"}},
			{"deb provides", section.Overrides["deb"].Provides, []string{"mail-transport-agent"}},
			{"deb conflicts", section.Overrides["deb"].Conflicts, []string{"mail-transport-agent"}},
			{"deb replaces", section.Overrides["deb"].Replaces, []string{"mail-transport-agent"}},
			{"rpm provides", section.Overrides["rpm"].Provides, []string{"MTA"}},
			{"rpm dependencies", section.Overrides["rpm"].Dependencies, []string{"/usr/sbin/useradd", "/usr/sbin/alternatives"}},
			{"apk replaces", section.Overrides["apk"].Replaces, []string(nil)},
			{"scripts", packageScripts(section), map[string]string{
				"deb-preinstall":  "packaging/scripts/preinstall.sh",
				"deb-postinstall": "packaging/scripts/deb/postinst.sh",
				"deb-preremove":   "packaging/scripts/deb/prerm.sh",
				"deb-postremove":  "packaging/scripts/deb/postrm.sh",
				"rpm-preinstall":  "packaging/scripts/preinstall.sh",
				"rpm-postinstall": "packaging/scripts/rpm/post.sh",
				"rpm-preremove":   "packaging/scripts/rpm/preun.sh",
				"rpm-postremove":  "packaging/scripts/rpm/postun.sh",
				"rpm-posttrans":   "packaging/scripts/rpm/posttrans.sh",
				"apk-preinstall":  "packaging/scripts/preinstall.sh",
				"apk-postinstall": "packaging/scripts/apk/post-install.sh",
				"apk-preupgrade":  "packaging/scripts/preinstall.sh",
				"apk-postupgrade": "packaging/scripts/apk/post-upgrade.sh",
				"apk-preremove":   "packaging/scripts/apk/pre-deinstall.sh",
				"apk-postremove":  "packaging/scripts/apk/post-deinstall.sh",
			}},
		} {
			if fmt.Sprint(check.got) != fmt.Sprint(check.want) {
				t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
			}
		}

		want := expectedContents()
		seen := map[[2]string]bool{}
		for _, content := range section.Contents {
			key := [2]string{content.Dst, content.Packager}
			if seen[key] {
				t.Errorf("contents: %s (packager %q) is listed twice", content.Dst, content.Packager)
				continue
			}
			seen[key] = true
			entry, ok := want[key]
			if !ok {
				t.Errorf("contents: unexpected entry %s (packager %q)", content.Dst, content.Packager)
				continue
			}
			got := packageEntry{content.Type, content.FileInfo.Owner, content.FileInfo.Group, content.FileInfo.Mode}
			if got.owner == "" {
				got.owner = "root"
			}
			if got.group == "" {
				got.group = "root"
			}
			if got != entry {
				t.Errorf("contents: %s (packager %q) = %+v, want %+v", content.Dst, content.Packager, got, entry)
			}
			if content.Src != "" && content.FileInfo.Mode == 0 {
				t.Errorf("contents: %s (packager %q) has src and no mode", content.Dst, content.Packager)
			}
			if content.Type == "symlink" {
				if target := path.Join(path.Dir(content.Dst), content.Src); target != "/usr/sbin/mailcrier" {
					t.Errorf("contents: %s (packager %q) points to %s, want /usr/sbin/mailcrier", content.Dst, content.Packager, target)
				}
			}
			if content.Src != "" && content.Type != "symlink" {
				if _, err := os.Stat(content.Src); err != nil {
					t.Errorf("contents: %s: %v", content.Dst, err)
				}
			}
		}
		for key := range want {
			if !seen[key] {
				t.Errorf("contents: %s (packager %q) is missing", key[0], key[1])
			}
		}
	})
}

// scriptRedirects are the paths of the host a package script tests or
// writes, mapped to names under the temporary directory of a test case.
var scriptRedirects = []struct{ path, name string }{
	{"/run/systemd/system", "run-systemd"},
	{"/etc/crontabs", "crontabs"},
	{"/var/spool/mailcrier", "spool"},
	{"/usr/sbin/nologin", "nologin"},
	{"/usr/bin/deb-systemd-helper", "bin/deb-systemd-helper"},
	{"/etc/selinux/config", "selinux-config"},
}

// scriptArguments are the absolute paths a package script may pass on to
// a command or use as its interpreter; any other one would reach the host.
var scriptArguments = []string{"/bin/sh", "/dev/null", "/usr/sbin/mailcrier", "/usr/sbin/sendmail", "/usr/bin/mailq", "/usr/bin/newaliases", "/usr/lib/sendmail", "/sbin/nologin",
	"/usr/share/selinux/packages/mailcrier/mailcrier.cil"}

var absolutePath = regexp.MustCompile(`/[A-Za-z0-9._/-]+`)

// commandPath finds an absolute path where sh takes the name of a command:
// at the start of a line, after an operator or a keyword that starts one,
// or as the operand of exec or command. Such a path bypasses the stubs.
var commandPath = regexp.MustCompile("(?m)(?:^|[;&|(!{`]|\\b(?:if|elif|then|else|do|while|until|exec|command)\\s)\\s*[\"']?/")

// shellComment is a comment of sh, which may name any path.
var shellComment = regexp.MustCompile(`(?m)(?:^|\s)#.*$`)

// scriptStubs are the commands a package script may run; each one is a
// stub that logs its arguments. rm is the real one: the scripts delete
// files of the spool, which lies in the temporary directory.
var scriptStubs = []string{"dpkg-statoverride", "deb-systemd-helper", "deb-systemd-invoke", "systemctl", "alternatives", "chgrp", "chmod", "touch", "getent", "groupadd", "useradd", "addgroup", "adduser", "mailcrier",
	"semodule", "selinuxenabled", "restorecon"}

// scriptCase runs one package script with arguments in a temporary
// directory: present lists what exists there ("run-systemd", "spool",
// "nologin"), files the files created there with their directories, of
// which remain must exist after the script and the others must not;
// env adds variables to the environment; without drops stubs, and exits
// gives the exit status of a stub per prefix of its arguments ("" for
// any). spool expects the script to name the spool on stdout, stdout is
// a line the script must print, and a case with neither wants no output;
// fails expects a non-zero exit status.
type scriptCase struct {
	name    string
	event   string
	args    []string
	present []string
	files   []string
	remain  []string
	env     []string
	without []string
	exits   map[string]map[string]int
	want    []string
	spool   bool
	stdout  string
	fails   bool
}

// prepareScript rewrites the host paths of script into dir. It stops the
// test before the script runs on an absolute path in the place of a command
// and on any other absolute path that is not a known argument, so a script
// never reaches a program of the host.
func prepareScript(t *testing.T, script, dir string) string {
	t.Helper()
	code := shellComment.ReplaceAllString(strings.ReplaceAll(script, "\\\n", " "), "")
	for _, match := range commandPath.FindAllStringIndex(code, -1) {
		t.Errorf("script runs the host command %s", absolutePath.FindString(code[match[1]-1:]))
	}
	for _, path := range absolutePath.FindAllString(script, -1) {
		redirected := slices.ContainsFunc(scriptRedirects, func(redirect struct{ path, name string }) bool {
			return path == redirect.path || strings.HasPrefix(path, redirect.path+"/")
		})
		if !redirected && !slices.Contains(scriptArguments, path) {
			t.Errorf("script names the host path %s", path)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	for _, redirect := range scriptRedirects {
		script = strings.ReplaceAll(script, redirect.path, filepath.Join(dir, redirect.name))
	}
	return script
}

// writeStubs creates the stub commands of tc in dir/bin, each appending
// "<name> <args>" to dir/log, or "<name>" when it has none.
func writeStubs(t *testing.T, tc scriptCase, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rm, err := exec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rm, filepath.Join(bin, "rm")); err != nil {
		t.Fatal(err)
	}
	for _, name := range scriptStubs {
		if slices.Contains(tc.without, name) {
			continue
		}
		var cases strings.Builder
		for prefix, code := range tc.exits[name] {
			fmt.Fprintf(&cases, "%q*) exit %d ;;\n", prefix, code)
		}
		stub := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\"${*:+ $*}\" >>%q\ncase \"$*\" in\n%sesac\nexit 0\n", name, filepath.Join(dir, "log"), cases.String())
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// scriptShells returns the interpreters the scripts are run with: /bin/sh,
// and dash and BusyBox sh where installed, which find what bash accepts and
// they do not. A BusyBox that runs its own applets ahead of PATH would
// bypass the stubs and is left out.
func scriptShells(t *testing.T) map[string][]string {
	t.Helper()
	shells := map[string][]string{"sh": {"/bin/sh"}}
	if path, err := exec.LookPath("dash"); err == nil {
		shells["dash"] = []string{path}
	} else {
		t.Log("dash not installed, scripts not run with it")
	}
	path, err := exec.LookPath("busybox")
	if err != nil {
		t.Log("busybox not installed, scripts not run with it")
		return shells
	}
	probe := exec.Command(path, "sh", "-c", "command -v chgrp")
	probe.Env = []string{"PATH=" + t.TempDir()}
	if probe.Run() == nil {
		t.Log("busybox sh prefers its applets to PATH, scripts not run with it")
		return shells
	}
	shells["busybox"] = []string{path, "sh"}
	return shells
}

// TestPackageScripts runs every maintainer script with stub commands and
// compares the commands it runs, in order. A script must do nothing on an
// upgrade where it acts on removal, never run mailcrier or touch its
// configuration, set the group of the binary before its mode (chgrp clears
// the setgid bit), keep a timer the administrator masked only through the
// unmask of debhelper, delete the queue run locks before removal only when
// the spool holds no message (the package manager then removes the empty
// spool), name the spool after removal only when it holds one, install
// the SELinux module only where /etc/selinux/config exists (the smoke
// images have semodule without it) and print how to install it when that
// fails, and exit 0 when a command that is not essential fails, because a failed script
// leaves dpkg half done and stops an rpm erase. It must fail when the
// group, the user, the setgid bit or the alternative cannot be set up: the
// package would install a binary that cannot read its configuration.
func TestPackageScripts(t *testing.T) {
	section := readNFPM(t)
	scripts := packageScripts(section)
	shells := scriptShells(t)
	const timer = "mailcrier-queue.timer"
	failing := func(names ...string) map[string]map[string]int {
		exits := map[string]map[string]int{}
		for _, name := range names {
			exits[name] = map[string]int{"": 1}
		}
		return exits
	}
	statoverride := []string{"dpkg-statoverride --list /usr/sbin/mailcrier", "dpkg-statoverride --update --add root mailcrier 2755 /usr/sbin/mailcrier"}
	setgid := []string{"chgrp mailcrier /usr/sbin/mailcrier", "chmod 2755 /usr/sbin/mailcrier"}
	alternatives := "alternatives --install /usr/sbin/sendmail mta /usr/sbin/mailcrier 100" +
		" --slave /usr/bin/mailq mta-mailq /usr/sbin/mailcrier" +
		" --slave /usr/bin/newaliases mta-newaliases /usr/sbin/mailcrier" +
		" --slave /usr/lib/sendmail mta-sendmail /usr/sbin/mailcrier"
	const module = "/usr/share/selinux/packages/mailcrier/mailcrier.cil"
	semoduleInstall := "semodule -X 200 -i " + module
	moduleHint := "mailcrier: SELinux module not installed; to install it run: " + semoduleInstall
	relabel := "restorecon -RF /usr/sbin/mailcrier {spool}"
	cases := []scriptCase{
		{name: "preinstall-existing", event: "deb-preinstall", args: []string{"install"},
			want: []string{"getent group mailcrier", "getent passwd mailcrier"}},
		{name: "preinstall-shadow", event: "rpm-preinstall", args: []string{"1"}, present: []string{"nologin"},
			exits: map[string]map[string]int{"getent": {"": 2}},
			want:  []string{"getent group mailcrier", "groupadd -r mailcrier", "getent passwd mailcrier", "useradd -r -g mailcrier -d {spool} -M -s {nologin} mailcrier"}},
		{name: "preinstall-busybox", event: "apk-preupgrade", args: []string{"0.2.0", "0.1.0"}, without: []string{"groupadd", "useradd"},
			exits: map[string]map[string]int{"getent": {"": 2}},
			want:  []string{"getent group mailcrier", "addgroup -S mailcrier", "getent passwd mailcrier", "adduser -S -D -H -h {spool} -s /sbin/nologin -G mailcrier mailcrier"}},
		{name: "deb-postinstall-install", event: "deb-postinstall", args: []string{"configure"}, present: []string{"run-systemd"},
			exits: map[string]map[string]int{"dpkg-statoverride": {"--list": 1}},
			want: append(slices.Clone(statoverride), "deb-systemd-helper unmask "+timer, "deb-systemd-helper --quiet was-enabled "+timer,
				"deb-systemd-helper enable "+timer, "systemctl --system daemon-reload", "deb-systemd-invoke start "+timer)},
		{name: "deb-postinstall-upgrade", event: "deb-postinstall", args: []string{"configure", "0.1.0"}, present: []string{"run-systemd"},
			exits: map[string]map[string]int{"deb-systemd-helper": {"--quiet was-enabled": 1}},
			want: []string{statoverride[0], "deb-systemd-helper unmask " + timer, "deb-systemd-helper --quiet was-enabled " + timer,
				"deb-systemd-helper update-state " + timer, "systemctl --system daemon-reload", "deb-systemd-invoke restart " + timer}},
		{name: "deb-postinstall-abort-remove", event: "deb-postinstall", args: []string{"abort-remove"},
			exits: map[string]map[string]int{"dpkg-statoverride": {"--list": 1}},
			want:  append(slices.Clone(statoverride), "deb-systemd-helper unmask "+timer, "deb-systemd-helper --quiet was-enabled "+timer, "deb-systemd-helper enable "+timer)},
		{name: "deb-postinstall-failing", event: "deb-postinstall", args: []string{"configure", "0.1.0"}, present: []string{"run-systemd"},
			exits: failing("deb-systemd-helper", "deb-systemd-invoke", "systemctl"),
			want: []string{statoverride[0], "deb-systemd-helper unmask " + timer, "deb-systemd-helper --quiet was-enabled " + timer,
				"deb-systemd-helper update-state " + timer, "systemctl --system daemon-reload", "deb-systemd-invoke restart " + timer}},
		{name: "deb-preremove-upgrade", event: "deb-preremove", args: []string{"upgrade", "0.2.0"}, present: []string{"run-systemd"}},
		{name: "deb-preremove-remove", event: "deb-preremove", args: []string{"remove"}, present: []string{"run-systemd"},
			exits: failing("deb-systemd-invoke"), want: []string{"deb-systemd-invoke stop " + timer}},
		{name: "deb-postremove-upgrade", event: "deb-postremove", args: []string{"upgrade", "0.2.0"}, present: []string{"run-systemd"},
			files: []string{"spool/hold/x.eml"}, remain: []string{"spool/hold/x.eml"}},
		{name: "deb-postremove-remove", event: "deb-postremove", args: []string{"remove"}, present: []string{"run-systemd"},
			want: []string{"systemctl --system daemon-reload"}},
		{name: "deb-postremove-purge", event: "deb-postremove", args: []string{"purge"}, present: []string{"spool"},
			exits: failing("deb-systemd-helper", "dpkg-statoverride"),
			want:  []string{"deb-systemd-helper purge " + timer, "dpkg-statoverride --quiet --remove /usr/sbin/mailcrier"}},
		{name: "rpm-postinstall-install", event: "rpm-postinstall", args: []string{"1"}, present: []string{"run-systemd"},
			want: append(slices.Clone(setgid), alternatives, "systemctl enable "+timer, "systemctl start "+timer)},
		{name: "rpm-postinstall-install-failing", event: "rpm-postinstall", args: []string{"1"},
			exits: failing("systemctl"), want: append(slices.Clone(setgid), alternatives, "systemctl enable "+timer)},
		{name: "rpm-postinstall-upgrade", event: "rpm-postinstall", args: []string{"2"}, present: []string{"run-systemd"},
			want: append(slices.Clone(setgid), alternatives, "systemctl daemon-reload")},
		{name: "rpm-preremove-upgrade", event: "rpm-preremove", args: []string{"1"}, present: []string{"run-systemd"}},
		{name: "rpm-preremove-erase", event: "rpm-preremove", args: []string{"0"}, present: []string{"run-systemd"},
			exits: failing("systemctl", "alternatives"),
			want:  []string{"systemctl disable " + timer, "systemctl stop " + timer, "alternatives --remove mta /usr/sbin/mailcrier"}},
		{name: "rpm-postremove-upgrade", event: "rpm-postremove", args: []string{"1"}, present: []string{"run-systemd"},
			files: []string{"spool/hold/x.eml"}, remain: []string{"spool/hold/x.eml"}},
		{name: "rpm-postremove-erase", event: "rpm-postremove", args: []string{"0"}, present: []string{"run-systemd", "spool"},
			exits: failing("systemctl"), want: []string{"systemctl daemon-reload"}},
		{name: "rpm-posttrans-no-selinux", event: "rpm-posttrans", args: []string{"1"}},
		{name: "rpm-posttrans-no-semodule", event: "rpm-posttrans", args: []string{"1"}, present: []string{"selinux-config"},
			without: []string{"semodule"}, stdout: moduleHint},
		{name: "rpm-posttrans-enforcing", event: "rpm-posttrans", args: []string{"1"}, present: []string{"selinux-config"},
			want: []string{semoduleInstall, "selinuxenabled", relabel}},
		{name: "rpm-posttrans-disabled", event: "rpm-posttrans", args: []string{"2"}, present: []string{"selinux-config"},
			exits: failing("selinuxenabled"), want: []string{semoduleInstall, "selinuxenabled"}},
		{name: "rpm-posttrans-semodule-failing", event: "rpm-posttrans", args: []string{"1"}, present: []string{"selinux-config"},
			exits: failing("semodule"), want: []string{semoduleInstall}, stdout: moduleHint},
		{name: "rpm-posttrans-restorecon-failing", event: "rpm-posttrans", args: []string{"1"}, present: []string{"selinux-config"},
			exits: failing("restorecon"), want: []string{semoduleInstall, "selinuxenabled", relabel}, spool: true},
		{name: "rpm-postremove-erase-selinux", event: "rpm-postremove", args: []string{"0"}, present: []string{"selinux-config"},
			exits: failing("semodule"), want: []string{"semodule -X 200 -r mailcrier"}},
		{name: "rpm-postremove-erase-no-semodule", event: "rpm-postremove", args: []string{"0"}, present: []string{"selinux-config"},
			without: []string{"semodule"}},
		{name: "rpm-postremove-upgrade-selinux", event: "rpm-postremove", args: []string{"1"}, present: []string{"selinux-config"}},
		{name: "apk-postinstall", event: "apk-postinstall", args: []string{"0.1.0"},
			want: append(slices.Clone(setgid), "touch {crontabs}/cron.update")},
		{name: "apk-postupgrade", event: "apk-postupgrade", args: []string{"0.2.0", "0.1.0"},
			exits: failing("touch"), want: append(slices.Clone(setgid), "touch {crontabs}/cron.update")},
		{name: "apk-postremove", event: "apk-postremove", args: []string{"0.1.0"}, present: []string{"spool"},
			exits: failing("touch"), want: []string{"touch {crontabs}/cron.update"}},
		{name: "deb-preremove-remove-locks", event: "deb-preremove", args: []string{"remove"},
			files: []string{"spool/locks/drain-0.lock", "spool/locks/drain-1000.lock", "spool/tmp/x"}, remain: []string{"spool/tmp/x"}},
		{name: "deb-preremove-remove-mail", event: "deb-preremove", args: []string{"remove"},
			files: []string{"spool/locks/drain-0.lock", "spool/hold/x.eml"}, remain: []string{"spool/locks/drain-0.lock", "spool/hold/x.eml"}},
		{name: "deb-preremove-remove-dpkg-root", event: "deb-preremove", args: []string{"remove"}, present: []string{"run-systemd"},
			env: []string{"DPKG_ROOT=/nonexistent"}, files: []string{"spool/locks/drain-0.lock"}, remain: []string{"spool/locks/drain-0.lock"}},
		{name: "deb-preremove-upgrade-locks", event: "deb-preremove", args: []string{"upgrade", "0.2.0"},
			files: []string{"spool/locks/drain-0.lock"}, remain: []string{"spool/locks/drain-0.lock"}},
		{name: "rpm-preremove-erase-locks", event: "rpm-preremove", args: []string{"0"},
			files: []string{"spool/locks/drain-0.lock", "spool/locks/drain-1000.lock"},
			want:  []string{"systemctl disable " + timer, "alternatives --remove mta /usr/sbin/mailcrier"}},
		{name: "rpm-preremove-erase-mail", event: "rpm-preremove", args: []string{"0"},
			files: []string{"spool/locks/drain-0.lock", "spool/queue/x.eml"}, remain: []string{"spool/locks/drain-0.lock", "spool/queue/x.eml"},
			want: []string{"systemctl disable " + timer, "alternatives --remove mta /usr/sbin/mailcrier"}},
		{name: "rpm-preremove-upgrade-locks", event: "rpm-preremove", args: []string{"1"},
			files: []string{"spool/locks/drain-0.lock"}, remain: []string{"spool/locks/drain-0.lock"}},
		{name: "apk-preremove-locks", event: "apk-preremove", args: []string{"0.1.0"},
			files: []string{"spool/locks/drain-0.lock", "spool/locks/drain-1000.lock"}},
		{name: "apk-preremove-mail", event: "apk-preremove", args: []string{"0.1.0"},
			files: []string{"spool/locks/drain-0.lock", "spool/failed/x.eml"}, remain: []string{"spool/locks/drain-0.lock", "spool/failed/x.eml"}},
		{name: "deb-postremove-remove-mail", event: "deb-postremove", args: []string{"remove"}, spool: true,
			files: []string{"spool/hold/x.eml"}, remain: []string{"spool/hold/x.eml"}},
		{name: "deb-postremove-purge-mail", event: "deb-postremove", args: []string{"purge"}, spool: true,
			files: []string{"spool/queue/x.eml"}, remain: []string{"spool/queue/x.eml"},
			want: []string{"deb-systemd-helper purge " + timer, "dpkg-statoverride --quiet --remove /usr/sbin/mailcrier"}},
		{name: "deb-postremove-remove-tmp", event: "deb-postremove", args: []string{"remove"},
			files: []string{"spool/tmp/x"}, remain: []string{"spool/tmp/x"}},
		{name: "rpm-postremove-erase-mail", event: "rpm-postremove", args: []string{"0"}, spool: true,
			files: []string{"spool/failed/x.eml"}, remain: []string{"spool/failed/x.eml"}},
		{name: "rpm-postremove-erase-tmp", event: "rpm-postremove", args: []string{"0"},
			files: []string{"spool/tmp/x"}, remain: []string{"spool/tmp/x"}},
		{name: "apk-postremove-mail", event: "apk-postremove", args: []string{"0.1.0"}, spool: true,
			files: []string{"spool/hold/x.eml"}, remain: []string{"spool/hold/x.eml"}, want: []string{"touch {crontabs}/cron.update"}},
		{name: "apk-postremove-tmp", event: "apk-postremove", args: []string{"0.1.0"},
			files: []string{"spool/tmp/x"}, remain: []string{"spool/tmp/x"}, want: []string{"touch {crontabs}/cron.update"}},
		{name: "preinstall-groupadd-failing", event: "rpm-preinstall", args: []string{"1"}, fails: true,
			exits: map[string]map[string]int{"getent": {"": 2}, "groupadd": {"": 1}},
			want:  []string{"getent group mailcrier", "groupadd -r mailcrier"}},
		{name: "preinstall-useradd-failing", event: "rpm-preinstall", args: []string{"1"}, fails: true,
			exits: map[string]map[string]int{"getent": {"": 2}, "useradd": {"": 1}},
			want:  []string{"getent group mailcrier", "groupadd -r mailcrier", "getent passwd mailcrier", "useradd -r -g mailcrier -d {spool} -M -s /sbin/nologin mailcrier"}},
		{name: "preinstall-addgroup-failing", event: "apk-preinstall", args: []string{"0.1.0"}, without: []string{"groupadd", "useradd"}, fails: true,
			exits: map[string]map[string]int{"getent": {"": 2}, "addgroup": {"": 1}},
			want:  []string{"getent group mailcrier", "addgroup -S mailcrier"}},
		{name: "preinstall-adduser-failing", event: "apk-preinstall", args: []string{"0.1.0"}, without: []string{"groupadd", "useradd"}, fails: true,
			exits: map[string]map[string]int{"getent": {"": 2}, "adduser": {"": 1}},
			want:  []string{"getent group mailcrier", "addgroup -S mailcrier", "getent passwd mailcrier", "adduser -S -D -H -h {spool} -s /sbin/nologin -G mailcrier mailcrier"}},
		{name: "deb-postinstall-statoverride-failing", event: "deb-postinstall", args: []string{"configure"}, fails: true,
			exits: map[string]map[string]int{"dpkg-statoverride": {"--list": 1, "--update --add": 1}},
			want:  slices.Clone(statoverride)},
		{name: "rpm-postinstall-alternatives-failing", event: "rpm-postinstall", args: []string{"1"}, fails: true,
			exits: map[string]map[string]int{"alternatives": {"--install": 1}},
			want:  append(slices.Clone(setgid), alternatives)},
	}
	for _, setup := range []struct {
		event string
		args  []string
	}{{"rpm-postinstall", []string{"2"}}, {"apk-postinstall", []string{"0.1.0"}}, {"apk-postupgrade", []string{"0.2.0", "0.1.0"}}} {
		for i, command := range setgid {
			name := strings.Fields(command)[0]
			cases = append(cases, scriptCase{name: setup.event + "-" + name + "-failing", event: setup.event, args: setup.args, fails: true,
				exits: failing(name), want: slices.Clone(setgid[:i+1])})
		}
	}
	tested := map[string]bool{}
	for _, tc := range cases {
		path, ok := scripts[tc.event]
		if !ok {
			t.Fatalf("%s: no script for %s", tc.name, tc.event)
		}
		tested[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("/etc/mailcrier.")) {
			t.Errorf("%s names the configuration", path)
		}
		for shell, argv := range shells {
			t.Run("T-ADJ-54/"+tc.name+"-"+shell, func(t *testing.T) {
				dir := t.TempDir()
				if absolutePath.FindString(dir) != dir {
					t.Fatalf("the temporary directory %q has characters sh splits or expands, and the scripts get it unquoted; set TMPDIR to a path of letters, digits and ._/-", dir)
				}
				for _, name := range tc.present {
					if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range tc.files {
					file := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				writeStubs(t, tc, dir)
				cmd := exec.Command(argv[0], append(append(argv[1:], "-s"), tc.args...)...)
				cmd.Env = append([]string{"PATH=" + filepath.Join(dir, "bin")}, tc.env...)
				cmd.Stdin = strings.NewReader(prepareScript(t, string(data), dir))
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				var exit *exec.ExitError
				switch {
				case tc.fails && !errors.As(err, &exit):
					t.Errorf("%s %s: %v, want a non-zero exit status", path, strings.Join(tc.args, " "), err)
				case !tc.fails && err != nil:
					t.Errorf("%s %s: %v, stderr %q", path, strings.Join(tc.args, " "), err, stderr.String())
				}
				if stderr.Len() > 0 {
					t.Errorf("stderr %q, want none", stderr.String())
				}
				log, err := os.ReadFile(filepath.Join(dir, "log"))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				got := strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
				if len(log) == 0 {
					got = nil
				}
				want := make([]string, len(tc.want))
				for i, line := range tc.want {
					want[i] = strings.NewReplacer("{spool}", filepath.Join(dir, "spool"), "{nologin}", filepath.Join(dir, "nologin"),
						"{crontabs}", filepath.Join(dir, "crontabs")).Replace(line)
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s %s ran\n%s\nwant\n%s", path, strings.Join(tc.args, " "), strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
				if named := strings.Contains(stdout.String(), filepath.Join(dir, "spool")); named != tc.spool {
					t.Errorf("stdout %q names the spool: %v, want %v", stdout.String(), named, tc.spool)
				}
				if !tc.spool && strings.TrimSuffix(stdout.String(), "\n") != tc.stdout {
					t.Errorf("stdout %q, want %q", stdout.String(), tc.stdout)
				}
				for _, name := range tc.files {
					_, err := os.Lstat(filepath.Join(dir, name))
					if exists, want := err == nil, slices.Contains(tc.remain, name); exists != want {
						t.Errorf("%s exists after the script: %v, want %v (%v)", name, exists, want, err)
					}
				}
			})
		}
	}
	for event, path := range scripts {
		if !tested[path] {
			t.Errorf("no case runs %s, the %s script", path, event)
		}
	}
}

// configExamples are the files of packaging/examples, installed in
// /usr/share/doc/mailcrier/examples.
var configExamples = []string{"mailcrier.conf", "telegram.conf", "team-chat.conf", "ntfy.conf", "webhook.conf", "hook.conf", "routes.conf", "templates.conf", "container.conf"}

// exampleFiles are the secret files, the template file and the hook the
// examples name, as --check-config reads them.
var exampleFiles = fstest.MapFS{
	"etc/mailcrier.d/telegram.token": {Data: []byte("123456:REPLACE-ME\n")},
	"etc/mailcrier.d/slack.token":    {Data: []byte("xoxb-REPLACE-ME\n")},
	"etc/mailcrier.d/discord.url":    {Data: []byte("https://discord.com/api/webhooks/123/REPLACE-ME\n")},
	"etc/mailcrier.d/mattermost.url": {Data: []byte("https://mattermost.example.org/hooks/REPLACE-ME\n")},
	"etc/mailcrier.d/ntfy.url":       {Data: []byte("https://ntfy.sh/REPLACE-ME\n")},
	"etc/mailcrier.d/gotify.url":     {Data: []byte("gotify://gotify.example.org/REPLACE-ME\n")},
	"etc/mailcrier.d/api.tmpl":       {Data: []byte(`{"title": {{ toJson .Subject }}, "host": {{ toJson .Hostname }},` + "\n" + ` "from": {{ toJson .From.Addr }}, "text": {{ toJson (.Body | head 50) }}}` + "\n")},
	"run/secrets/telegram_token":     {Data: []byte("123456:REPLACE-ME\n")},
	"usr/local/bin/mailcrier-hook":   {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
}

// unfoldExample turns a configuration file written as comments into the
// configuration it shows: lines with "## " explain and are dropped, lines
// with "# " lose that prefix.
func unfoldExample(data []byte) []byte {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "##" || strings.HasPrefix(line, "## "):
		case line == "#":
			lines = append(lines, "")
		case strings.HasPrefix(line, "# "):
			lines = append(lines, line[2:])
		default:
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// checkExample runs --check-config on doc as /etc/mailcrier.conf, by a
// caller without the setgid bit, with exampleFiles beside it, and returns
// the exit status and stderr.
func checkExample(t *testing.T, doc []byte) (int, string) {
	t.Helper()
	fsys := fstest.MapFS{"etc/mailcrier.conf": {Data: doc}}
	for name, file := range exampleFiles {
		fsys[name] = file
	}
	var stdout, stderr bytes.Buffer
	deps := app.Deps{
		NewLogger:      func(string) *slog.Logger { return slog.New(slog.DiscardHandler) },
		ConfigFS:       fsys,
		ConfigPath:     "etc/mailcrier.conf",
		HTTP:           &http.Client{Transport: refusingTransport{}},
		Hostname:       "host1.example.org",
		Now:            func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) },
		Program:        "/usr/sbin/sendmail",
		Stdout:         &stdout,
		Stderr:         &stderr,
		SetLogOutput:   func(io.Writer) {},
		Credentials:    app.Credentials{UID: 1000, GID: 1000, EGID: 1000, ServiceUID: 990},
		LookupUserName: func(int) (string, bool) { return "", false },
	}
	code := app.Run(context.Background(), deps, []string{"--check-config"}, strings.NewReader(""))
	if stdout.Len() > 0 {
		t.Errorf("stdout %q, want none", stdout.String())
	}
	return code, stderr.String()
}

// refusingTransport fails every request: checking a configuration sends
// nothing.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("request during --check-config")
}

// TestPackageConfigExample loads the configuration file of the packages,
// which is all comments: the program must reject it for lack of targets,
// and for nothing else, until the administrator sets one; with the comment
// signs of its example lines removed it is a valid configuration.
func TestPackageConfigExample(t *testing.T) {
	t.Run("T-ADJ-54/config", func(t *testing.T) {
		data, err := os.ReadFile("packaging/mailcrier.conf")
		if err != nil {
			t.Fatal(err)
		}
		_, err = config.Load(fstest.MapFS{"etc/mailcrier.conf": {Data: data}}, "etc/mailcrier.conf")
		if err == nil || err.Error() != "/etc/mailcrier.conf: no targets configured" {
			t.Errorf("Load = %v, want /etc/mailcrier.conf: no targets configured", err)
		}
		if code, stderr := checkExample(t, unfoldExample(data)); code != 0 || stderr != checkClean {
			t.Errorf("--check-config of the unfolded file = %d, stderr\n%s", code, stderr)
		}
	})
}

// checkClean is the stderr of --check-config without findings.
const checkClean = "/etc/mailcrier.conf: 0 errors, 0 warnings\n"

// TestConfigExamplesLoad runs --check-config on every example of
// packaging/examples, the reference with its example lines unfolded: each
// must load, build its targets and render the sample message with every
// template without an error or a warning. A build without shoutrrr must
// reject a file with a shoutrrr target for that alone. Each example is
// installed by the packages.
func TestConfigExamplesLoad(t *testing.T) {
	paths, err := filepath.Glob("packaging/examples/*")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(configExamples))) {
		t.Fatalf("packaging/examples holds %v, configExamples lists %v", names, configExamples)
	}
	installed := map[string]string{}
	for _, content := range readNFPM(t).Contents {
		installed[content.Src] = content.Dst
	}
	for _, name := range configExamples {
		t.Run(name, func(t *testing.T) {
			src := "packaging/examples/" + name
			if dst := installed[src]; dst != "/usr/share/doc/mailcrier/examples/"+name {
				t.Errorf("%s is installed as %q, want /usr/share/doc/mailcrier/examples/%s", src, dst, name)
			}
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if name == "mailcrier.conf" {
				data = unfoldExample(data)
			}
			code, stderr := checkExample(t, data)
			if !hasShoutrrr && bytes.Contains(data, []byte(`type = "shoutrrr"`)) {
				if code != 78 || !strings.Contains(stderr, "built without shoutrrr") || strings.Count(stderr, "\n") != 2 {
					t.Errorf("--check-config = %d, stderr\n%s\nwant 78 and only built without shoutrrr", code, stderr)
				}
				return
			}
			if code != 0 || stderr != checkClean {
				t.Errorf("--check-config = %d, stderr\n%s", code, stderr)
			}
		})
	}
}
