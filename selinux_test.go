//go:build selinux

package main

import (
	"flag"
	"os"
	"testing"
)

var selinuxLog = flag.String("selinux-log", "", "check.log of a run of make selinux-<distro>")

// TestSELinux reads the output of testdata/selinux/check.sh on a Rocky
// virtual machine with SELinux enforcing (make selinux-<distro>) and fails
// on a denial of mailcrier or of its callers, or when the run cannot show
// one: a caller that never delivered, a daemon outside its domain, no audit
// record since the start.
func TestSELinux(t *testing.T) {
	data, err := os.ReadFile(*selinuxLog)
	if err != nil {
		t.Fatalf("no output of check.sh, run make selinux-<distro>: %v", err)
	}
	t.Run("T-PKG-22/no-denials-of-mailcrier", func(t *testing.T) {
		problems, others := checkSELinuxRun(string(data))
		for _, record := range others {
			t.Logf("denial of another process, for the record under Result:\n%s", record)
		}
		for _, problem := range problems {
			t.Error(problem)
		}
	})
}
