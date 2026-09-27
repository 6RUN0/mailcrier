package message

import (
	"net/mail"
	"strings"
)

// ParseAddressList returns the mailboxes of an address header value. It
// never fails: a list net/mail rejects, typically because an address has
// no domain as in cron's `"(Cron Daemon)" <root>` or `root (Cron Daemon)`,
// is split at top-level commas, and each part yields the address in angle
// brackets, or else the text outside comments, with the display name or
// the first comment as Name.
func ParseAddressList(value string) []Address {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if list, err := mail.ParseAddressList(value); err == nil {
		addresses := make([]Address, 0, len(list))
		for _, a := range list {
			addresses = append(addresses, Address{Name: a.Name, Addr: a.Address})
		}
		return addresses
	}
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
		return Address{Name: name, Addr: strings.TrimSpace(outside[open+1 : open+end])}, true
	}
	a := Address{Addr: strings.Join(strings.Fields(outside), "")}
	if len(comments) > 0 {
		a.Name = comments[0]
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
