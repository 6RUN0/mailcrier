package app

import (
	"bytes"
	"fmt"
	"time"

	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
)

// sampleMessage returns the message --probe sends and --check-config
// renders. It has no From header: the sender from the environment could
// carry line breaks into the header block, so the sender goes in the
// envelope only.
func sampleMessage(d Deps) []byte {
	return fmt.Appendf(nil, "To: root\nSubject: slendmail probe from %s\nDate: %s\nContent-Type: text/plain; charset=utf-8\n\n"+
		"Test message from slendmail %s on %s.\nIt was sent by slendmail --probe to the targets of the configuration.\n",
		d.Hostname, d.Now().Format(time.RFC1123Z), buildVersion(), d.Hostname)
}

// readSample reads sampleMessage the way a message from stdin is read and
// returns it with its envelope, for root, and its template data, the
// notices of strs in place.
func readSample(d Deps, strs render.Strings) (*message.Message, message.Envelope, render.Data, error) {
	receivedAt := d.Now()
	msg, bcc, _, err := message.Read(bytes.NewReader(sampleMessage(d)), message.ReadOptions{IgnoreDots: true, MaxSize: message.MaxSize, ReceivedAt: receivedAt})
	if err != nil {
		return nil, message.Envelope{}, render.Data{}, fmt.Errorf("sample message not read: %w", err)
	}
	env := message.Envelope{Sender: defaultSender(d), Recipients: []string{"root"}}
	data := render.NewData(msg, env, bcc)
	data.Hostname, data.ReceivedAt, data.Strings = d.Hostname, receivedAt, strs
	return msg, env, data, nil
}
