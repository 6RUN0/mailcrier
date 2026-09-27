// Package message reads the mail a caller pipes into sendmail and exposes
// the parts a notification needs. It is a leaf of the package graph.
package message

import (
	"bytes"
	"fmt"
	"io"
	"iter"
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
//
// The input is held once: line ends and dots are rewritten in place, the
// header block is scanned without splitting it into lines, and the body is
// one slice of the input.
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
	msg := &Message{Header: mail.Header{}, Size: int64(len(raw)) + discarded}
	raw = normalize(raw, opt.IgnoreDots)
	if bytes.HasPrefix(raw, []byte("From ")) {
		raw = raw[lineEnd(raw, 0):]
	}
	headerEnd, bodyStart, isMalformed := scanHeader(raw)
	if isMalformed {
		warnings = append(warnings, WarningMalformedHeader)
	}
	var from, bcc []Address
	for name, value := range fields(string(raw[:headerEnd])) {
		key := textproto.CanonicalMIMEHeaderKey(name)
		switch key {
		case "Bcc":
			bcc = append(bcc, ParseAddressList(value)...)
			continue
		case "From":
			if _, ok := msg.Header[key]; !ok {
				from = ParseAddressList(value)
			}
		case "To":
			msg.To = append(msg.To, ParseAddressList(value)...)
		case "Cc":
			msg.Cc = append(msg.Cc, ParseAddressList(value)...)
		}
		msg.Header[key] = append(msg.Header[key], value)
	}
	msg.Body = string(raw[bodyStart:])
	msg.Subject = msg.Header.Get("Subject")
	msg.MessageID = msg.Header.Get("Message-Id")
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

// normalize rewrites raw in place and returns the shortened slice: the CRs
// that end a line are removed, and unless ignoreDots is set, a line with a
// single dot ends the input and a line starting with two dots loses one,
// as sendmail 8 does: callers that pass no -i rely on it.
func normalize(raw []byte, ignoreDots bool) []byte {
	w := 0
	isLineStart := true
	for r := 0; r < len(raw); {
		if isLineStart && !ignoreDots && raw[r] == '.' {
			next := r + 1
			for next < len(raw) && raw[next] == '\r' {
				next++
			}
			if next == len(raw) || raw[next] == '\n' {
				return raw[:w]
			}
			if raw[r+1] == '.' {
				r++
			}
		}
		isLineStart = false
		switch c := raw[r]; c {
		case '\r':
			// A run of CRs is copied at once, so that a long run without
			// an LF is not scanned again for every CR in it.
			end := r
			for end < len(raw) && raw[end] == '\r' {
				end++
			}
			if end < len(raw) && raw[end] == '\n' {
				r = end
				continue
			}
			w += copy(raw[w:], raw[r:end])
			r = end
		case '\n':
			raw[w] = c
			w, r = w+1, r+1
			isLineStart = true
		default:
			raw[w] = c
			w, r = w+1, r+1
		}
	}
	return raw[:w]
}

// lineEnd returns the offset after the line of raw that starts at pos: past
// its LF, or the end of raw.
func lineEnd(raw []byte, pos int) int {
	if n := bytes.IndexByte(raw[pos:], '\n'); n >= 0 {
		return pos + n + 1
	}
	return len(raw)
}

// scanHeader returns where the header block of raw ends and where the body
// starts. The block ends before an empty line, which belongs to neither,
// before a malformed line, which starts the body, or at the end of the
// input. isMalformed reports a malformed line after at least one field;
// input whose first line is no field is a body without headers.
func scanHeader(raw []byte) (headerEnd, bodyStart int, isMalformed bool) {
	for pos := 0; pos < len(raw); pos = lineEnd(raw, pos) {
		switch c := raw[pos]; {
		case c == '\n':
			return pos, pos + 1, false
		case c == ' ' || c == '\t':
			if pos == 0 {
				return 0, 0, false
			}
		case !isFieldStart(raw[pos:]):
			return pos, pos, pos > 0
		}
	}
	return len(raw), len(raw), false
}

// isFieldStart reports whether line starts with a field name, printable
// ASCII other than the colon, directly followed by a colon.
func isFieldStart(line []byte) bool {
	for i, c := range line {
		switch {
		case c == ':':
			return i > 0
		case c <= ' ' || c > '~':
			return false
		}
	}
	return false
}

// fields yields the name and value of each field of a header block, the
// value with its continuation lines joined by one space and trimmed. A
// value on one line is a substring of block and costs no copy.
func fields(block string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for pos := 0; pos < len(block); {
			end := pos
			for {
				if n := strings.IndexByte(block[end:], '\n'); n >= 0 {
					end += n + 1
				} else {
					end = len(block)
				}
				if end == len(block) || (block[end] != ' ' && block[end] != '\t') {
					break
				}
			}
			name, value, _ := strings.Cut(block[pos:end], ":")
			pos = end
			if !yield(name, unfold(value)) {
				return
			}
		}
	}
}

// unfold joins the lines of a field value with one space, each line
// trimmed.
func unfold(value string) string {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "\n") {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for {
		line, rest, found := strings.Cut(value, "\n")
		b.WriteString(strings.TrimSpace(line))
		if !found {
			return b.String()
		}
		b.WriteByte(' ')
		value = rest
	}
}
