// Package message reads the mail a caller pipes into sendmail and exposes
// the parts a notification needs. It is a leaf of the package graph.
package message

import (
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
	"strings"
)

// MaxSize is the input limit Read is given in a real invocation: large
// enough for a rotated log mailed by logrotate, small enough to hold in
// memory.
const MaxSize = 10 << 20

// Warnings Read returns. They are constant texts, so that a log record
// quoting one carries no data from the message.
const (
	WarningTruncated       = "message over the size limit, rest discarded"
	WarningMalformedHeader = "malformed header line, taken as the start of the body"
	WarningControlAddress  = "address with a control character dropped"
)

// Address is one mailbox of an address header.
type Address struct {
	// Name is the display name or the comment; empty when absent.
	Name string
	// Addr is the address, which cron and sudo often send without a
	// domain ("root").
	Addr string
}

// Message is one mail read from standard input. It holds no Bcc: those
// addresses route the message and never reach its text.
type Message struct {
	// Header holds the header fields in canonical form, continuation lines
	// joined, Bcc removed.
	Header mail.Header
	// Subject and MessageID are the raw header values; empty when absent.
	Subject   string
	MessageID string
	// From is the first address of the From header.
	From Address
	// To and Cc are the addresses of all To and Cc headers.
	To, Cc []Address
	// Body is everything after the header block, with LF line ends.
	Body string
	// Size is the number of bytes read from the input, including any part
	// over the limit.
	Size int64
}

// Envelope is where a message goes, as opposed to what its headers say.
type Envelope struct {
	// Sender is the envelope sender; empty for the null sender.
	Sender string
	// SenderName is the full name from -F.
	SenderName string
	// Recipients are the addresses the message is for, Bcc included,
	// without duplicates.
	Recipients []string
}

// ReadOptions control how Read treats the input.
type ReadOptions struct {
	// IgnoreDots is sendmail's -i: without it a line with a single dot
	// ends the message and a leading dot of a line is removed.
	IgnoreDots bool
	// MaxSize is the number of bytes kept; the rest is read and
	// discarded, so that the caller does not get a broken pipe. Zero
	// keeps everything.
	MaxSize int64
}

// Read consumes r to the end and splits the mail into headers and body.
// It returns the message, the addresses of its Bcc headers, and warnings.
//
// Nothing short of a read error rejects the input: CRLF becomes LF, a
// leading mbox "From " line is dropped, input without a header block is
// all body, and a malformed header line starts the body, because a
// notification with an odd layout is worth more to the operator than a
// lost one.
func Read(r io.Reader, opt ReadOptions) (*Message, []Address, []string, error) {
	var warnings []string
	limited := r
	if opt.MaxSize > 0 {
		limited = io.LimitReader(r, opt.MaxSize)
	}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read message: %w", err)
	}
	discarded, err := io.Copy(io.Discard, r)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read message: %w", err)
	}
	if discarded > 0 {
		warnings = append(warnings, WarningTruncated)
	}
	lines := splitLines(raw, opt.IgnoreDots)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "From ") {
		lines = lines[1:]
	}
	msg := &Message{Header: mail.Header{}, Size: int64(len(raw)) + discarded}
	var bcc []Address
	headerEnd, isMalformed := scanHeader(lines)
	if isMalformed {
		warnings = append(warnings, WarningMalformedHeader)
	}
	for _, field := range unfold(lines[:headerEnd]) {
		name, value, _ := strings.Cut(field, ":")
		key := textproto.CanonicalMIMEHeaderKey(name)
		value = strings.TrimSpace(value)
		if key == "Bcc" {
			bcc = append(bcc, ParseAddressList(value)...)
			continue
		}
		msg.Header[key] = append(msg.Header[key], value)
	}
	body := lines[headerEnd:]
	if len(body) > 0 && body[0] == "\n" {
		body = body[1:]
	}
	msg.Body = strings.Join(body, "")
	msg.Subject = msg.Header.Get("Subject")
	msg.MessageID = msg.Header.Get("Message-Id")
	var from []Address
	if value, ok := msg.Header["From"]; ok {
		from = ParseAddressList(value[0])
	}
	for _, value := range msg.Header["To"] {
		msg.To = append(msg.To, ParseAddressList(value)...)
	}
	for _, value := range msg.Header["Cc"] {
		msg.Cc = append(msg.Cc, ParseAddressList(value)...)
	}
	isDropped := false
	for _, list := range []*[]Address{&from, &msg.To, &msg.Cc, &bcc} {
		if dropControlAddresses(list) {
			isDropped = true
		}
	}
	if isDropped {
		warnings = append(warnings, WarningControlAddress)
	}
	if len(from) > 0 {
		msg.From = from[0]
	}
	return msg, bcc, warnings, nil
}

// dropControlAddresses removes the addresses whose name or address holds
// a control character, such as a bare CR, and reports whether it removed
// any: the envelope must never carry a line break into a header.
func dropControlAddresses(list *[]Address) bool {
	kept := (*list)[:0]
	for _, a := range *list {
		if !hasControl(a.Name) && !hasControl(a.Addr) {
			kept = append(kept, a)
		}
	}
	isDropped := len(kept) != len(*list)
	*list = kept
	return isDropped
}

// hasControl reports whether s holds an ASCII control character.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f })
}

// splitLines returns the lines of raw with their LF ends, the CRs before
// an LF removed. Unless ignoreDots is set, a line with a single dot ends
// the input and a line starting with two dots loses one, as sendmail 8
// does: callers that pass no -i rely on it.
func splitLines(raw []byte, ignoreDots bool) []string {
	lines := strings.SplitAfter(string(raw), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		if content, ok := strings.CutSuffix(line, "\n"); ok {
			lines[i] = strings.TrimRight(content, "\r") + "\n"
		}
	}
	if ignoreDots {
		return lines
	}
	for i, line := range lines {
		content := strings.TrimSuffix(line, "\n")
		if content == "." || content == ".\r" {
			return lines[:i]
		}
		if strings.HasPrefix(line, "..") {
			lines[i] = line[1:]
		}
	}
	return lines
}

// scanHeader returns the number of lines of the header block, which ends
// before an empty line, before a malformed line, or at the end of the
// input. isMalformed reports a malformed line after at least one field;
// input whose first line is no field is a body without headers.
func scanHeader(lines []string) (end int, isMalformed bool) {
	for i, line := range lines {
		switch {
		case line == "\n":
			return i, false
		case line[0] == ' ' || line[0] == '\t':
			if i == 0 {
				return 0, false
			}
		case !isFieldStart(line):
			return i, i > 0
		}
	}
	return len(lines), false
}

// isFieldStart reports whether line starts with a field name, printable
// ASCII other than the colon, directly followed by a colon.
func isFieldStart(line string) bool {
	name, _, found := strings.Cut(line, ":")
	if !found || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] <= ' ' || name[i] > '~' {
			return false
		}
	}
	return true
}

// unfold joins each field of a header block with its continuation lines,
// one space between the parts. Each field is joined once: appending line
// by line would copy the field again for every continuation line.
func unfold(lines []string) []string {
	var fields, parts []string
	flush := func() {
		if len(parts) > 0 {
			fields = append(fields, strings.Join(parts, " "))
		}
	}
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if line[0] == ' ' || line[0] == '\t' {
			parts = append(parts, strings.TrimSpace(line))
			continue
		}
		flush()
		parts = []string{line}
	}
	flush()
	return fields
}
