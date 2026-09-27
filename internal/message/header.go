package message

import (
	"bytes"
	"io"
	"mime"
	"net/textproto"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Header maps a canonical field name, as textproto.CanonicalMIMEHeaderKey
// returns it, to the values of all fields of that name in message order.
// Values have their continuation lines joined and encoded words decoded.
type Header map[string][]string

// Get returns the first value of the field name, in any case; empty when
// the message has none.
func (h Header) Get(name string) string {
	if values := h[textproto.CanonicalMIMEHeaderKey(name)]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// Values returns all values of the field name, in any case.
func (h Header) Values(name string) []string {
	return h[textproto.CanonicalMIMEHeaderKey(name)]
}

// Has reports whether the message has a field name, in any case.
func (h Header) Has(name string) bool {
	_, ok := h[textproto.CanonicalMIMEHeaderKey(name)]
	return ok
}

// Names returns the canonical names of the fields, sorted.
func (h Header) Names() []string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// wordDecoder decodes RFC 2047 encoded words in any charset decodeText
// knows.
var wordDecoder = &mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(decodeText(data, charset)), nil
	},
}

// builtinWordCharsets are the charsets mime.WordDecoder decodes without
// CharsetReader, us-ascii with 8-bit bytes as U+FFFD and iso-8859-1 as
// Latin-1, each with an alias that decodeText reads as it reads a body.
var builtinWordCharsets = [...]struct{ name, alias string }{
	{"us-ascii", "ascii"}, {"iso-8859-1", "latin1"},
}

// aliasWordCharsets returns s with the charset of each encoded word that
// is in builtinWordCharsets replaced by its alias, so that encoded words
// follow the charset rule of the body.
func aliasWordCharsets(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for {
		n := strings.Index(s[pos:], "=?")
		if n < 0 {
			break
		}
		start := pos + n + 2
		b.WriteString(s[pos:start])
		pos = start
		if end, _ := encodedWordEnd(s, start-2); end < 0 {
			continue
		}
		for _, c := range builtinWordCharsets {
			if charset, _, _ := strings.Cut(s[start:], "?"); strings.EqualFold(charset, c.name) {
				b.WriteString(c.alias)
				pos += len(c.name)
				break
			}
		}
	}
	b.WriteString(s[pos:])
	return b.String()
}

// decodeHeader returns a header value as valid UTF-8: encoded words are
// decoded, the white space between two adjacent ones is dropped, and the
// text around them is taken as UTF-8 or else windows-1252. A word that
// does not decode stays as it is.
//
// mime.WordDecoder.DecodeHeader is not used: it passes raw 8-bit text
// between the words through unchanged, so an 8-bit byte would become
// U+FFFD instead of its letter, and it takes everything up to the next
// "?=" after a malformed start as one word, losing the words inside.
func decodeHeader(value string) string {
	if !strings.Contains(value, "=?") {
		return validText(value)
	}
	var b strings.Builder
	b.Grow(len(value))
	pos, lastWordEnd := 0, -1
	for {
		n := strings.Index(value[pos:], "=?")
		if n < 0 {
			break
		}
		start := pos + n
		end, failAt := encodedWordEnd(value, start)
		if end < 0 {
			// No word can start between start and failAt-1, so the
			// search resumes there and each byte is scanned at most twice.
			resume := max(start+1, failAt-1)
			b.WriteString(validText(value[pos:resume]))
			pos = resume
			continue
		}
		decoded, err := wordDecoder.Decode(aliasWordCharsets(value[start:end]))
		if err != nil {
			b.WriteString(validText(value[pos:end]))
			pos, lastWordEnd = end, -1
			continue
		}
		if gap := value[pos:start]; lastWordEnd != pos || strings.Trim(gap, " \t") != "" {
			b.WriteString(validText(gap))
		}
		b.WriteString(strings.ToValidUTF8(decoded, string(utf8.RuneError)))
		pos, lastWordEnd = end, end
	}
	b.WriteString(validText(value[pos:]))
	return b.String()
}

// encodedWordEnd returns the end of the encoded word "=?charset?X?text?="
// at start, or -1 and the offset where it proved malformed. The charset
// holds no '=' and the encoding is a letter, so that a word starting inside
// a malformed one begins at failAt-1 at the earliest.
func encodedWordEnd(s string, start int) (end, failAt int) {
	p := start + 2
	for p < len(s) && s[p] != '?' {
		if c := s[p]; c <= ' ' || c == '=' || c >= 0x7f {
			return -1, p
		}
		p++
	}
	if p+2 >= len(s) {
		return -1, p
	}
	switch s[p+1] {
	case 'B', 'b', 'Q', 'q':
	default:
		return -1, p + 1
	}
	if s[p+2] != '?' {
		return -1, p + 2
	}
	p += 3
	for p < len(s) && s[p] != '?' {
		if c := s[p]; c <= ' ' || c >= 0x7f {
			return -1, p
		}
		p++
	}
	if p+1 >= len(s) || s[p+1] != '=' {
		return -1, p
	}
	return p + 2, 0
}

// collapseSpace returns s with each run of white space replaced by one
// space and none at either end.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	isSpacePending := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			isSpacePending = b.Len() > 0
			continue
		}
		if isSpacePending {
			b.WriteByte(' ')
			isSpacePending = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// normalizeLineEnds replaces CRLF by LF: text decoded from base64 or
// quoted-printable keeps the line ends of the sender.
func normalizeLineEnds(data []byte) []byte {
	if !bytes.Contains(data, []byte("\r\n")) {
		return data
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}
