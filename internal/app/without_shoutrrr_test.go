//go:build noshoutrrr

package app

import (
	"strings"
	"testing"
)

// shoutrrrRejection is the log text for any shoutrrr target in a build
// without the library.
const shoutrrrRejection = `target \"bus\": built without shoutrrr`

// TestRunRejectsShoutrrrWithoutLibrary pins that a build without shoutrrr
// rejects a valid shoutrrr target as a configuration error.
func TestRunRejectsShoutrrrWithoutLibrary(t *testing.T) {
	inv := &invocation{config: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"generic://hooks.example.org/in\"\n", stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 78 {
		t.Fatalf("Run() = %d, want 78", code)
	}
	if log := inv.log("mailcrier"); !strings.Contains(log, shoutrrrRejection) {
		t.Errorf("log lacks %q:\n%s", shoutrrrRejection, log)
	}
}

// shoutrrrTargetConfig is empty: a build without the library rejects any
// shoutrrr target.
const shoutrrrTargetConfig = ""
