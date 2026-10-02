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
// the service: JSON that no longer parses, and Telegram MarkdownV2 with an
// unclosed entity or escape, which the Bot API rejects. Telegram HTML is
// cut by text.CutTelegramHTML instead, which closes what it leaves open.
var strictFormats = map[text.Format]bool{
	text.FormatTelegramMarkdownV2: true,
	text.FormatMattermost:         true, text.FormatSlackWebhook: true, text.FormatGenericJSON: true,
}

// Fit renders d with t so that measure of the result is at most limit, in
// the unit of the target; a limit of 0 or less means no limit.
//
// When the full text is too long, Fit first cuts a subject that measures
// more than a quarter of the limit, and more than minSubjectShare, to that
// share by text.CutAtWord: for a notification the body matters more than
// the subject. The subject is escaped as the template's format escapes it
// and then measured with measure, which counts it in the unit of the
// target. A cut subject ends with
// subjectCutMark. Then Fit tries, in this order, until a rendering fits:
//
//   - the full body with the first attachments only, followed by the
//     MoreAttachments notice with the number left out;
//   - the longest body, cut by text.CutAtWord and ended by the Truncated
//     notice, with every attachment;
//   - the same with no attachment but the MoreAttachments notice;
//   - the Truncated notice as the body and the longest subject of one
//     character or more, cut and marked, then the empty subject, which
//     the built-in templates replace by the NoSubject notice.
//
// Each step is a binary search over the number of attachments or
// characters kept: at most about log2 of that number renderings. The body
// is cut before escaping, so a cut never splits an entity, and the
// rendering is measured after escaping, because escaping grows the text by
// a factor that depends on the characters. The search relies on the
// template growing with the body, the subject and the attachments, which
// holds for every template that writes them once or more.
//
// A template from the configuration whose output exceeds its size limit
// counts as too long here, as long as there is a limit, so that a body of
// megabytes is cut instead of failing the template.
//
// When nothing fits, because the rest of the template is too long or
// ignores the body, the subject and the attachments, a template parsed for
// a format in strictFormats gives ErrLimitTooSmall, inside a
// *TemplateError for a template from the configuration. Any other gives the
// longest prefix that fits of the rendering with the cut subject, the
// Truncated notice and no attachment: for Telegram HTML cut between tags
// and entities with the open tags closed, by text.CutTelegramHTML, else
// cut at a character boundary.
func Fit(t *Template, d Data, limit int, measure func(string) int) (out string, truncated bool, err error) {
	return FitLines(t, d, limit, 0, measure)
}

// FitLines is Fit for a target that also takes at most maxLines lines of
// the body; 0 or less means no such limit. A body of more lines is cut
// after line maxLines and ended by the Truncated notice before Fit cuts
// anything else, and the result counts as truncated even when it then
// fits the length limit.
func FitLines(t *Template, d Data, limit, maxLines int, measure func(string) int) (out string, truncated bool, err error) {
	body, isCut := firstLines(d.Body, maxLines)
	if isCut {
		d.Body = withNotice(body, d.Strings.Truncated)
	}
	full, err := t.Execute(d)
	isOverOutputLimit := limit > 0 && errors.Is(err, errOutputLimit)
	if isOverOutputLimit {
		err = nil
	}
	if err != nil || limit <= 0 || (!isOverOutputLimit && measure(full) <= limit) {
		return full, isCut, err
	}
	render := func() (string, bool, error) {
		out, err := t.Execute(d)
		if errors.Is(err, errOutputLimit) {
			return "", false, nil
		}
		return out, err == nil && measure(out) <= limit, err
	}
	subject, attachments := d.Subject, d.Attachments
	subjectLength := utf8.RuneCountInString(subject)
	measureSubject := measure
	if escape, ok := subjectEscapers[t.format]; ok {
		measureSubject = func(s string) int { return measure(escape(s)) }
	}
	if share := max(limit/4, minSubjectShare); measureSubject(subject) > share {
		subjectLength = longestSubject(subject, share, measureSubject)
		d.Subject = cutSubject(subject, subjectLength)
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
		d.Subject = cutSubject(subject, kept+1)
		return render()
	}
	if best, found, err := longestFit(subjectLength-1, renderSubject); err != nil || found {
		return best, true, err
	}
	d.Subject = ""
	if out, fits, err := render(); err != nil || fits {
		return out, true, err
	}
	if strictFormats[t.format] && t.user != nil {
		return "", false, &TemplateError{Err: ErrLimitTooSmall}
	}
	if strictFormats[t.format] {
		return "", false, ErrLimitTooSmall
	}
	d.Subject = cutSubject(subject, subjectLength)
	shortest, err := t.Execute(d)
	if err != nil {
		return "", false, err
	}
	if t.format == text.FormatTelegramHTML {
		return text.CutTelegramHTML(shortest, limit, measure), true, nil
	}
	return longestPrefix(shortest, limit, measure), true, nil
}

// firstLines returns the first maxLines lines of body, without the line
// break after the last one, and whether anything but white space follows;
// body itself when maxLines is 0 or less or nothing is cut.
func firstLines(body string, maxLines int) (string, bool) {
	if maxLines <= 0 {
		return body, false
	}
	end := 0
	for range maxLines {
		next := strings.IndexByte(body[end:], '\n')
		if next < 0 {
			return body, false
		}
		end += next + 1
	}
	if strings.TrimSpace(body[end:]) == "" {
		return body, false
	}
	return body[:end-1], true
}

// subjectEscapers escape a subject the way the built-in template of each
// format does before Fit measures it for its share: a measure that parses
// markup, as text.MeasureTelegramHTML does, would take "<root@host>" in a
// raw subject for a tag and count it as nothing. A format without an entry
// writes the subject as it is. The JSON quoting of mattermost,
// slack-webhook and generic-json is not measured here: a subject of
// quotes, backslashes or control characters grows up to six times in JSON,
// takes more than its share and leaves the body less room, while the limit
// still holds.
var subjectEscapers = map[text.Format]func(string) string{
	text.FormatTelegramHTML:       text.EscapeTelegramHTML,
	text.FormatTelegramMarkdownV2: text.EscapeTelegramMarkdownV2,
	text.FormatSlackMrkdwn:        text.EscapeSlack,
	text.FormatSlackWebhook:       text.EscapeSlack,
	text.FormatDiscord:            text.EscapeDiscord,
	text.FormatMattermost:         text.EscapeMattermostMarkdown,
}

// minSubjectShare is the length, in the unit of the target, to which Fit
// cuts a long subject at least, however small the limit: a few words that
// still name the event.
const minSubjectShare = 64

// subjectCutMark ends a subject that Fit has cut, so that the reader does
// not take the rest for the whole subject.
const subjectCutMark = "..."

// cutSubject returns subject cut to at most n characters by
// text.CutAtWord, followed by subjectCutMark when anything was cut.
func cutSubject(subject string, n int) string {
	cut := text.CutAtWord(subject, n)
	if cut == subject {
		return subject
	}
	return cut + subjectCutMark
}

// longestSubject returns the largest number of characters, one at least,
// that cutSubject keeps of subject within share by measure.
func longestSubject(subject string, share int, measure func(string) int) int {
	low, high := 1, utf8.RuneCountInString(subject)
	for low < high {
		middle := low + (high-low+1)/2
		if measure(cutSubject(subject, middle)) <= share {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

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
