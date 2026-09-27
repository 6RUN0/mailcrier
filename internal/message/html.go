package message

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// droppedElements are removed together with their content: code, styles,
// embedded objects and document metadata carry no text for a reader.
// rawTextElements among them have content that the tokenizer returns as
// text even after a self-closing tag such as <script/>, up to the end tag.
var droppedElements = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Svg: true, atom.Math: true, atom.Noscript: true,
	atom.Title: true, atom.Template: true,
}

var rawTextElements = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Iframe: true, atom.Noscript: true,
	atom.Title: true,
}

// blockElements start and end on a line of their own; paragraphElements
// are also separated by an empty line.
var (
	blockElements = map[atom.Atom]bool{
		atom.Div: true, atom.Tr: true, atom.Table: true, atom.Ul: true, atom.Ol: true,
		atom.Blockquote: true, atom.Section: true, atom.Article: true, atom.Header: true,
		atom.Footer: true, atom.Dl: true, atom.Dt: true, atom.Dd: true, atom.Hr: true,
	}
	paragraphElements = map[atom.Atom]bool{
		atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
		atom.H5: true, atom.H6: true,
	}
)

// linkSchemes are the link targets worth showing; javascript:, data: and
// relative links are dropped and only their text stays.
var linkSchemes = []string{"http://", "https://", "mailto:", "ftp://"}

// htmlToText returns the text a reader of the HTML document sees: dropped
// elements removed with their content, white space collapsed outside pre,
// line breaks for blocks, "- " before list items, and the target of a link
// in parentheses after its text when the text is not the target itself.
func htmlToText(doc string) string {
	z := html.NewTokenizer(strings.NewReader(doc))
	var w textWriter
	var skip atom.Atom
	skipDepth := 0
	type link struct {
		href  string
		start int
	}
	var links []link
	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return w.finish()
		}
		name, _ := z.TagName()
		tag := atom.Lookup(name)
		if skipDepth > 0 {
			switch {
			case tokenType == html.StartTagToken && tag == skip:
				skipDepth++
			case tokenType == html.EndTagToken && tag == skip:
				skipDepth--
			}
			continue
		}
		switch tokenType {
		case html.TextToken:
			w.writeText(string(z.Text()))
		case html.StartTagToken, html.SelfClosingTagToken:
			switch {
			case droppedElements[tag]:
				if tokenType == html.StartTagToken && tag != atom.Embed || rawTextElements[tag] {
					skip, skipDepth = tag, 1
				}
			case tag == atom.Br:
				w.lineBreak(1)
			case tag == atom.Li:
				w.lineBreak(1)
				w.writeRaw("- ")
			case tag == atom.Pre:
				w.lineBreak(1)
				w.pre++
			case tag == atom.A:
				links = append(links, link{href: linkTarget(z), start: w.b.Len()})
			case tag == atom.Td || tag == atom.Th:
				w.writeText(" ")
			case paragraphElements[tag]:
				w.lineBreak(2)
			case blockElements[tag]:
				w.lineBreak(1)
			}
		case html.EndTagToken:
			switch {
			case tag == atom.Pre:
				w.pre = max(w.pre-1, 0)
				w.lineBreak(1)
			case tag == atom.A && len(links) > 0:
				l := links[len(links)-1]
				links = links[:len(links)-1]
				if l.href != "" && !isLinkText(w.b.String()[min(l.start, w.b.Len()):], l.href) {
					w.writeRaw(" (" + l.href + ")")
				}
			case paragraphElements[tag]:
				w.lineBreak(2)
			case blockElements[tag] || tag == atom.Li:
				w.lineBreak(1)
			}
		}
	}
}

// isLinkText reports whether the text of a link shows its target, with or
// without "mailto:". Text much longer than the target is not compared, so
// that many links around one long text cost no more than the text.
func isLinkText(text, href string) bool {
	if len(text) > 2*len(href)+64 {
		return false
	}
	text = strings.TrimSpace(text)
	address, isMailto := strings.CutPrefix(href, "mailto:")
	return text == href || isMailto && text == address
}

// linkTarget returns the href of the current a tag when its scheme is one
// of linkSchemes, else empty.
func linkTarget(z *html.Tokenizer) string {
	for {
		key, value, more := z.TagAttr()
		if string(key) == "href" {
			href := strings.TrimSpace(string(value))
			lower := strings.ToLower(href)
			for _, scheme := range linkSchemes {
				if strings.HasPrefix(lower, scheme) {
					return href
				}
			}
			return ""
		}
		if !more {
			return ""
		}
	}
}

// textWriter builds the text of htmlToText: at most two line breaks in a
// row, no space at the start or end of a line, white space collapsed
// unless inside pre.
type textWriter struct {
	b              strings.Builder
	pre            int
	pendingSpace   bool
	pendingBreaks  int
	trailingBreaks int
}

func (w *textWriter) writeText(text string) {
	if w.pre > 0 {
		w.writeRaw(text)
		return
	}
	for text != "" {
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		if unicode.IsSpace(r) {
			w.pendingSpace = true
			continue
		}
		w.writeRune(r)
	}
}

// writeRaw writes text as it is, after the pending line breaks, except
// line breaks at the start of the text or after two in a row.
func (w *textWriter) writeRaw(text string) {
	for _, r := range text {
		if r == '\n' {
			w.flushBreaks()
			w.pendingSpace = false
			if w.b.Len() == 0 || w.trailingBreaks >= 2 {
				continue
			}
			w.b.WriteByte('\n')
			w.trailingBreaks++
			continue
		}
		w.writeRune(r)
	}
}

func (w *textWriter) writeRune(r rune) {
	w.flushBreaks()
	if w.pendingSpace && w.b.Len() > 0 && w.trailingBreaks == 0 {
		w.b.WriteByte(' ')
	}
	w.pendingSpace = false
	w.b.WriteRune(r)
	w.trailingBreaks = 0
}

// lineBreak asks for n line breaks before the next text; breaks at the
// start of the text are dropped.
func (w *textWriter) lineBreak(n int) {
	w.pendingSpace = false
	w.pendingBreaks = max(w.pendingBreaks, n)
}

func (w *textWriter) flushBreaks() {
	if w.b.Len() > 0 {
		for w.trailingBreaks < min(w.pendingBreaks, 2) {
			w.b.WriteByte('\n')
			w.trailingBreaks++
		}
	}
	w.pendingBreaks = 0
}

// finish returns the text with one final line break.
func (w *textWriter) finish() string {
	text := strings.TrimRight(w.b.String(), " \n")
	if text == "" {
		return ""
	}
	return text + "\n"
}
