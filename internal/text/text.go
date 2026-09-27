// Package text escapes, measures and truncates text for the markup of each
// target. It is a leaf of the package graph.
//
// Every escaper is applied exactly once to a value taken from the message;
// none of them is idempotent.
package text

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Format names the markup of a built-in template: what a target expects
// in its text field or request body.
type Format string

// Built-in formats.
const (
	FormatPlain              Format = "plain"
	FormatTelegramHTML       Format = "telegram-html"
	FormatTelegramMarkdownV2 Format = "telegram-markdownv2"
	FormatSlackMrkdwn        Format = "slack-mrkdwn"
	FormatDiscord            Format = "discord"
	FormatNtfy               Format = "ntfy"
	FormatMattermost         Format = "mattermost"
	FormatSlackWebhook       Format = "slack-webhook"
	FormatGenericJSON        Format = "generic-json"
)

// zeroWidthSpace separates characters that would otherwise form markup,
// without a visible trace.
const zeroWidthSpace = "\u200b"

var (
	telegramHTMLReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	slackReplacer        = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// EscapeTelegramHTML escapes s for parse_mode HTML: &, < and > become the
// entities Telegram knows.
func EscapeTelegramHTML(s string) string {
	return telegramHTMLReplacer.Replace(s)
}

// markdownV2Special are the characters Telegram MarkdownV2 requires to be
// escaped outside code, the backslash included.
const markdownV2Special = "_*[]()~`>#+-=|{}.!\\"

// EscapeTelegramMarkdownV2 puts a backslash before each character that
// MarkdownV2 treats as markup outside code and pre entities.
func EscapeTelegramMarkdownV2(s string) string {
	return escapeWithBackslash(s, markdownV2Special)
}

// EscapeTelegramMarkdownV2Code escapes s for the inside of a MarkdownV2
// code or pre entity, where only the backquote and the backslash are
// markup.
func EscapeTelegramMarkdownV2Code(s string) string {
	return escapeWithBackslash(s, "`\\")
}

// escapeWithBackslash puts a backslash before each byte of s found in
// special, which holds ASCII only.
func escapeWithBackslash(s, special string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(special, s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// EscapeSlack escapes s for Slack mrkdwn: only &, < and > become entities,
// which keeps <!channel> and <@U123> from becoming mentions. Formatting
// characters such as * and _ have no escape in mrkdwn and stay.
func EscapeSlack(s string) string {
	return slackReplacer.Replace(s)
}

// discordSpecial are the characters Discord markdown treats as markup
// anywhere in a line; a backslash before any of them shows it literally.
// An escaped [ is enough to break a masked link [text](url).
const discordSpecial = "\\*_~|`[]"

// discordLineStart are the characters that are markup at the start of a
// line only: headings, quotes and list items.
const discordLineStart = "#>-+"

// EscapeDiscord escapes Discord markdown: bold, italics, underline,
// strike-through, spoilers, inline code, masked links, the backslash, and
// headings, quotes and list markers at the start of a line. Mentions are
// not touched; allowed_mentions in the request keeps them silent.
func EscapeDiscord(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	isLineStart := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(discordSpecial, c) >= 0:
			b.WriteByte('\\')
		case isLineStart && strings.IndexByte(discordLineStart, c) >= 0:
			b.WriteByte('\\')
		}
		b.WriteByte(c)
		switch c {
		case '\n':
			isLineStart = true
		case ' ', '\t':
		default:
			isLineStart = false
		}
	}
	return b.String()
}

// EscapeCodeBlock keeps s from closing a fenced code block of Discord,
// Mattermost or Slack: a zero-width space goes between two adjacent
// backquotes, so no run of three remains. Backslash escapes do not work
// inside a code block, so this is the only way to show the backquotes.
func EscapeCodeBlock(s string) string {
	if !strings.Contains(s, "``") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); i++ {
		if s[i] == '`' && i > 0 && s[i-1] == '`' {
			b.WriteString(zeroWidthSpace)
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mattermostMentions are the mentions that notify a whole channel.
var mattermostMentions = []string{"channel", "all", "here"}

// EscapeMattermost keeps @channel, @all and @here, in any case, from
// notifying a channel: a zero-width space follows the @. The mention counts
// when the @ does not follow a word character and the name is not followed
// by one, as Mattermost matches it.
func EscapeMattermost(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		b.WriteByte(s[i])
		if s[i] == '@' && (i == 0 || !isWordByte(s[i-1])) && isChannelMention(s[i+1:]) {
			b.WriteString(zeroWidthSpace)
		}
	}
	return b.String()
}

// mattermostSpecial are the characters Mattermost markdown treats as
// markup inside a line of text; a backslash before any of them shows it
// literally, and an escaped [ is enough to break a link [text](url).
const mattermostSpecial = "\\`*_~[]<>#|&"

// EscapeMattermostMarkdown escapes s for a line of Mattermost markdown
// outside code: markup characters get a backslash, and channel-wide
// mentions are silenced as by EscapeMattermost.
func EscapeMattermostMarkdown(s string) string {
	return EscapeMattermost(escapeWithBackslash(s, mattermostSpecial))
}

// isChannelMention reports whether s starts with a channel-wide mention
// name that no word character follows.
func isChannelMention(s string) bool {
	for _, name := range mattermostMentions {
		if len(s) >= len(name) && strings.EqualFold(s[:len(name)], name) && (len(s) == len(name) || !isWordByte(s[len(name)])) {
			return true
		}
	}
	return false
}

// isWordByte reports whether c is an ASCII letter, digit or underscore.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// UTF16Len returns the length of s in UTF-16 code units, the unit of
// Telegram's limits: a character outside the BMP counts twice.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// RuneCount returns the number of characters of s.
func RuneCount(s string) int {
	return utf8.RuneCountInString(s)
}

// ByteLen returns the length of s in bytes.
func ByteLen(s string) int {
	return len(s)
}

// maxEntityLength bounds the name of an HTML entity MeasureTelegramHTML
// looks for, so that an & without ; costs no scan to the end of the text.
const maxEntityLength = 10

// MeasureTelegramHTML returns the length of a parse_mode HTML text as
// Telegram counts it after parsing entities, in UTF-16 code units: tags
// count nothing, &lt; &gt; &amp; &quot; and numeric entities count as the
// character they stand for, anything else counts as written.
func MeasureTelegramHTML(s string) int {
	n := 0
	hasClosingBracket := true
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			if hasClosingBracket {
				if end := strings.IndexByte(s[i:], '>'); end >= 0 {
					i += end + 1
					continue
				}
				hasClosingBracket = false
			}
		case '&':
			if length, units := entityAt(s[i:]); length > 0 {
				n += units
				i += length
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		n += utf16.RuneLen(r)
		i += size
	}
	return n
}

// entityAt returns the length of the entity at the start of s and the
// UTF-16 units of the character it stands for; 0 when there is none that
// Telegram decodes.
func entityAt(s string) (length, units int) {
	end := strings.IndexByte(s[:min(len(s), maxEntityLength+2)], ';')
	if end < 0 {
		return 0, 0
	}
	name := s[1:end]
	switch name {
	case "lt", "gt", "amp", "quot":
		return end + 1, 1
	}
	if r, ok := numericEntity(name); ok {
		return end + 1, utf16.RuneLen(r)
	}
	return 0, 0
}

// numericEntity decodes "#123" or "#x7B".
func numericEntity(name string) (rune, bool) {
	digits, base := strings.TrimPrefix(name, "#"), 10
	if len(digits) == len(name) || digits == "" {
		return 0, false
	}
	if hex, ok := strings.CutPrefix(digits, "x"); ok {
		digits, base = hex, 16
	} else if hex, ok := strings.CutPrefix(digits, "X"); ok {
		digits, base = hex, 16
	}
	if digits == "" {
		return 0, false
	}
	value := 0
	for _, c := range digits {
		d := strings.IndexRune("0123456789abcdef", c|0x20)
		if d < 0 || d >= base {
			return 0, false
		}
		value = value*base + d
		if value > utf8.MaxRune {
			return 0, false
		}
	}
	if utf16.RuneLen(rune(value)) < 0 {
		return utf8.RuneError, true
	}
	return rune(value), true
}

// TruncateRunes returns the first n characters of s; empty for n <= 0.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// TruncateBytes returns the longest prefix of s of at most n bytes that
// does not end inside a character; empty for n <= 0.
func TruncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for cut := n; cut > n-utf8.UTFMax && cut > 0; cut-- {
		if utf8.RuneStart(s[cut]) {
			return s[:cut]
		}
	}
	return s[:n]
}

// wordWindow is how many characters CutAtWord gives up at most to end at
// a word boundary.
const wordWindow = 100

// CutAtWord returns s cut to at most n characters. When s is longer, the
// cut moves back to the last white space among the final wordWindow
// characters, if there is one, so that no word is split; the white space
// itself is dropped.
func CutAtWord(s string, n int) string {
	prefix := TruncateRunes(s, n)
	if len(prefix) == len(s) {
		return s
	}
	rest := prefix
	for range wordWindow {
		r, size := utf8.DecodeLastRuneInString(rest)
		if size == 0 {
			break
		}
		rest = rest[:len(rest)-size]
		if r == ' ' || r == '\t' || r == '\n' {
			return rest
		}
	}
	return prefix
}
