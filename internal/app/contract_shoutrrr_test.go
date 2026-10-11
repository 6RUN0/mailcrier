//go:build !noshoutrrr

package app

import (
	"net/url"
	"testing"
)

// TestContractShoutrrrNtfy delivers the contract message through the ntfy
// service of shoutrrr: one message without title whose text is the plain
// template, subject and body unescaped, and no file.
func TestContractShoutrrrNtfy(t *testing.T) {
	t.Parallel()
	base := contractServer(t, contractNtfyEnv)
	server, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	topic := contractName("mailcrier-")
	runContract(t, "[target.bus]\ntype = \"shoutrrr\"\nurl = \"ntfy://"+server.Host+"/"+topic+"?scheme=http\"\n")

	messages := pollNtfy(t, base, topic)
	if len(messages) != 1 {
		t.Fatalf("topic holds %d messages, want 1: %+v", len(messages), messages)
	}
	m := messages[0]
	if m.Title != "" {
		t.Errorf("title = %q, want none", m.Title)
	}
	want := contractSubject + "\nhost1.example.org: cron@example.org\n\n" + contractBody + "\n" + contractAttachmentLine
	if m.Message != want {
		t.Errorf("message = %q, want %q", m.Message, want)
	}
	if m.Attachment != nil {
		t.Errorf("message has an attachment: %+v", *m.Attachment)
	}
}
