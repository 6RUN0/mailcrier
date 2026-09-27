// Package message reads the mail a caller pipes into sendmail and exposes
// the parts a notification needs. It is a leaf of the package graph.
package message

import (
	"bytes"
	"fmt"
	"io"
	"net/mail"
)

// Message is one mail read from standard input.
type Message struct {
	// Subject is the raw Subject header value.
	Subject string
	// MessageID is the raw Message-ID header value; empty when absent.
	MessageID string
	// Body is everything after the header block.
	Body string
	// Raw is the input exactly as read.
	Raw []byte
}

// Read consumes r to the end and splits the mail into headers and body.
//
// Input that does not start with a valid header block is not rejected: the
// whole input becomes the body, because a notification with an odd layout
// is worth more to the operator than a lost one.
func Read(r io.Reader) (*Message, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	msg := &Message{Raw: raw}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		msg.Body = string(raw)
		return msg, nil
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		return nil, fmt.Errorf("read message body: %w", err)
	}
	msg.Subject = parsed.Header.Get("Subject")
	msg.MessageID = parsed.Header.Get("Message-Id")
	msg.Body = string(body)
	return msg, nil
}
