package text

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeTelegramMarkdownV2(t *testing.T) {
	t.Run("T-ESC-01/each-special-character", func(t *testing.T) {
		for _, c := range "_*[]()~`>#+-=|{}.!" {
			if got, want := EscapeTelegramMarkdownV2("a"+string(c)+"b"), `a\`+string(c)+"b"; got != want {
				t.Errorf("EscapeTelegramMarkdownV2(%q) = %q, want %q", "a"+string(c)+"b", got, want)
			}
		}
	})
	t.Run("T-ESC-02/backslash-doubled", func(t *testing.T) {
		if got := EscapeTelegramMarkdownV2(`C:\dir\.`); got != `C:\\dir\\\.` {
			t.Errorf("got %q", got)
		}
	})
	t.Run("plain-text-and-non-ascii-unchanged", func(t *testing.T) {
		if got := EscapeTelegramMarkdownV2("abc привет 123 @ $"); got != "abc привет 123 @ $" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("T-ESC-03/code-escapes-backquote-and-backslash-only", func(t *testing.T) {
		if got := EscapeTelegramMarkdownV2Code("a_b*c.d `x` \\ -"); got != "a_b*c.d \\`x\\` \\\\ -" {
			t.Errorf("got %q", got)
		}
	})
}

func TestEscapeTelegramHTML(t *testing.T) {
	t.Run("T-ESC-05/entities", func(t *testing.T) {
		if got := EscapeTelegramHTML(`<b>a & "b"</b>`); got != `&lt;b&gt;a &amp; "b"&lt;/b&gt;` {
			t.Errorf("got %q", got)
		}
	})
}

func TestEscapeSlack(t *testing.T) {
	t.Run("T-ESC-07/only-three-characters", func(t *testing.T) {
		if got := EscapeSlack("<!channel> & <@U123> *b* _i_ ~s~ `c` ü"); got != "&lt;!channel&gt; &amp; &lt;@U123&gt; *b* _i_ ~s~ `c` ü" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("T-ESC-07/applied-once-is-not-idempotent", func(t *testing.T) {
		if got := EscapeSlack(EscapeSlack("&")); got != "&amp;amp;" {
			t.Errorf("got %q", got)
		}
	})
}

func TestEscapeDiscord(t *testing.T) {
	cases := map[string]string{
		"**bold** __under__ ~~strike~~ ||spoiler||": `\*\*bold\*\* \_\_under\_\_ \~\~strike\~\~ \|\|spoiler\|\|`,
		`back\slash`:                          `back\\slash`,
		"# heading\n> quote\n  - item\na # b": "\\# heading\n\\> quote\n  \\- item\na # b",
		"[text](https://example.org)":         `\[text\](https://example.org)`,
		"`code`":                              "\\`code\\`",
	}
	t.Run("T-ESC-10/markdown-escaped", func(t *testing.T) {
		for in, want := range cases {
			if got := EscapeDiscord(in); got != want {
				t.Errorf("EscapeDiscord(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("T-ESC-10/code-block-survives-backquotes", func(t *testing.T) {
		got := EscapeCodeBlock("before ``` after `` ````")
		if strings.Contains(got, "``") {
			t.Errorf("EscapeCodeBlock kept adjacent backquotes: %q", got)
		}
		if strings.ReplaceAll(got, zeroWidthSpace, "") != "before ``` after `` ````" {
			t.Errorf("EscapeCodeBlock changed more than the backquotes: %q", got)
		}
	})
}

// mattermostMention matches a channel-wide mention as Mattermost does.
var mattermostMention = regexp.MustCompile(`(?i)(?:^|[^\w])@(?:channel|all|here)(?:[^\w]|$)`)

func TestEscapeMattermostMarkdown(t *testing.T) {
	t.Run("markup-escaped-mentions-silenced", func(t *testing.T) {
		in := "[click](https://example.com) *b* _i_ ~s~ `c` #h <https://x> a&b | \\ @channel"
		want := "\\[click\\](https://example.com) \\*b\\* \\_i\\_ \\~s\\~ \\`c\\` \\#h \\<https://x\\> a\\&b \\| \\\\ @\u200bchannel"
		if got := EscapeMattermostMarkdown(in); got != want {
			t.Errorf("EscapeMattermostMarkdown(%q) = %q, want %q", in, got, want)
		}
	})
}

func TestEscapeMattermost(t *testing.T) {
	t.Run("channel-wide-mentions-neutralized", func(t *testing.T) {
		in := "@channel disk full, @ALL @Here: see @here. ops@allhosts.example.org @alice @channels"
		got := EscapeMattermost(in)
		if mattermostMention.MatchString(got) {
			t.Errorf("EscapeMattermost(%q) = %q keeps a mention", in, got)
		}
		want := "@\u200bchannel disk full, @\u200bALL @\u200bHere: see @\u200bhere. ops@allhosts.example.org @alice @channels"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestMeasure(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"<b>bold</b> &lt;x&gt; &amp;", 10},
		{"&#39;&#x41;&quot;", 3},
		{"&foo; &", 7},
		{"a < b", 5},
		{"😀", 1},
		{"<pre>привет</pre>", 6},
		{"&#128512;", 1},
	}
	for _, tc := range cases {
		if got := MeasureTelegramHTML(tc.text); got != tc.want {
			t.Errorf("MeasureTelegramHTML(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
	if got := UTF16Len("a😀я"); got != 4 {
		t.Errorf("UTF16Len = %d, want 4", got)
	}
	if got := RuneCount("a😀я"); got != 3 {
		t.Errorf("RuneCount = %d, want 3", got)
	}
}

func TestTruncate(t *testing.T) {
	t.Run("T-ADJ-48/bytes-never-split-a-rune", func(t *testing.T) {
		s := "aя😀b"
		for n := -1; n <= len(s)+1; n++ {
			got := TruncateBytes(s, n)
			if len(got) > max(n, 0) || !utf8.ValidString(got) || !strings.HasPrefix(s, got) {
				t.Errorf("TruncateBytes(%q, %d) = %q", s, n, got)
			}
		}
		if got := TruncateBytes(s, 6); got != "aя" {
			t.Errorf("TruncateBytes(%q, 6) = %q, want %q", s, got, "aя")
		}
	})
	t.Run("T-ADJ-48/zero-or-negative-limit", func(t *testing.T) {
		if TruncateBytes("abc", 0) != "" || TruncateBytes("abc", -5) != "" || TruncateRunes("abc", 0) != "" || TruncateRunes("abc", -1) != "" {
			t.Error("a zero or negative limit must give an empty string")
		}
	})
	t.Run("T-LIM-15/runes-never-split-a-character", func(t *testing.T) {
		s := "Привет 😀 мир"
		for n := 0; n <= utf8.RuneCountInString(s)+1; n++ {
			got := TruncateRunes(s, n)
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) != min(n, utf8.RuneCountInString(s)) {
				t.Errorf("TruncateRunes(%q, %d) = %q", s, n, got)
			}
		}
	})
}

func TestCutAtWord(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"hello world again", 13, "hello world"},
		{"hello world", 11, "hello world"},
		{strings.Repeat("x", 150) + " tail", 150, strings.Repeat("x", 150)},
		{"a " + strings.Repeat("x", 120), 110, "a " + strings.Repeat("x", 108)},
		{"слово другое", 9, "слово"},
		{"abc", 0, ""},
	}
	for _, tc := range cases {
		if got := CutAtWord(tc.s, tc.n); got != tc.want {
			t.Errorf("CutAtWord(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

// unescapeBackslash removes one backslash before each character.
func unescapeBackslash(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// FuzzEscapeTelegram checks the Telegram escapers: the input comes back
// after unescaping, no markup character is left bare, and the measured
// length of escaped HTML is the length of the input.
func FuzzEscapeTelegram(f *testing.F) {
	for _, seed := range []string{"a_b*c", "<b>&amp;</b>", `\`, "😀.-!", "&#39;"} {
		f.Add(seed)
	}
	htmlUnescaper := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		escapedHTML := EscapeTelegramHTML(s)
		if strings.ContainsAny(escapedHTML, "<>") || htmlUnescaper.Replace(escapedHTML) != s {
			t.Errorf("EscapeTelegramHTML(%q) = %q", s, escapedHTML)
		}
		if got, want := MeasureTelegramHTML(escapedHTML), RuneCount(s); got != want {
			t.Errorf("MeasureTelegramHTML(EscapeTelegramHTML(%q)) = %d, want %d", s, got, want)
		}
		for name, escape := range map[string]func(string) string{"MarkdownV2": EscapeTelegramMarkdownV2, "MarkdownV2Code": EscapeTelegramMarkdownV2Code} {
			escaped := escape(s)
			if unescapeBackslash(escaped) != s {
				t.Errorf("%s(%q) = %q does not unescape", name, s, escaped)
			}
		}
		escaped := EscapeTelegramMarkdownV2(s)
		for i := 0; i < len(escaped); i++ {
			if escaped[i] == '\\' {
				i++
				continue
			}
			if strings.IndexByte(markdownV2Special, escaped[i]) >= 0 {
				t.Fatalf("EscapeTelegramMarkdownV2(%q) = %q leaves %q bare", s, escaped, escaped[i])
			}
		}
	})
}

// FuzzEscapeChat checks the Slack, Discord and Mattermost escapers: no
// markup that could mention or format survives, and removing the escapes
// gives the input back.
func FuzzEscapeChat(f *testing.F) {
	for _, seed := range []string{"<!channel>", "**x** # y\n> z", "@here @all", "```", "a\\b"} {
		f.Add(seed)
	}
	slackUnescaper := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
	f.Fuzz(func(t *testing.T, s string) {
		if escaped := EscapeSlack(s); strings.ContainsAny(escaped, "<>") || slackUnescaper.Replace(escaped) != s {
			t.Errorf("EscapeSlack(%q) = %q", s, escaped)
		}
		if escaped := EscapeDiscord(s); unescapeBackslash(escaped) != s {
			t.Errorf("EscapeDiscord(%q) = %q does not unescape", s, escaped)
		}
		if escaped := EscapeCodeBlock(s); strings.Contains(escaped, "``") || strings.ReplaceAll(escaped, zeroWidthSpace, "") != strings.ReplaceAll(s, zeroWidthSpace, "") {
			t.Errorf("EscapeCodeBlock(%q) = %q", s, escaped)
		}
		markdown := EscapeMattermostMarkdown(s)
		if unescapeBackslash(strings.ReplaceAll(markdown, zeroWidthSpace, "")) != strings.ReplaceAll(s, zeroWidthSpace, "") {
			t.Errorf("EscapeMattermostMarkdown(%q) = %q does not unescape", s, markdown)
		}
		if utf8.ValidString(s) && mattermostMention.MatchString(markdown) {
			t.Errorf("EscapeMattermostMarkdown(%q) = %q keeps a mention", s, markdown)
		}
		escaped := EscapeMattermost(s)
		if strings.ReplaceAll(escaped, zeroWidthSpace, "") != strings.ReplaceAll(s, zeroWidthSpace, "") {
			t.Errorf("EscapeMattermost(%q) = %q changes more than zero-width spaces", s, escaped)
		}
		if utf8.ValidString(s) && mattermostMention.MatchString(escaped) {
			t.Errorf("EscapeMattermost(%q) = %q keeps a mention", s, escaped)
		}
	})
}

func TestCutTelegramHTML(t *testing.T) {
	t.Run("fits-unchanged", checkCut("<b>a &amp; b</b>", 5, "<b>a &amp; b</b>"))
	t.Run("T-LIM-05/entity-kept-whole", checkCut("<b>a &amp; b</b>", 3, "<b>a &amp;</b>"))
	t.Run("T-LIM-05/entity-not-split", checkCut("<b>a &amp; b</b>", 2, "<b>a </b>"))
	t.Run("T-LIM-05/open-tags-closed-innermost-first", checkCut("<blockquote expandable><b>xyz</b>abc</blockquote>", 2, "<blockquote expandable><b>xy</b></blockquote>"))
	t.Run("T-LIM-05/closed-element-stays-closed", checkCut("<b>x</b><i>yz</i>", 2, "<b>x</b><i>y</i>"))
	t.Run("character-outside-bmp-whole", checkCut("<pre>😀😀</pre>", 1, "<pre>😀</pre>"))
	t.Run("no-empty-element-at-the-end", checkCut("<b>x</b><i>yz</i>", 1, "<b>x</b>"))
	t.Run("nothing-fits", checkCut("<b>abc</b>", 0, ""))
	// The validator knows only the tags of the built-in template, so these
	// two compare the result alone.
	for _, tc := range []struct{ name, in, want string }{
		{"name-ends-at-any-space", "<a\thref=\"https://example.org\">link</a>", "<a\thref=\"https://example.org\">li</a>"},
		{"end-tag-in-other-case-closes", "<B>x</b><i>yz</i>", "<B>x</b><i>y</i>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CutTelegramHTML(tc.in, 2, MeasureTelegramHTML); got != tc.want {
				t.Errorf("CutTelegramHTML(%q, 2) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	t.Run("byte-measure", func(t *testing.T) {
		got := CutTelegramHTML("<pre>abcdef</pre>", 15, ByteLen)
		if got != "<pre>abcd</pre>" {
			t.Errorf("got %q, want %q", got, "<pre>abcd</pre>")
		}
	})
}

// checkCut returns a subtest that cuts in to limit characters and wants
// the result want, which the Bot API parses.
func checkCut(in string, limit int, want string) func(*testing.T) {
	return func(t *testing.T) {
		got := CutTelegramHTML(in, limit, MeasureTelegramHTML)
		if got != want {
			t.Errorf("CutTelegramHTML(%q, %d) = %q, want %q", in, limit, got, want)
		}
		if err := validTelegramHTML(got); err != "" {
			t.Error(err)
		}
	}
}

// telegramMarkup matches a tag or an entity of the HTML parse mode as the
// built-in template writes them.
var telegramMarkup = regexp.MustCompile(`</?[a-z]+(?: expandable)?>|&(?:amp|lt|gt|quot|#[0-9]+|#x[0-9a-fA-F]+);`)

// validTelegramHTML returns why the Bot API would not parse s, or "":
// every tag closed in order, no <, > or & outside a tag or an entity.
func validTelegramHTML(s string) string {
	var open []string
	for _, markup := range telegramMarkup.FindAllString(s, -1) {
		switch {
		case strings.HasPrefix(markup, "&"):
		case strings.HasPrefix(markup, "</"):
			if len(open) == 0 || open[len(open)-1] != strings.Trim(markup, "</>") {
				return "unbalanced " + markup + " in " + s
			}
			open = open[:len(open)-1]
		default:
			name, _, _ := strings.Cut(strings.Trim(markup, "<>"), " ")
			open = append(open, name)
		}
	}
	if len(open) > 0 {
		return "unclosed " + strings.Join(open, ",") + " in " + s
	}
	if strings.ContainsAny(telegramMarkup.ReplaceAllString(s, ""), "<>&") {
		return "bare markup character in " + s
	}
	return ""
}

// FuzzCutTelegramHTML checks that a cut of escaped text inside nested
// elements measures at most the limit, is a prefix of the input up to the
// end tags it adds, and parses.
func FuzzCutTelegramHTML(f *testing.F) {
	f.Add("a & b < c > d", 5)
	f.Add("😀&amp;ёжик", 2)
	f.Add("", 0)
	f.Fuzz(func(t *testing.T, body string, limit int) {
		if !utf8.ValidString(body) {
			return
		}
		limit %= 200
		in := "<b>" + EscapeTelegramHTML(body) + "</b>\n<blockquote expandable>" + EscapeTelegramHTML(body) + "</blockquote>"
		got := CutTelegramHTML(in, limit, MeasureTelegramHTML)
		if n := MeasureTelegramHTML(got); n > max(limit, 0) {
			t.Errorf("CutTelegramHTML(%q, %d) measures %d", in, limit, n)
		}
		if err := validTelegramHTML(got); err != "" {
			t.Error(err)
		}
		trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(got, "</blockquote>"), "</b>"), "</b>")
		if !strings.HasPrefix(in, trimmed) {
			t.Errorf("CutTelegramHTML(%q, %d) = %q is no prefix of the input", in, limit, got)
		}
	})
}
