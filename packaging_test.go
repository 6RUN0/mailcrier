package main

import (
	"os"
	"strings"
	"testing"
)

// TestQueueRunnerFiles pins the periodic queue run of the packages: every
// 5 minutes as the slendmail user, whose run takes the entries of all
// users; cron with MAILTO empty, because -q reports to syslog and a mail
// from cron would come back through this very program, and without quotes
// in the BusyBox crontab, whose crond takes MAILTO="" for an address and
// mails the output through sendmail; the cron.d line
// only where systemd is not running, so that the timer and cron do not
// both run the queue; the service with a time limit, so that a run that
// hangs does not keep the timer from starting the next one.
func TestQueueRunnerFiles(t *testing.T) {
	cases := []struct {
		path  string
		lines []string
	}{
		{"packaging/systemd/slendmail-queue.service", []string{"Type=oneshot", "User=slendmail", "Group=slendmail", "ExecStart=/usr/sbin/slendmail -q", "TimeoutStartSec=3min"}},
		{"packaging/systemd/slendmail-queue.timer", []string{"OnBootSec=2min", "OnUnitActiveSec=5min", "WantedBy=timers.target"}},
		{"packaging/cron/slendmail", []string{`MAILTO=""`, "*/5 * * * * slendmail [ -d /run/systemd/system ] || /usr/sbin/slendmail -q"}},
		{"packaging/cron/crontabs-slendmail", []string{"MAILTO=", "*/5 * * * * /usr/sbin/slendmail -q"}},
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
