package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// selinuxPorts are the targets of testdata/selinux/check.sh: http on 80,
// 8080 (http_cache_port_t) and 2586 (unreserved_port_t), https by name on
// 8443.
var selinuxPorts = []string{"80", "8080", "2586", "8443"}

// selinuxWaits are the waits of check.sh; each must end with "ok", or the
// caller it waits for never delivered. The control waits are those of
// postfix: without them its denials are no measure.
func selinuxWaits() []string {
	waits := []string{"control user-cron", "control system-cron", "control smartd", "control nnp", "control service"}
	for _, caller := range []string{"root", "service", "nnp", "smartd", "user-cron", "system-cron", "at", "queued", "upgrade"} {
		for _, port := range selinuxPorts {
			waits = append(waits, caller+" "+port)
		}
	}
	for _, caller := range []string{"root", "service", "nnp", "smartd", "user-crontab", "cron-mail", "queued"} {
		waits = append(waits, "hook "+caller)
	}
	return waits
}

// selinuxDomains are the processes whose domain makes an empty search
// meaningful, with the domains each may run in: a daemon started outside
// them would call mailcrier from another one. The policy of Rocky 9 labels
// /usr/sbin/atd crond_exec_t, so atd runs in crond_t there.
var selinuxDomains = map[string][]string{
	"crond":  {"crond_t"},
	"atd":    {"atd_t", "crond_t"},
	"smartd": {"fsdaemon_t"},
}

// hookDomains are the domains the hook runs in per subject of its message,
// as measured on Rocky 9.8 and 10.2: calls of an administrator and of a
// user crontab stay in unconfined_t, the mail of crond and of smartd moves
// into system_mail_t through sendmail_exec_t, the queue service and a
// service without NoNewPrivileges into sendmail_t; under NoNewPrivileges the
// transition is refused and the call stays in initrc_t, as with postfix.
// The queue service runs as the user mailcrier, so its hook has the group
// mailcrier (README, "Ids" of the exec target); every other hook must not.
var hookDomains = []struct {
	subject, domain string
	isService       bool
}{
	{"root-call", "unconfined_t", false},
	{"user-crontab-hook", "unconfined_t", false},
	{"service-call", "sendmail_t", false},
	{"queued-call", "sendmail_t", true},
	{"nnp-call", "initrc_t", false},
	{"EmailTest", "system_mail_t", false},
	{"cron-mail-hook", "system_mail_t", false},
}

// mailcrierComms are the comm values of the callers of the check: the
// binary is called as sendmail, runs as exe after the re-exec through
// /proc/self/exe, smartd mails through mail of s-nail, and the hook runs as
// selinux-hook and curl. A denial of another process is reported, not
// failed: with the dontaudit rules off, dnf, sshd and systemd give denials
// of their own.
var mailcrierComms = []string{"mailcrier", "sendmail", "exe", "selinux-hook", "curl", "mail", "s-nail"}

// mailcrierObjects are substrings of the paths and executables of
// mailcrier and of the check.
var mailcrierObjects = []string{"mailcrier", "/usr/sbin/sendmail", "selinux-hook"}

// selinuxRun is the record of one run of check.sh: its "== ... ==" lines
// and the audit records of its searches.
type selinuxRun struct {
	marks          []string
	control        []string
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
			case "control denials":
				section = &run.control
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

// values returns the rest of every mark that starts with prefix and a
// space.
func (r selinuxRun) values(prefix string) []string {
	var found []string
	for _, mark := range r.marks {
		if rest, ok := strings.CutPrefix(mark, prefix+" "); ok {
			found = append(found, rest)
		}
	}
	return found
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

// avcLine matches the AVC line of an interpreted audit record: the
// permissions, then the types of the contexts and the class.
var avcLine = regexp.MustCompile(`avc:\s+denied\s+\{ ([^}]*) \}.* scontext=[^: ]*:[^: ]*:(\w+):\S* tcontext=[^: ]*:[^: ]*:(\w+):\S* tclass=(\w+)`)

// denialKeys returns "source target class permission" for each permission
// an audit record denies; a record without an AVC line gives none.
func denialKeys(record string) []string {
	var keys []string
	for _, match := range avcLine.FindAllStringSubmatch(record, -1) {
		for _, permission := range strings.Fields(match[1]) {
			keys = append(keys, strings.Join([]string{match[2], match[3], match[4], permission}, " "))
		}
	}
	return keys
}

// checkSELinuxRun returns what the output of check.sh shows wrong, and for
// a person to look at the denials of other processes and those of
// mailcrier that postfix gets from the same callers.
func checkSELinuxRun(output string) (problems, others []string) {
	run := parseSELinuxRun(output)
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !slices.Contains(run.marks, "end") {
		step := "none"
		for _, mark := range run.marks {
			if rest, ok := strings.CutPrefix(mark, "step "); ok {
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
	expectType := func(key, want string) {
		if got, _ := run.value(key); !strings.Contains(got, ":"+want+":") {
			add("%s = %q, want the type %s", key, got, want)
		}
	}
	if context, _ := run.value("context"); !strings.HasPrefix(context, "unconfined_u:unconfined_r:unconfined_t:") {
		add("check did not run in unconfined_t: %q", context)
	}
	expect("enforce", "Enforcing")
	expect("auditd", "active")
	expect("timer", "enabled active")
	expect("timer after upgrade", "enabled")
	expect("call upgrade: exit", "0")
	expect("call root: exit", "0")
	expect("call queued: exit", "0")
	expect("queue service", "success")
	expect("module", "200 cil")
	expect("module after erase:", "absent")
	expectType("label binary", "sendmail_exec_t")
	expectType("label spool", "mqueue_spool_t")
	expectType("label entry", "mqueue_spool_t")
	if !slices.Contains(run.marks, "smart available") {
		add("the IDE disk of the machine shows no SMART, so smartd was not checked")
	}
	for name, domains := range selinuxDomains {
		label, _ := run.value("domain " + name)
		if fields := strings.Split(label, ":"); len(fields) < 3 || !slices.Contains(domains, fields[2]) {
			add("%s runs in %q, want one of %v", name, label, domains)
		}
	}
	for _, name := range []string{"control-nnp", "control-service", "nnp-call", "service-call"} {
		record, ok := run.value("unit " + name)
		fields := strings.Fields(record)
		switch {
		case !ok || len(fields) < 2:
			add("no record of the unit %s", name)
		case fields[len(fields)-1] != "0":
			add("the call of the unit %s exited %s", name, fields[len(fields)-1])
		}
	}
	for _, name := range selinuxWaits() {
		expect("wait "+name+":", "ok")
	}
	gid, _ := run.value("mailcrier gid")
	hooks := run.values("hook")
	for _, want := range hookDomains {
		i := slices.IndexFunc(hooks, func(hook string) bool {
			subject, _, _ := strings.Cut(hook, " ")
			return strings.Contains(subject, want.subject)
		})
		if i < 0 {
			add("no hook ran for %s", want.subject)
			continue
		}
		fields := strings.Fields(hooks[i])
		if len(fields) != 3 || !strings.Contains(fields[1], ":"+want.domain+":") {
			add("the hook for %s ran as %q, want the domain %s", want.subject, hooks[i], want.domain)
			continue
		}
		if hasGroup := slices.Contains(strings.Split(fields[2], ","), gid); gid == "" || hasGroup != want.isService {
			add("the hook for %s ran with groups %s, the group mailcrier is %q, want it: %v", want.subject, fields[2], gid, want.isService)
		}
	}
	if !slices.Contains(run.marks, "policy load found") {
		add("no MAC_POLICY_LOAD record since the start: auditd or ausearch -ts did not work, an empty search proves nothing")
	}
	var control []string
	for _, record := range run.control {
		control = append(control, denialKeys(record)...)
	}
	judge := func(record, what string) {
		keys := denialKeys(record)
		if len(keys) == 0 {
			add("%s:\n%s", what, record)
			return
		}
		for _, key := range keys {
			if !slices.Contains(control, key) {
				add("%s, not among those of postfix (%s):\n%s", what, key, record)
				return
			}
		}
		others = append(others, record)
	}
	for _, record := range run.denials {
		if isMailcrierDenial(record) {
			judge(record, "denial of mailcrier")
		} else {
			others = append(others, record)
		}
	}
	for _, record := range run.mailcrierExecs {
		if !slices.Contains(run.denials, record) {
			judge(record, "denial of /usr/sbin/mailcrier")
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
	const crondMail = "type=AVC msg=audit(10/09/26 12:00:01.123:456) : avc:  denied  { read write } for  pid=4242 comm=sendmail path=socket:[1234] dev=\"sockfs\" ino=1234 scontext=system_u:system_r:system_mail_t:s0-s0:c0.c1023 tcontext=system_u:system_r:init_t:s0 tclass=unix_stream_socket permissive=0"
	const spool = "type=AVC msg=audit(10/09/26 12:00:02.123:457) : avc:  denied  { write } for  pid=4243 comm=sendmail name=tmp dev=\"vda4\" ino=99 scontext=system_u:system_r:system_mail_t:s0 tcontext=system_u:object_r:var_spool_t:s0 tclass=dir permissive=0"
	const other = "type=AVC msg=audit(10/09/26 12:00:03.123:458) : avc:  denied  { search } for  pid=77 comm=sshd name=root dev=\"vda4\" ino=2 scontext=system_u:system_r:sshd_t:s0-s0:c0.c1023 tcontext=system_u:object_r:admin_home_t:s0 tclass=dir permissive=0"
	section := func(output, title string, records ...string) string {
		return strings.Replace(output, "== "+title+" ==\n<no matches>\n", "== "+title+" ==\n----\n"+strings.Join(records, "\n----\n")+"\n", 1)
	}
	withControl := section(clean, "control denials", crondMail)
	for _, c := range []struct {
		name, output string
		problem      string
		others       int
	}{
		{"clean", clean, "", 0},
		{"other process denied", section(clean, "denials", other), "", 1},
		{"denial postfix gets too", section(withControl, "denials", crondMail), "", 1},
		{"denial postfix does not get", section(clean, "denials", crondMail), "not among those of postfix (system_mail_t init_t unix_stream_socket read)", 0},
		{"spool denied", section(withControl, "denials", spool), "(system_mail_t var_spool_t dir write)", 0},
		{"not unconfined", strings.Replace(clean, "context unconfined_u:unconfined_r:unconfined_t:", "context system_u:system_r:cloud_init_t:", 1), "not run in unconfined_t", 0},
		{"no policy load", strings.Replace(clean, "== policy load found ==", "== policy load missing ==", 1), "MAC_POLICY_LOAD", 0},
		{"wait timed out", strings.Replace(clean, "== wait at 8443: ok ==", "== wait at 8443: timeout ==", 1), `wait at 8443: = "timeout"`, 0},
		{"control timed out", strings.Replace(clean, "== wait control smartd: ok ==", "== wait control smartd: timeout ==", 1), "wait control smartd:", 0},
		{"no smart", strings.Replace(clean, "== smart available ==", "== smart unavailable ==", 1), "no SMART", 0},
		{"atd outside its domains", strings.Replace(clean, "domain atd system_u:system_r:crond_t:s0-s0:c0.c1023", "domain atd system_u:system_r:unconfined_service_t:s0", 1), "atd runs in", 0},
		{"binary unlabelled", strings.Replace(clean, "label binary system_u:object_r:sendmail_exec_t:s0", "label binary system_u:object_r:bin_t:s0", 1), "want the type sendmail_exec_t", 0},
		{"module left", strings.Replace(clean, "module after erase: absent", "module after erase: present", 1), "module after erase:", 0},
		{"smartd hook unconfined", strings.Replace(clean, "_selinux system_u:system_r:system_mail_t:s0", "_selinux system_u:system_r:fsdaemon_t:s0", 1), "want the domain system_mail_t", 0},
		{"hook kept the group", strings.Replace(clean, "hook service-call system_u:system_r:sendmail_t:s0 0", "hook service-call system_u:system_r:sendmail_t:s0 0,994", 1), "ran with groups 0,994", 0},
		{"queue hook without the group", strings.Replace(clean, "hook queued-call system_u:system_r:sendmail_t:s0 994", "hook queued-call system_u:system_r:sendmail_t:s0 1001", 1), "ran with groups 1001", 0},
		{"unit call failed", strings.Replace(clean, "unit nnp-call system_u:system_r:initrc_t:s0 0", "unit nnp-call system_u:system_r:initrc_t:s0 1", 1), "nnp-call exited 1", 0},
		{"stopped", clean[:strings.Index(clean, "== step 4 ==")] + "== failed: mailcrier --check-config ==\n== cleanup ==\n", "failed in step 3: mailcrier --check-config", 0},
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
