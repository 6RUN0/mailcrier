package message

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"unicode"
)

// maxMultipartDepth bounds the nesting of multipart entities; a deeper
// entity is kept as an attachment. Real mail nests three levels at most
// (mixed, alternative, related).
const maxMultipartDepth = 8

// maxBoundaryLength is the longest multipart boundary accepted; RFC 2046
// allows 70 characters.
const maxBoundaryLength = 200

// entity is one MIME entity: the raw values of the fields that shape it
// and its body, a slice of the input.
type entity struct {
	contentType, encoding, disposition string
	body                               []byte
}

// entityFields returns the entity of a header block and a body, with the
// first value of each field that matters.
func entityFields(block string, body []byte) entity {
	e := entity{body: body}
	for name, value := range fields(block) {
		var dst *string
		switch textproto.CanonicalMIMEHeaderKey(name) {
		case "Content-Type":
			dst = &e.contentType
		case "Content-Transfer-Encoding":
			dst = &e.encoding
		case "Content-Disposition":
			dst = &e.disposition
		default:
			continue
		}
		if *dst == "" {
			*dst = value
		}
	}
	return e
}

// bodyParts collects what walking an entity tree finds.
type bodyParts struct {
	plain       []string
	html        string
	attachments []Attachment
	isMalformed bool
}

// decodeBody fills Body, BodyHTML and Attachments of msg from the top-level
// entity e. It never fails: a broken multipart structure keeps the parts
// read before the break, and a multipart entity without any part is text.
// The Content-Type is honoured without MIME-Version, as sudo sends it.
func decodeBody(msg *Message, e entity) (isMalformed bool) {
	var parts bodyParts
	parts.walk(e, 0)
	msg.Body = joinText(parts.plain)
	msg.BodyHTML = parts.html
	msg.Attachments = parts.attachments
	if len(parts.plain) == 0 && parts.html != "" {
		msg.Body = htmlToText(parts.html)
	}
	return parts.isMalformed
}

// walk adds the content of e to p.
func (p *bodyParts) walk(e entity, depth int) {
	mediaType, params := parseMediaType(e.contentType)
	if strings.HasPrefix(mediaType, "multipart/") && depth < maxMultipartDepth {
		children, isMalformed := splitMultipart(e.body, params["boundary"])
		p.isMalformed = p.isMalformed || isMalformed
		if len(children) == 0 {
			p.plain = append(p.plain, decodeText(e.body, ""))
			return
		}
		if mediaType == "multipart/alternative" {
			p.walkAlternative(children, depth+1)
			return
		}
		for _, child := range children {
			p.walk(child, depth+1)
		}
		return
	}
	data := decodeTransfer(e.encoding, e.body)
	disposition, dispositionParams := parseMediaType(e.disposition)
	name := dispositionParams["filename"]
	if name == "" {
		name = params["name"]
	}
	// A named text/plain part is the text unless it is an attachment
	// explicitly: some callers name the log they send inline.
	isAttachment := disposition == "attachment" || name != "" && mediaType != "text/plain"
	switch {
	case mediaType == "text/plain" && !isAttachment:
		p.plain = append(p.plain, decodeText(normalizeLineEnds(data), params["charset"]))
	case mediaType == "text/html" && !isAttachment && p.html == "":
		p.html = decodeText(normalizeLineEnds(data), params["charset"])
	default:
		p.attachments = append(p.attachments, Attachment{
			Name:        attachmentName(name),
			ContentType: mediaType,
			Size:        int64(len(data)),
			Data:        data,
		})
	}
}

// walkAlternative takes the plain text of a multipart/alternative entity,
// and keeps its HTML version, a text/html part or the one inside a
// multipart/related part, as BodyHTML together with the attachments of
// that version; without plain text it walks the HTML version, or else the
// last part, the richest one by RFC 2046.
func (p *bodyParts) walkAlternative(children []entity, depth int) {
	plain, rich := -1, -1
	for i, child := range children {
		mediaType, _ := parseMediaType(child.contentType)
		switch {
		case mediaType == "text/plain" && plain < 0:
			plain = i
		case (mediaType == "text/html" || mediaType == "multipart/related") && rich < 0:
			rich = i
		}
	}
	switch {
	case plain >= 0:
		if rich >= 0 {
			// The text of the HTML version is dropped: the plain part
			// says the same.
			version := bodyParts{html: p.html}
			version.walk(children[rich], depth)
			p.html = version.html
			p.attachments = append(p.attachments, version.attachments...)
			p.isMalformed = p.isMalformed || version.isMalformed
		}
		p.walk(children[plain], depth)
	case rich >= 0:
		p.walk(children[rich], depth)
	default:
		p.walk(children[len(children)-1], depth)
	}
}

// parseMediaType returns the lowercase media type and the parameters of a
// Content-Type or Content-Disposition value; text/plain for an empty or
// unreadable Content-Type. Parameters that do not parse are dropped.
func parseMediaType(value string) (string, map[string]string) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil && err != mime.ErrInvalidMediaParameter {
		mediaType, params = "", nil
	}
	if mediaType == "" {
		mediaType = "text/plain"
	}
	return mediaType, params
}

// decodeTransfer undoes a Content-Transfer-Encoding. Both decoders are
// lenient: base64 skips bytes outside its alphabet, and quoted-printable
// input that the standard reader rejects stays as it came.
func decodeTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return decodeBase64(body)
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return body
		}
		return decoded
	default:
		return body
	}
}

// decodeBase64 decodes the base64 alphabet characters of data and ignores
// everything else, padding and line breaks included.
func decodeBase64(data []byte) []byte {
	filtered := make([]byte, 0, len(data))
	for _, c := range data {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' {
			filtered = append(filtered, c)
		}
	}
	if len(filtered)%4 == 1 {
		filtered = filtered[:len(filtered)-1]
	}
	decoded := make([]byte, base64.RawStdEncoding.DecodedLen(len(filtered)))
	n, _ := base64.RawStdEncoding.Decode(decoded, filtered)
	return decoded[:n]
}

// attachmentName returns the file name of an attachment: encoded words
// decoded, directories, control characters and the marks that reorder
// text removed, and empty for "." and "..". A right-to-left override
// would show "evil\u202egpj.exe" as "evilexe.jpg".
func attachmentName(name string) string {
	name = decodeHeader(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// isBidiControl reports whether r is a mark or an embedding, override or
// isolate control of the Unicode bidirectional algorithm.
func isBidiControl(r rune) bool {
	return r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

// joinText joins text parts, each starting on a new line.
func joinText(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	var b strings.Builder
	for _, part := range parts {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

// splitMultipart returns the parts of a multipart body as slices of it.
// The input has LF line ends. A delimiter is a line of "--" and the
// boundary, the close delimiter adds "--"; trailing white space is allowed.
// The preamble and the epilogue are dropped, and a body without a close
// delimiter ends its last part at the end of the input. isMalformed
// reports a boundary that is missing, too long, or never found.
func splitMultipart(body []byte, boundary string) (parts []entity, isMalformed bool) {
	if boundary == "" || len(boundary) > maxBoundaryLength {
		return nil, true
	}
	delimiter := []byte("--" + boundary)
	lineDelimiter := []byte("\n--" + boundary)
	partStart := -1
	for pos := 0; pos <= len(body); {
		at := findDelimiter(body, pos, delimiter, lineDelimiter)
		if at < 0 {
			break
		}
		lineStart, rest := at, at+len(delimiter)
		delimiterEnd := lineEnd(body, lineStart)
		tail := bytes.TrimRight(body[rest:delimiterEnd], " \t\n")
		isClose := bytes.Equal(tail, []byte("--"))
		if len(tail) > 0 && !isClose {
			pos = rest
			continue
		}
		if partStart >= 0 {
			parts = append(parts, newPart(body[partStart:max(partStart, lineStart-1)]))
		}
		if isClose {
			return parts, false
		}
		partStart, pos = delimiterEnd, delimiterEnd
		if delimiterEnd == len(body) {
			break
		}
	}
	if partStart >= 0 && partStart < len(body) {
		parts = append(parts, newPart(body[partStart:]))
	}
	return parts, partStart < 0
}

// findDelimiter returns the offset of the next line at or after pos that
// starts with delimiter, or -1; lineDelimiter is delimiter after an LF.
func findDelimiter(body []byte, pos int, delimiter, lineDelimiter []byte) int {
	if pos == 0 && bytes.HasPrefix(body, delimiter) {
		return 0
	}
	from := max(pos-1, 0)
	n := bytes.Index(body[from:], lineDelimiter)
	if n < 0 {
		return -1
	}
	return from + n + 1
}

// newPart splits one part into its header block and body, as Read does for
// the message; a part without header fields is all body.
func newPart(part []byte) entity {
	headerEnd, bodyStart, _ := scanHeader(part)
	return entityFields(string(part[:headerEnd]), part[bodyStart:])
}
