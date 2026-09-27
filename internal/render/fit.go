package render

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/6RUN0/slendmail/internal/text"
)

// ErrLimitTooSmall is returned by Fit when a text that a service parses
// strictly does not fit even with the body and the subject cut away.
var ErrLimitTooSmall = errors.New("render: the rest of the template alone exceeds the length limit")

// strictFormats are the formats a cut at an arbitrary character breaks for
// the service: JSON that no longer parses, and Telegram markup with an
// unclosed tag, entity or escape, which the Bot API rejects.
var strictFormats = map[text.Format]bool{
	text.FormatTelegramHTML: true, text.FormatTelegramMarkdownV2: true,
	text.FormatMattermost: true, text.FormatSlackWebhook: true, text.FormatGenericJSON: true,
}

// Fit renders d with t so that measure of the result is at most limit, in
// the unit of the target; a limit of 0 or less means no limit.
//
// When the full text is too long, Fit first cuts a subject longer than a
// quarter of the limit, and at least minSubjectShare characters, to that
// length by text.CutAtWord: for a notification the body matters more than
// the subject. Then it tries, in this order, until a rendering fits:
//
//   - the full body with the first attachments only, followed by the
//     MoreAttachments notice with the number left out;
//   - the longest body, cut by text.CutAtWord and ended by the Truncated
//     notice, with every attachment;
//   - the same with no attachment but the MoreAttachments notice;
//   - the Truncated notice as the body and the longest subject of one
//     character or more, then the empty subject, which the built-in
//     templates replace by the NoSubject notice.
//
// Each step is a binary search over the number of attachments or
// characters kept: at most about log2 of that number renderings. The body
// is cut before escaping, so a cut never splits an entity, and the
// rendering is measured after escaping, because escaping grows the text by
// a factor that depends on the characters. The search relies on the
// template growing with the body, the subject and the attachments, which
// holds for every template that writes them once or more.
//
// When nothing fits, because the rest of the template is too long or
// ignores the body, the subject and the attachments, a template of a
// format in strictFormats gives ErrLimitTooSmall, and any other the
// longest prefix of the rendering with the cut subject, the Truncated
// notice and no attachment that fits, cut at a character boundary.
func Fit(t *Template, d Data, limit int, measure func(string) int) (out string, truncated bool, err error) {
	full, err := t.Execute(d)
	if err != nil || limit <= 0 || measure(full) <= limit {
		return full, false, err
	}
	render := func() (string, bool, error) {
		out, err := t.Execute(d)
		return out, err == nil && measure(out) <= limit, err
	}
	body, subject, attachments := d.Body, d.Subject, d.Attachments
	if share := max(limit/4, minSubjectShare); utf8.RuneCountInString(subject) > share {
		subject = text.CutAtWord(subject, share)
		d.Subject = subject
		if out, fits, err := render(); err != nil || fits {
			return out, true, err
		}
	}
	keepAttachments := func(kept int) {
		d.Attachments, d.MoreAttachments = attachments[:kept], len(attachments)-kept
	}
	renderAttachments := func(kept int) (string, bool, error) {
		keepAttachments(kept)
		return render()
	}
	renderBody := func(kept int) (string, bool, error) {
		d.Body = withNotice(text.CutAtWord(body, kept), d.Strings.Truncated)
		return render()
	}
	if len(attachments) > 0 {
		if best, found, err := longestFit(len(attachments), renderAttachments); err != nil || found {
			return best, true, err
		}
		keepAttachments(len(attachments))
	}
	if best, found, err := longestFit(utf8.RuneCountInString(body), renderBody); err != nil || found {
		return best, true, err
	}
	if len(attachments) > 0 {
		keepAttachments(0)
		if best, found, err := longestFit(utf8.RuneCountInString(body), renderBody); err != nil || found {
			return best, true, err
		}
	}
	d.Body = withNotice("", d.Strings.Truncated)
	renderSubject := func(kept int) (string, bool, error) {
		d.Subject = text.CutAtWord(subject, kept+1)
		return render()
	}
	if best, found, err := longestFit(utf8.RuneCountInString(subject)-1, renderSubject); err != nil || found {
		return best, true, err
	}
	d.Subject = ""
	if out, fits, err := render(); err != nil || fits {
		return out, true, err
	}
	if strictFormats[t.format] {
		return "", false, ErrLimitTooSmall
	}
	d.Subject = subject
	shortest, err := t.Execute(d)
	if err != nil {
		return "", false, err
	}
	return longestPrefix(shortest, limit, measure), true, nil
}

// minSubjectShare is the length in characters to which Fit cuts a long
// subject at least, however small the limit: a few words that still name
// the event.
const minSubjectShare = 64

// longestFit returns the rendering of the largest number of items kept,
// below total, for which render reports a fit, and whether there is one.
// The search is binary: it keeps fits(low) true, or low = -1 when no
// number fitted yet, and fits(high) false. high starts at total, whose
// rendering is at least as long as one Fit has found too long, or at 1
// for total 0 or less, so that keeping nothing is tried.
func longestFit(total int, render func(kept int) (string, bool, error)) (best string, found bool, err error) {
	low, high := -1, max(total, 1)
	for high-low > 1 {
		middle := low + (high-low)/2
		candidate, fits, err := render(middle)
		if err != nil {
			return "", false, err
		}
		if fits {
			low, best = middle, candidate
		} else {
			high = middle
		}
	}
	return best, low >= 0, nil
}

// withNotice ends a cut body with the notice on a line of its own.
func withNotice(body, notice string) string {
	if notice == "" {
		return body
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + notice
}

// longestPrefix returns the longest prefix of s, cut at a character
// boundary, whose measure is at most limit, found by binary search over
// the number of characters.
func longestPrefix(s string, limit int, measure func(string) int) string {
	low, high := 0, utf8.RuneCountInString(s)
	for low < high {
		middle := low + (high-low+1)/2
		if measure(text.TruncateRunes(s, middle)) <= limit {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return text.TruncateRunes(s, low)
}
