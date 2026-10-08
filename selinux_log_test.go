package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// selinuxWaits are the waits of testdata/selinux/check.sh; each must end
// with "ok", or the caller it waits for never delivered.
var selinuxWaits = []string{"root", "user-cron", "hook", "system-cron", "at", "queued", "smartd"}

// selinuxDomains are the processes whose domain makes an empty search
// meaningful, with the domains each may run in: a daemon started outside
// them would call mailcrier from another one. The policy of Rocky 9 labels
// /usr/sbin/atd crond_exec_t, so atd runs in crond_t there.
var selinuxDomains = map[string][]string{
	"crond":  {"crond_t"},
	"atd":    {"atd_t", "crond_t"},
	"smartd": {"fsdaemon_t"},
}

// mailcrierComms are the comm values of the callers of the check: the
// binary is called as sendmail, runs as exe after the re-exec through
// /proc/self/exe, smartd mails through mail of s-nail, and the hook runs as
// selinux-hook. A denial of another process is reported, not failed:
// with the dontaudit rules off, dnf, sshd and systemd give denials of
// their own.
var mailcrierComms = []string{"mailcrier", "sendmail", "exe", "selinux-hook", "mail", "s-nail"}

// mailcrierObjects are substrings of the paths and executables of
// mailcrier and of the check.
var mailcrierObjects = []string{"mailcrier", "/usr/sbin/sendmail", "selinux-hook"}

// selinuxRun is the record of one run of check.sh: its "== ... ==" lines
// and the audit records of step 10.
type selinuxRun struct {
	marks          []string
	denials        []string
	mailcrierExecs []string
}

// parseSELinuxRun reads the output of check.sh.
func parseSELinuxRun(output string) selinuxRun {
	var run selinuxRun
	var section *[]string
	var record strings.Builder
	flush := func() {
		if text := strings.TrimSpace(record.String()); section != nil && text != "" {
			*section = append(*section, text)
		}
		record.Reset()
	}
	for line := range strings.Lines(output) {
		line = strings.TrimRight(line, "\r\n")
		if mark, ok := strings.CutPrefix(line, "== "); ok && strings.HasSuffix(mark, " ==") {
			flush()
			mark = strings.TrimSuffix(mark, " ==")
			run.marks = append(run.marks, mark)
			switch mark {
			case "denials":
				section = &run.denials
			case "denials of mailcrier":
				section = &run.mailcrierExecs
			default:
				section = nil
			}
			continue
		}
		if section == nil || line == "<no matches>" {
			continue
		}
		if line == "----" {
			flush()
			continue
		}
		record.WriteString(line + "\n")
	}
	flush()
	return run
}

// value returns the rest of the first mark that starts with prefix and a
// space.
func (r selinuxRun) value(prefix string) (string, bool) {
	for _, mark := range r.marks {
		if rest, ok := strings.CutPrefix(mark, prefix+" "); ok {
			return rest, true
		}
	}
	return "", false
}

// isMailcrierDenial tells whether an audit record names a process or an
// object of mailcrier or of its callers in the check.
func isMailcrierDenial(record string) bool {
	for _, field := range strings.Fields(record) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		if key == "comm" && slices.Contains(mailcrierComms, value) {
			return true
		}
		if slices.ContainsFunc(mailcrierObjects, func(object string) bool { return strings.Contains(value, object) }) {
			return true
		}
	}
	return false
}

// checkSELinuxRun returns what the output of check.sh shows wrong, and the
// denials of other processes for a person to look at.
func checkSELinuxRun(output string) (problems, others []string) {
	run := parseSELinuxRun(output)
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !slices.Contains(run.marks, "end") {
		step := "none"
		for _, mark := range run.marks {
			if rest, ok := strings.CutPrefix(mark, "step "); ok && rest != "11" {
				step = rest
			}
			if rest, ok := strings.CutPrefix(mark, "failed: "); ok {
				add("check.sh failed in step %s: %s", step, rest)
			}
		}
		add("check.sh did not reach the end, last step %s", step)
		return problems, nil
	}
	expect := func(key, want string) {
		if got, ok := run.value(key); !ok || got != want {
			add("%s = %q, want %q", key, got, want)
		}
	}
	if context, _ := run.value("context"); !strings.HasPrefix(context, "unconfined_u:unconfined_r:unconfined_t:") {
		add("check did not run in unconfined_t: %q", context)
	}
	expect("enforce", "Enforcing")
	expect("auditd", "active")
	expect("timer", "enabled active")
	expect("call root: exit", "0")
	expect("call queued: exit", "0")
	expect("queue service", "success")
	if !slices.Contains(run.marks, "smart available") {
		add("the IDE disk of the machine shows no SMART, so smartd was not checked")
	}
	for name, domains := range selinuxDomains {
		label, _ := run.value("domain " + name)
		if fields := strings.Split(label, ":"); len(fields) < 3 || !slices.Contains(domains, fields[2]) {
			add("%s runs in %q, want one of %v", name, label, domains)
		}
	}
	for _, name := range selinuxWaits {
		expect("wait "+name+":", "ok")
	}
	gid, _ := run.value("mailcrier gid")
	groups, hasHook := run.value("hook groups")
	switch {
	case !hasHook:
		add("the hook recorded no groups")
	case gid == "" || slices.Contains(strings.Fields(groups), gid):
		add("the hook ran with groups %q, the group mailcrier is %q", groups, gid)
	}
	if !slices.Contains(run.marks, "policy load found") {
		add("no MAC_POLICY_LOAD record since the start: auditd or ausearch -ts did not work, an empty search proves nothing")
	}
	for _, record := range run.denials {
		if isMailcrierDenial(record) {
			add("denial of mailcrier:\n%s", record)
		} else {
			others = append(others, record)
		}
	}
	for _, record := range run.mailcrierExecs {
		if !slices.Contains(run.denials, record) {
			add("denial of /usr/sbin/mailcrier:\n%s", record)
		}
	}
	return problems, others
}

// TestCheckSELinuxRun pins the verdict on the output of check.sh against
// the record of a clean run and variants of it, so the reading of the log
// is checked without a virtual machine.
func TestCheckSELinuxRun(t *testing.T) {
	data, err := os.ReadFile("testdata/selinux/clean.log")
	if err != nil {
		t.Fatal(err)
	}
	clean := string(data)
	const mailcrierDenial = "type=AVC msg=audit(10/08/26 12:00:01.123:456) : avc:  denied  { read } for  pid=4242 comm=sendmail name=mailcrier.conf dev=\"vda4\" ino=1234 scontext=unconfined_u:unconfined_r:cronjob_t:s0 tcontext=system_u:object_r:etc_t:s0 tclass=file permissive=0"
	const otherDenial = "type=AVC msg=audit(10/08/26 12:00:02.123:457) : avc:  denied  { search } for  pid=77 comm=sshd name=root dev=\"vda4\" ino=2 scontext=system_u:system_r:sshd_t:s0-s0:c0.c1023 tcontext=system_u:object_r:admin_home_t:s0 tclass=dir permissive=0"
	withDenials := func(records ...string) string {
		return strings.Replace(clean, "== denials ==\n<no matches>\n", "== denials ==\n----\n"+strings.Join(records, "\n----\n")+"\n", 1)
	}
	for _, c := range []struct {
		name, output string
		problem      string
		others       int
	}{
		{"clean", clean, "", 0},
		{"other process denied", withDenials(otherDenial), "", 1},
		{"mailcrier denied", withDenials(otherDenial, mailcrierDenial), "denial of mailcrier", 1},
		{"not unconfined", strings.Replace(clean, "context unconfined_u:unconfined_r:unconfined_t:", "context system_u:system_r:cloud_init_t:", 1), "not run in unconfined_t", 0},
		{"no policy load", strings.Replace(clean, "== policy load found ==", "== policy load missing ==", 1), "MAC_POLICY_LOAD", 0},
		{"wait timed out", strings.Replace(clean, "== wait at: ok ==", "== wait at: timeout ==", 1), `wait at: = "timeout"`, 0},
		{"atd outside its domains", strings.Replace(clean, "domain atd system_u:system_r:crond_t:s0-s0:c0.c1023", "domain atd system_u:system_r:unconfined_service_t:s0", 1), "atd runs in", 0},
		{"no smart", strings.Replace(clean, "== smart available ==", "== smart unavailable ==", 1), "no SMART", 0},
		{"hook kept the group", strings.Replace(clean, "== hook groups 1000 ==", "== hook groups 1000 993 ==", 1), "the hook ran with groups", 0},
		{"stopped", clean[:strings.Index(clean, "== step 4 ==")] + "== failed: mailcrier --check-config ==\n== step 11 ==\n", "failed in step 3: mailcrier --check-config", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			problems, others := checkSELinuxRun(c.output)
			text := strings.Join(problems, "\n")
			if c.problem == "" && len(problems) != 0 || c.problem != "" && !strings.Contains(text, c.problem) {
				t.Errorf("problems:\n%s\nwant %q", text, c.problem)
			}
			if len(others) != c.others {
				t.Errorf("others = %q, want %d", others, c.others)
			}
		})
	}
}
