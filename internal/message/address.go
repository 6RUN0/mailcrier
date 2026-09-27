package message

import (
	"net/mail"
	"strings"
	"unicode"
)

// ParseAddressList returns the mailboxes of an address header value. It
// never fails: a list net/mail rejects, typically because an address has
// no domain as in cron's `"(Cron Daemon)" <root>` or `root (Cron Daemon)`,
// is split at top-level commas, and each part yields the address in angle
// brackets, or else the text outside comments, with the display name or
// the first comment as Name.
//
// A value over maxStrictListLength goes to the loose parser directly:
// net/mail collects the words of a phrase in a slice, which for megabytes
// of words costs many times the value in memory.
func ParseAddressList(value string) []Address {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if len(value) > maxStrictListLength {
		return parseLooseList(value)
	}
	list, err := addressParser.ParseList(value)
	if err != nil {
		return parseLooseList(value)
	}
	var aliased []*mail.Address
	if value != aliasWordCharsets(value) {
		if names, err := addressParser.ParseList(aliasWordCharsets(value)); err == nil && len(names) == len(list) {
			aliased = names
		}
	}
	addresses := make([]Address, 0, len(list))
	for i, a := range list {
		name := a.Name
		if aliased != nil && aliased[i].Name != aliasWordCharsets(name) {
			name = aliased[i].Name
		}
		addresses = append(addresses, Address{Name: name, Addr: a.Address})
	}
	return addresses
}

// addressParser decodes encoded display names in every charset decodeText
// knows; net/mail alone knows only UTF-8, US-ASCII and Latin-1.
//
// For an encoded word in US-ASCII or Latin-1 to follow the charset rule of
// the body, ParseAddressList parses the list a second time with the words
// passed through aliasWordCharsets and takes the names from there, but
// only those that net/mail decoded: an encoded word in a quoted string or
// an address is text, and the alias would change it. The addresses always
// come from the list as written.
var addressParser = &mail.AddressParser{WordDecoder: wordDecoder}

// maxStrictListLength bounds the header values given to net/mail; a list
// of a thousand addresses stays far below it.
const maxStrictListLength = 256 << 10

// parseLooseList reads a list without requiring a domain.
func parseLooseList(value string) []Address {
	var addresses []Address
	for _, part := range splitTopLevel(value) {
		if a, ok := parseLooseAddress(part); ok {
			addresses = append(addresses, a)
		}
	}
	return addresses
}

// splitTopLevel splits value at commas outside quotes, angle brackets and
// comments.
func splitTopLevel(value string) []string {
	var parts []string
	var quoted, escaped bool
	angle, comment, start := 0, 0, 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case quoted:
			quoted = c != '"'
		case c == '"':
			quoted = true
		case c == '(':
			comment++
		case c == ')' && comment > 0:
			comment--
		case comment > 0:
		case c == '<':
			angle++
		case c == '>' && angle > 0:
			angle--
		case c == ',' && angle == 0:
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

// parseLooseAddress reads one mailbox without requiring a domain.
func parseLooseAddress(part string) (Address, bool) {
	part = strings.TrimSpace(part)
	if part == "" {
		return Address{}, false
	}
	outside, comments := stripComments(part)
	if open := strings.LastIndexByte(outside, '<'); open >= 0 {
		end := strings.IndexByte(outside[open:], '>')
		if end < 0 {
			end = len(outside) - open
		}
		name := unquote(strings.TrimSpace(outside[:open]))
		if name == "" && len(comments) > 0 {
			name = comments[0]
		}
		return Address{Name: decodeHeader(name), Addr: validText(strings.TrimSpace(outside[open+1 : open+end]))}, true
	}
	a := Address{Addr: validText(withoutSpace(outside))}
	if len(comments) > 0 {
		a.Name = decodeHeader(comments[0])
	}
	return a, a.Addr != "" || a.Name != ""
}

// stripComments returns part without its parenthesized comments, which
// are returned trimmed, outermost level only.
func stripComments(part string) (string, []string) {
	var outside strings.Builder
	var comments []string
	var current strings.Builder
	var quoted, escaped bool
	depth := 0
	for i := 0; i < len(part); i++ {
		c := part[i]
		switch {
		case depth > 0 && escaped:
			escaped = false
			current.WriteByte(c)
		case depth > 0 && c == '\\':
			escaped = true
		case depth > 0 && c == '(':
			depth++
			current.WriteByte(c)
		case depth > 0 && c == ')':
			depth--
			if depth == 0 {
				comments = append(comments, strings.TrimSpace(current.String()))
				current.Reset()
			} else {
				current.WriteByte(c)
			}
		case depth > 0:
			current.WriteByte(c)
		case escaped:
			escaped = false
			outside.WriteByte(c)
		case c == '\\':
			escaped = true
			outside.WriteByte(c)
		case quoted:
			quoted = c != '"'
			outside.WriteByte(c)
		case c == '"':
			quoted = true
			outside.WriteByte(c)
		case c == '(':
			depth = 1
		default:
			outside.WriteByte(c)
		}
	}
	if depth > 0 {
		comments = append(comments, strings.TrimSpace(current.String()))
	}
	return outside.String(), comments
}

// unquote removes the quotes around a quoted display name and the
// backslashes of its quoted pairs.
func unquote(name string) string {
	if len(name) < 2 || name[0] != '"' || name[len(name)-1] != '"' {
		return name
	}
	var out strings.Builder
	escaped := false
	for i := 1; i < len(name)-1; i++ {
		if !escaped && name[i] == '\\' {
			escaped = true
			continue
		}
		escaped = false
		out.WriteByte(name[i])
	}
	return out.String()
}

// withoutSpace returns s without its white space.
func withoutSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
