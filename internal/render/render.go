// Package render turns a message into the text of one target with Go
// templates and fits that text into the target's length limit.
//
// The built-in templates live in defaults/, one per text.Format. A template
// sees Data, which never holds a Bcc address.
package render

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/text"
)

// Strings are the notices a template writes in place of missing content.
// Each can be overridden in the configuration.
type Strings struct {
	// NoSubject stands in for an empty Subject.
	NoSubject string
	// EmptyBody stands in for a body without visible text: services reject
	// an empty message.
	EmptyBody string
	// Truncated ends a body that Fit has cut.
	Truncated string
	// MoreAttachments follows the attachments Fit has listed when it left
	// out the rest; its one %d verb is the number left out.
	MoreAttachments string
	// NotSent follows an attachment listed in the text but not sent with
	// it, because it exceeds a file limit of the target.
	NotSent string
}

// DefaultStrings returns the English notices.
func DefaultStrings() Strings {
	return Strings{
		NoSubject:       "(no subject)",
		EmptyBody:       "(empty body)",
		Truncated:       "[truncated]",
		MoreAttachments: "... and %d more",
		NotSent:         "[not sent]",
	}
}

// Attachment describes an attachment to a template; its content is not
// part of the data.
type Attachment struct {
	Name        string
	ContentType string
	Size        int64
	// IsSkipped reports an attachment the target sends files but not this
	// one: it exceeds the size or number of files the target accepts.
	IsSkipped bool
}

// Data is what a template sees.
type Data struct {
	// Subject is the decoded Subject; empty when the message has none.
	Subject string
	// From is the From address, or the envelope sender when the message
	// has no From header.
	From message.Address
	// Sender and SenderName are the envelope sender and the -F name.
	Sender     string
	SenderName string
	// To and Cc are the addresses of the headers.
	To, Cc []message.Address
	// Recipients are the envelope recipients without the addresses that
	// only a Bcc or Resent-Bcc header named.
	Recipients []string
	// Date is the Date header, or the receive time.
	Date time.Time
	// ReceivedAt is the time slendmail read the message.
	ReceivedAt time.Time
	// MessageID is the Message-ID header.
	MessageID string
	// Headers are all header fields except Bcc and Resent-Bcc; Get, Values, Has and
	// Names ignore the case of the name.
	Headers message.Header
	// Body is the text of the message; BodyHTML its HTML part, if any.
	Body     string
	BodyHTML string
	// Attachments describe the attachments.
	Attachments []Attachment
	// MoreAttachments is the number of attachments Fit left out of
	// Attachments; 0 when it listed them all.
	MoreAttachments int
	// Hostname is the name of the machine.
	Hostname string
	// Target is the name of the target the text is for.
	Target string
	// Limit is the text limit of the target in its own unit; 0 when it
	// has none.
	Limit int
	// Strings are the notices for missing content.
	Strings Strings
}

// maxFromLength bounds the name and the address of Data.From in
// characters. Fit cuts subject, body and attachments but not the sender,
// so an endless display name in From would leave no room for the rest,
// and a strict format would lose the message to ErrLimitTooSmall. The
// host name needs no bound: the kernel keeps it to 64 bytes.
const maxFromLength = 256

// NewData returns the data of msg for a template. The blind copies route
// the message but appear nowhere in the data, not even among the
// recipients, unless To, Cc, Resent-To or Resent-Cc name them too. Name
// and address of From are cut to maxFromLength characters at a word. The
// caller sets Hostname, ReceivedAt, Target and Limit.
func NewData(msg *message.Message, env message.Envelope, blind message.BlindCopies) Data {
	visible := map[string]bool{}
	for _, list := range [][]message.Address{msg.To, msg.Cc, msg.ResentTo, msg.ResentCc} {
		for _, a := range list {
			visible[a.Addr] = true
		}
	}
	hidden := map[string]bool{}
	for _, list := range [][]message.Address{blind.Bcc, blind.ResentBcc} {
		for _, a := range list {
			if !visible[a.Addr] {
				hidden[a.Addr] = true
			}
		}
	}
	var recipients []string
	for _, r := range env.Recipients {
		if !hidden[r] {
			recipients = append(recipients, r)
		}
	}
	from := msg.From
	if from == (message.Address{}) {
		from = message.Address{Name: env.SenderName, Addr: env.Sender}
	}
	from.Name, from.Addr = text.CutAtWord(from.Name, maxFromLength), text.CutAtWord(from.Addr, maxFromLength)
	attachments := make([]Attachment, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		attachments = append(attachments, Attachment{Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	return Data{
		Subject:     msg.Subject,
		From:        from,
		Sender:      env.Sender,
		SenderName:  env.SenderName,
		To:          msg.To,
		Cc:          msg.Cc,
		Recipients:  recipients,
		Date:        msg.Date,
		MessageID:   msg.MessageID,
		Headers:     msg.Header,
		Body:        msg.Body,
		BodyHTML:    msg.BodyHTML,
		Attachments: attachments,
		Strings:     DefaultStrings(),
	}
}

// Template is a parsed template with the functions of this package.
type Template struct {
	tmpl *template.Template
	// format is the markup of the target the template writes for; Fit
	// takes its strictness from it, whoever wrote the template.
	format text.Format
}

//go:embed defaults/*.tmpl
var defaults embed.FS

// Builtin returns the built-in template of format.
func Builtin(format text.Format) (*Template, error) {
	source, err := defaults.ReadFile("defaults/" + string(format) + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("no built-in template for format %q", format)
	}
	return parse(string(format), strings.TrimSuffix(string(source), "\n"), format)
}

// parse parses source with the functions of this package, for a target
// whose markup is format.
func parse(name, source string, format text.Format) (*Template, error) {
	tmpl, err := template.New(name).Funcs(funcs()).Parse(source)
	if err != nil {
		return nil, err
	}
	return &Template{tmpl: tmpl, format: format}, nil
}

// Execute renders d.
func (t *Template) Execute(d Data) (string, error) {
	var b strings.Builder
	if err := t.tmpl.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// funcs returns the template functions.
func funcs() template.FuncMap {
	return template.FuncMap{
		"tgHTML":        text.EscapeTelegramHTML,
		"tgMDv2":        text.EscapeTelegramMarkdownV2,
		"tgMDv2Code":    text.EscapeTelegramMarkdownV2Code,
		"slackEscape":   text.EscapeSlack,
		"slackCode":     func(s string) string { return text.EscapeSlack(text.EscapeCodeBlock(s)) },
		"discordEscape": text.EscapeDiscord,
		"discordCode":   text.EscapeCodeBlock,
		"mmEscape":      text.EscapeMattermostMarkdown,
		"mmCode":        func(s string) string { return text.EscapeMattermost(text.EscapeCodeBlock(s)) },
		"trimEnd":       func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) },
		"toJson":        toJSON,
		"default":       defaultValue,
		"humanizeBytes": humanizeBytes,
	}
}

// toJSON encodes v as JSON without escaping <, > and &. Invalid UTF-8
// becomes U+FFFD and control characters are escaped, so the result is
// always valid JSON.
func toJSON(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// defaultValue returns value, or fallback when value is empty: a string of
// white space only, nil, or the zero value of its type.
func defaultValue(fallback, value any) any {
	if s, ok := value.(string); ok {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	}
	v := reflect.ValueOf(value)
	switch {
	case !v.IsValid(), v.IsZero():
		return fallback
	case (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.Len() == 0:
		return fallback
	}
	return value
}

// humanizeBytes returns a size in bytes with a binary unit: "512 B",
// "1.5 KiB", "20.0 MiB".
func humanizeBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/1024)
}
