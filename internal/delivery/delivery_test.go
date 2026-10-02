package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
)

type fakeSender struct {
	caps backend.Caps
	err  error
	// send, when set, runs instead of returning err.
	send func(ctx context.Context, p backend.Payload) error
	sent []backend.Payload
}

func (f *fakeSender) Caps() backend.Caps { return f.caps }

func (f *fakeSender) Send(ctx context.Context, p backend.Payload) error {
	f.sent = append(f.sent, p)
	if f.send != nil {
		return f.send(ctx, p)
	}
	return f.err
}

// plainTemplate returns the built-in plain template.
func plainTemplate(t *testing.T) *render.Template {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// testData returns template data with a subject and body.
func testData() render.Data {
	return render.Data{Subject: "s", Hostname: "h", Body: "b\n", Strings: render.DefaultStrings()}
}

func TestDeliver(t *testing.T) {
	temporary := &backend.Error{Class: backend.Temporary, Err: errors.New("timeout")}
	permanent := &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("bad request")}
	partial := &backend.Error{Class: backend.Permanent, Status: 413, Err: errors.New("file too large"), IsPartial: true}
	contractBreach := errors.New("plain error")
	senders := []*fakeSender{{}, {err: temporary}, {err: permanent}, {err: contractBreach}, {err: partial}}
	ids := []string{"ok", "temp", "perm", "plain", "partial"}
	var targets []Target
	for i, sender := range senders {
		targets = append(targets, Target{ID: ids[i], Sender: sender, Template: plainTemplate(t)})
	}

	results := Deliver(context.Background(), targets, testData(), nil)

	want := []Result{
		{TargetID: "ok", Status: OK},
		{TargetID: "temp", Status: Temp, Err: temporary},
		{TargetID: "perm", Status: Perm, Err: permanent},
		{TargetID: "plain", Status: Perm, Err: contractBreach},
		{TargetID: "partial", Status: OK, Err: partial},
	}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(want))
	}
	wantText, err := plainTemplate(t).Execute(testData())
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("results[%d] = %+v, want %+v", i, results[i], want[i])
		}
		if len(senders[i].sent) != 1 || senders[i].sent[0].Text != wantText || senders[i].sent[0].Title != "s" {
			t.Errorf("target %q got payloads %+v, want exactly one with the plain rendering", want[i].TargetID, senders[i].sent)
		}
	}
}

// TestDeliverInParallel pins that a slow target does not hold the others
// back: every sender waits until all of them have been called.
func TestDeliverInParallel(t *testing.T) {
	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	var targets []Target
	for i := range n {
		sender := &fakeSender{send: func(context.Context, backend.Payload) error {
			started.Done()
			started.Wait()
			return nil
		}}
		targets = append(targets, Target{ID: fmt.Sprint(i), Sender: sender, Template: plainTemplate(t)})
	}
	done := make(chan []Result)
	go func() { done <- Deliver(context.Background(), targets, testData(), nil) }()
	select {
	case results := <-done:
		if ExitCode(results, QueueOff) != 0 {
			t.Errorf("results = %+v", results)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deliver calls the targets one after another")
	}
}

// TestDeliverEachReportsEarly pins that the result of a finished target
// reaches the caller while another target still runs: the spool records
// it at once, so a crash later does not repeat that target.
func TestDeliverEachReportsEarly(t *testing.T) {
	fastDone := make(chan struct{})
	fast := &fakeSender{send: func(context.Context, backend.Payload) error { return nil }}
	slow := &fakeSender{send: func(ctx context.Context, _ backend.Payload) error {
		select {
		case <-fastDone:
			return nil
		case <-ctx.Done():
			return &backend.Error{Class: backend.Temporary, Err: ctx.Err()}
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var order []string
	results := DeliverEach(ctx, []Target{{ID: "slow", Sender: slow, Template: plainTemplate(t)}, {ID: "fast", Sender: fast, Template: plainTemplate(t)}}, testData(), nil, nil, func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, r.TargetID)
		if r.TargetID == "fast" {
			close(fastDone)
		}
	})
	if results[0].Status != OK || results[1].Status != OK || !slices.Equal(order, []string{"fast", "slow"}) {
		t.Errorf("results = %+v, done order %v, want both OK, fast first", results, order)
	}
}

// TestDeliverRecoversPanic pins that a panic for one target becomes its
// permanent failure while the other target delivers.
func TestDeliverRecoversPanic(t *testing.T) {
	t.Run("panic-in-sender", func(t *testing.T) {
		panicking := &fakeSender{send: func(context.Context, backend.Payload) error { panic("bug in sender") }}
		results := Deliver(context.Background(), []Target{
			{ID: "bad", Sender: panicking, Template: plainTemplate(t)},
			{ID: "good", Sender: &fakeSender{}, Template: plainTemplate(t)},
		}, testData(), nil)
		if results[0].Status != Perm || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "bug in sender") {
			t.Errorf("panicking target: %+v", results[0])
		}
		if results[1].Status != OK || ExitCode(results, QueueOff) != 0 {
			t.Errorf("other target: %+v", results[1])
		}
	})
	t.Run("panic-in-rendering", func(t *testing.T) {
		results := Deliver(context.Background(), []Target{{ID: "no-template", Sender: &fakeSender{}}}, testData(), nil)
		if results[0].Status != Perm || results[0].Err == nil || !strings.HasPrefix(results[0].Err.Error(), "panic: ") {
			t.Errorf("result = %+v", results[0])
		}
	})
}

// TestDeliverFitsText pins that the text is fitted to the limit of the
// target, in its unit, with the result marked as truncated.
func TestDeliverFitsText(t *testing.T) {
	sender := &fakeSender{caps: backend.Caps{MaxText: 200, Measure: text.ByteLen}}
	d := testData()
	d.Body = strings.Repeat("ёжик ", 200)
	results := Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, d, nil)
	if !results[0].IsTruncated || len(sender.sent) != 1 {
		t.Fatalf("result = %+v", results[0])
	}
	if out := sender.sent[0].Text; len(out) > 200 || !strings.HasSuffix(out, "[truncated, 1.8 KiB in full]") {
		t.Errorf("text of %d bytes: %q", len(out), out)
	}
	sender = &fakeSender{caps: backend.Caps{MaxText: 100}}
	results = Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, testData(), nil)
	if results[0].IsTruncated {
		t.Errorf("short text marked as truncated: %+v", results[0])
	}
}

// TestDeliverLongText pins the policies for a text over the limit: what
// the text says and which file goes along.
func TestDeliverLongText(t *testing.T) {
	withFiles := backend.Caps{MaxText: 300, MaxFiles: 10}
	raw := []byte("To: a@example.org\nBcc: hidden@example.org\nSubject: s\n\n" + longBody)
	t.Run("T-LIM-16/file-ahead-of-attachments", longTextCase{Target{}, withFiles, nil, []string{"message.txt", "a.log"}, "[truncated]\nmessage.txt (text/plain; charset=utf-8, 1.9 KiB)\na.log"}.check)
	t.Run("truncate-notes-full-size", longTextCase{Target{OnLong: OnLongTruncate}, withFiles, nil, []string{"a.log"}, "[truncated, 1.9 KiB in full]\na.log"}.check)
	t.Run("text-only-target-notes-full-size", longTextCase{Target{}, backend.Caps{MaxText: 300}, nil, nil, "[truncated, 1.9 KiB in full]"}.check)
	t.Run("file-over-size-notes-full-size", longTextCase{Target{}, backend.Caps{MaxText: 300, MaxFiles: 10, MaxFileSize: 1000}, nil, []string{"a.log"}, "[truncated, 1.9 KiB in full]"}.check)
	t.Run("eml-without-bcc", longTextCase{Target{LongFile: LongFileMessage}, withFiles, raw, []string{"message.eml", "a.log"}, "message.eml (message/rfc822"}.check)
	t.Run("eml-unknown-gives-text", longTextCase{Target{LongFile: LongFileMessage}, withFiles, nil, []string{"message.txt", "a.log"}, "message.txt"}.check)
	t.Run("max-file-size-over-full-text-notes-full-size", longTextCase{Target{MaxFileSize: 1000}, withFiles, nil, []string{"a.log"}, "[truncated, 1.9 KiB in full]"}.check)
	t.Run("max-text-replaces-the-limit", longTextCase{Target{MaxText: 200}, backend.Caps{MaxText: 4000, MaxFiles: 10}, nil, []string{"message.txt", "a.log"}, "message.txt"}.check)
	t.Run("T-LIM-19/max-lines-then-file", longTextCase{Target{MaxLines: 3}, backend.Caps{MaxText: 4000, MaxFiles: 10}, nil, []string{"message.txt", "a.log"}, "a line of the body\na line of the body\na line of the body\n[truncated]\nmessage.txt"}.check)
}

// TestDeliverSharesLongFile pins that the full text is built once for
// all targets: every target gets the same bytes, not a copy each.
func TestDeliverSharesLongFile(t *testing.T) {
	d := testData()
	d.Body = longBody
	senders := []*fakeSender{{caps: backend.Caps{MaxText: 300, MaxFiles: 10}}, {caps: backend.Caps{MaxText: 200, MaxFiles: 1}}}
	var targets []Target
	for i, sender := range senders {
		targets = append(targets, Target{ID: fmt.Sprint(i), Sender: sender, Template: plainTemplate(t)})
	}
	Deliver(context.Background(), targets, d, nil)
	first, second := senders[0].sent[0].Attachments, senders[1].sent[0].Attachments
	if len(first) != 1 || len(second) != 1 || &first[0].Data[0] != &second[0].Data[0] {
		t.Errorf("files %d and %d, want one message.txt shared", len(first), len(second))
	}
}

// TestDeliverBuildsOnlyNeededLongFile pins that a target builds only the
// long file it uses: message.eml sent leaves message.txt unbuilt, and a
// cut text without its file builds no message.eml.
func TestDeliverBuildsOnlyNeededLongFile(t *testing.T) {
	raw := []byte("To: a@example.org\nSubject: s\n\n" + longBody)
	for _, tc := range []struct {
		name                  string
		target                Target
		caps                  backend.Caps
		wantText, wantMessage bool
	}{
		{"eml-sent-without-text-file", Target{LongFile: LongFileMessage}, backend.Caps{MaxText: 300, MaxFiles: 10}, false, true},
		{"truncate-without-eml", Target{LongFile: LongFileMessage, OnLong: OnLongTruncate}, backend.Caps{MaxText: 300, MaxFiles: 10}, true, false},
		{"text-only-target-without-eml", Target{LongFile: LongFileMessage}, backend.Caps{MaxText: 300}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testData()
			d.Body = longBody
			long := newLongFiles(d, raw)
			buildText, buildMessage := long.text, long.message
			isTextBuilt, isMessageBuilt := false, false
			long.text = func() (*backend.Attachment, error) { isTextBuilt = true; return buildText() }
			long.message = func() *backend.Attachment { isMessageBuilt = true; return buildMessage() }
			tc.target.ID, tc.target.Sender, tc.target.Template = "t", &fakeSender{caps: tc.caps}, plainTemplate(t)
			if result := deliverOne(context.Background(), tc.target, d, nil, long); result.Status != OK || !result.IsTruncated {
				t.Fatalf("result = %+v", result)
			}
			if isTextBuilt != tc.wantText || isMessageBuilt != tc.wantMessage {
				t.Errorf("message.txt built %v, message.eml built %v; want %v, %v", isTextBuilt, isMessageBuilt, tc.wantText, tc.wantMessage)
			}
		})
	}
}

// longBody is a body of 1900 bytes.
var longBody = strings.Repeat("a line of the body\n", 100)

// longTextCase delivers longBody with one attachment to target with caps
// and the raw message raw, and wants the files wantFiles and wantText in
// the text.
type longTextCase struct {
	target    Target
	caps      backend.Caps
	raw       []byte
	wantFiles []string
	wantText  string
}

func (tc longTextCase) check(t *testing.T) {
	d := testData()
	d.Body = longBody
	d.Attachments = []render.Attachment{{Name: "a.log", ContentType: "text/plain", Size: 8}}
	files := []message.Attachment{{Name: "a.log", ContentType: "text/plain", Data: []byte("attached")}}
	sender := &fakeSender{caps: tc.caps}
	tc.target.ID, tc.target.Sender, tc.target.Template = "t", sender, plainTemplate(t)
	result := DeliverEach(context.Background(), []Target{tc.target}, d, files, tc.raw, nil)[0]
	if result.Status != OK || !result.IsTruncated || len(sender.sent) != 1 {
		t.Fatalf("result = %+v", result)
	}
	p := sender.sent[0]
	var names []string
	for _, file := range p.Attachments {
		names = append(names, file.Name)
	}
	if !slices.Equal(names, tc.wantFiles) {
		t.Errorf("files %q, want %q", names, tc.wantFiles)
	}
	if limit := cmp.Or(tc.target.MaxText, tc.caps.MaxText); text.RuneCount(p.Text) > limit {
		t.Errorf("text of %d characters, limit %d", text.RuneCount(p.Text), limit)
	}
	if !strings.Contains(p.Text, tc.wantText) {
		t.Errorf("text lacks %q:\n%s", tc.wantText, p.Text)
	}
	if len(p.Attachments) > 1 {
		full := string(p.Attachments[0].Data)
		if !strings.Contains(full, longBody) || strings.Contains(full, "hidden@example.org") {
			t.Errorf("%s of %d bytes lacks the body or keeps the Bcc", names[0], len(full))
		}
	}
}

// TestDeliverRetriesRejectedText pins the retry after a rejected text:
// once, with the full text as a file and no text, and only for a target
// that takes files.
func TestDeliverRetriesRejectedText(t *testing.T) {
	rejected := &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("can't parse entities"), IsTextRejected: true}
	rejectText := func(_ context.Context, p backend.Payload) error {
		if p.Text != "" {
			return rejected
		}
		return nil
	}
	t.Run("T-ADJ-49/retried-once-as-file", func(t *testing.T) {
		sender := &fakeSender{caps: backend.Caps{MaxFiles: 10}, send: rejectText}
		result := Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t), OnLong: OnLongTruncate}}, testData(), nil)[0]
		if result.Status != OK || result.Err != nil || result.TextRejected != rejected || len(sender.sent) != 2 {
			t.Fatalf("result = %+v, %d sends", result, len(sender.sent))
		}
		if retry := sender.sent[1]; retry.Text != "" || len(retry.Attachments) != 1 || retry.Attachments[0].Name != "message.txt" || retry.Title != "s" {
			t.Errorf("retry = %+v", retry)
		}
	})
	t.Run("T-ADJ-49/second-rejection-not-retried", func(t *testing.T) {
		sender := &fakeSender{caps: backend.Caps{MaxFiles: 10}, err: rejected}
		result := Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, testData(), nil)[0]
		if result.Status != Perm || len(sender.sent) != 2 {
			t.Errorf("result = %+v, %d sends", result, len(sender.sent))
		}
	})
	t.Run("text-only-target-not-retried", func(t *testing.T) {
		sender := &fakeSender{err: rejected}
		result := Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, testData(), nil)[0]
		if result.Status != Perm || result.TextRejected != nil || len(sender.sent) != 1 {
			t.Errorf("result = %+v, %d sends", result, len(sender.sent))
		}
	})
}

// TestDeliverSelectsFiles pins which attachments a target sends: in order,
// within the number, size and total size it accepts; the rest are marked
// in the text. A target without file support sends none and marks none.
func TestDeliverSelectsFiles(t *testing.T) {
	d := testData()
	var files []message.Attachment
	for i, size := range []int{10, 40, 10, 10, 10} {
		name := fmt.Sprintf("f%d.log", i)
		if i == 3 {
			name = ""
		}
		files = append(files, message.Attachment{Name: name, ContentType: "text/plain", Size: int64(size), Data: make([]byte, size)})
		d.Attachments = append(d.Attachments, render.Attachment{Name: name, ContentType: "text/plain", Size: int64(size)})
	}
	cases := []struct {
		name        string
		caps        backend.Caps
		maxFileSize int64
		wantSent    []string
		wantMarks   int
	}{
		{"T-LIM-09/at-most-max-files", backend.Caps{MaxFiles: 2}, 0, []string{"f0.log", "f1.log"}, 3},
		{"T-LIM-07/file-over-size-skipped", backend.Caps{MaxFiles: 10, MaxFileSize: 20}, 0, []string{"f0.log", "f2.log", "attachment-4", "f4.log"}, 1},
		{"T-LIM-09/request-over-total-skipped", backend.Caps{MaxFiles: 10, MaxFilesSize: 60}, 0, []string{"f0.log", "f1.log", "f2.log"}, 2},
		{"max-file-size-replaces-the-limit", backend.Caps{MaxFiles: 10, MaxFileSize: 100}, 20, []string{"f0.log", "f2.log", "attachment-4", "f4.log"}, 1},
		{"text-only-target", backend.Caps{}, 0, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{caps: tc.caps}
			Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t), MaxFileSize: tc.maxFileSize}}, d, files)
			var sent []string
			for _, file := range sender.sent[0].Attachments {
				sent = append(sent, file.Name)
			}
			if !slices.Equal(sent, tc.wantSent) {
				t.Errorf("sent %q, want %q", sent, tc.wantSent)
			}
			if !strings.Contains(sender.sent[0].Text, "\nattachment-4 (text/plain") {
				t.Errorf("text does not list the unnamed file as it is sent:\n%s", sender.sent[0].Text)
			}
			if got := strings.Count(sender.sent[0].Text, "[not sent]"); got != tc.wantMarks {
				t.Errorf("%d attachments marked, want %d:\n%s", got, tc.wantMarks, sender.sent[0].Text)
			}
		})
	}
	if slices.ContainsFunc(d.Attachments, func(a render.Attachment) bool { return a.IsSkipped }) {
		t.Error("Deliver marked the caller's attachments")
	}
}

// TestExitCode has one subtest per row of the exit status matrix, named
// after the situation.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name     string
		statuses []Status
		queue    Queue
		want     int
	}{
		{"T-MTA-36/temp-and-entry-not-created", []Status{Temp, Temp}, QueueNotCreated, 73},
		{"ok-temp-and-entry-not-created", []Status{OK, Temp}, QueueNotCreated, 73},
		{"T-MTA-36/temp-and-entry-not-written", []Status{Temp, OK}, QueueNotWritten, 74},
		{"ok-and-entry-not-created", []Status{OK, Perm}, QueueNotCreated, 0},
		{"all-ok", []Status{OK, OK}, QueueOff, 0},
		{"ok-and-suppressed", []Status{Suppressed, OK}, Queued, 0},
		{"all-suppressed", []Status{Suppressed, Suppressed}, QueueOff, 0},
		{"ok-and-temp-queued", []Status{Temp, OK}, Queued, 0},
		{"ok-and-perm", []Status{OK, Perm}, Queued, 0},
		{"perm-and-temp-queued", []Status{Temp, Perm}, Queued, 69},
		{"all-perm", []Status{Perm, Perm}, Queued, 69},
		{"suppressed-and-perm", []Status{Suppressed, Perm}, QueueOff, 69},
		{"T-MTA-36/all-temp-queued", []Status{Temp, Temp}, Queued, 0},
		{"temp-and-suppressed-queued", []Status{Temp, Suppressed}, Queued, 0},
		{"all-temp-without-spool", []Status{Temp, Temp}, QueueOff, 69},
		{"ok-and-temp-without-spool", []Status{Temp, OK}, QueueOff, 0},
		{"perm-and-temp-without-spool", []Status{Temp, Perm}, QueueOff, 69},
		{"no-results", nil, QueueOff, 69},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]Result, len(tc.statuses))
			for i, s := range tc.statuses {
				results[i] = Result{Status: s}
			}
			if got := ExitCode(results, tc.queue); got != tc.want {
				t.Errorf("ExitCode(%v, %d) = %d, want %d", tc.statuses, tc.queue, got, tc.want)
			}
		})
	}
}

// TestDeliverEachPassesMessage pins that a target with CanTakeMessage
// gets the message without its blind copies, the same bytes for every
// such target, and that another target gets none.
func TestDeliverEachPassesMessage(t *testing.T) {
	raw := []byte("From: a@example.org\nTo: b@example.org\nBcc: hidden@example.org\nSubject: s\n\nbody\n")
	d := testData()
	d.From, d.Recipients, d.MessageID = message.Address{Name: "A", Addr: "a@example.org"}, []string{"b@example.org"}, "<1@example.org>"
	first, second := &fakeSender{caps: backend.Caps{CanTakeMessage: true}}, &fakeSender{caps: backend.Caps{CanTakeMessage: true}}
	other := &fakeSender{}
	var targets []Target
	for i, s := range []*fakeSender{first, second, other} {
		targets = append(targets, Target{ID: fmt.Sprint(i), Sender: s, Template: plainTemplate(t)})
	}
	DeliverEach(context.Background(), targets, d, nil, raw, nil)

	got := first.sent[0].Message
	if got == nil {
		t.Fatal("target with CanTakeMessage got no message")
	}
	if want := string(message.WithoutBlindCopies(raw)); string(got.Raw) != want || strings.Contains(want, "hidden") {
		t.Errorf("Raw = %q, want %q without the Bcc field", got.Raw, want)
	}
	if got.Subject != "s" || got.From != "a@example.org" || !slices.Equal(got.To, []string{"b@example.org"}) || got.MessageID != "<1@example.org>" || got.Hostname != "h" {
		t.Errorf("Message = %+v", got)
	}
	if second.sent[0].Message != got {
		t.Error("targets got separate copies of the message")
	}
	if other.sent[0].Message != nil {
		t.Error("target without CanTakeMessage got the message")
	}
}

// userTemplate parses source as a template from the configuration for
// the plain format.
func userTemplate(t *testing.T, source string) *render.Template {
	t.Helper()
	tmpl, err := render.Parse("custom", source, text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// TestDeliverFallsBackToBuiltin pins that a failed template from the
// configuration costs the message nothing: the built-in template renders
// the text, the result carries the template error, and the status is
// that of the delivery.
func TestDeliverFallsBackToBuiltin(t *testing.T) {
	builtin, err := plainTemplate(t).Execute(testData())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("T-TPL-05/execution-error", fallbackCase{"{{ index .To 3 }}", builtin}.check)
	t.Run("T-ADJ-58/blank-output", fallbackCase{"{{ .MessageID }}\n", builtin}.check)
	t.Run("user-text-sent", func(t *testing.T) {
		sender := &fakeSender{}
		target := Target{ID: "t", Sender: sender, Template: userTemplate(t, "custom {{ .Subject }}"), Fallback: plainTemplate(t)}
		result := Deliver(context.Background(), []Target{target}, testData(), nil)[0]
		if result.Status != OK || result.TemplateErr != nil || len(sender.sent) != 1 || sender.sent[0].Text != "custom s" {
			t.Errorf("result = %+v, sent %+v", result, sender.sent)
		}
	})
	t.Run("shared-template-in-parallel", func(t *testing.T) {
		tmpl := userTemplate(t, "{{ index .To 3 }}")
		var targets []Target
		for i := range 8 {
			targets = append(targets, Target{ID: fmt.Sprint(i), Sender: &fakeSender{}, Template: tmpl, Fallback: plainTemplate(t)})
		}
		for _, result := range Deliver(context.Background(), targets, testData(), nil) {
			if result.Status != OK || result.TemplateErr == nil {
				t.Errorf("result = %+v", result)
			}
		}
	})
}

// fallbackCase is a template from the configuration that fails for
// testData, and the built-in text the target gets instead.
type fallbackCase struct {
	source, builtin string
}

func (c fallbackCase) check(t *testing.T) {
	sender := &fakeSender{}
	target := Target{ID: "t", Sender: sender, Template: userTemplate(t, c.source), Fallback: plainTemplate(t)}
	result := Deliver(context.Background(), []Target{target}, testData(), nil)[0]
	var templateErr *render.TemplateError
	if result.Status != OK || result.Err != nil || !errors.As(result.TemplateErr, &templateErr) {
		t.Fatalf("result = %+v", result)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != c.builtin {
		t.Errorf("sent = %+v, want the built-in text %q", sender.sent, c.builtin)
	}
	if code := ExitCode([]Result{result}, QueueOff); code != 0 {
		t.Errorf("exit code %d", code)
	}
}

// TestDeliverRetriesRejectedUserText pins the order after a target
// rejects the text of a template from the configuration: the text of the
// built-in template once, then the full text as a file alone.
func TestDeliverRetriesRejectedUserText(t *testing.T) {
	rejected := &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("can't parse entities"), IsTextRejected: true}
	builtin, err := plainTemplate(t).Execute(testData())
	if err != nil {
		t.Fatal(err)
	}
	newTarget := func(sender *fakeSender) Target {
		return Target{ID: "t", Sender: sender, Template: userTemplate(t, "<b>{{ .Subject }}"), Fallback: plainTemplate(t)}
	}
	t.Run("builtin-text-then-file", func(t *testing.T) {
		sender := &fakeSender{caps: backend.Caps{MaxFiles: 10}, send: func(_ context.Context, p backend.Payload) error {
			if p.Text != "" {
				return rejected
			}
			return nil
		}}
		result := Deliver(context.Background(), []Target{newTarget(sender)}, testData(), nil)[0]
		if result.Status != OK || result.TemplateErr != rejected || result.TextRejected != rejected || len(sender.sent) != 3 {
			t.Fatalf("result = %+v, %d sends", result, len(sender.sent))
		}
		if first := sender.sent[0].Text; first != "<b>s" {
			t.Errorf("first text = %q", first)
		}
		if second := sender.sent[1].Text; second != builtin {
			t.Errorf("second text = %q, want %q", second, builtin)
		}
		if third := sender.sent[2]; third.Text != "" || len(third.Attachments) != 1 || third.Attachments[0].Name != "message.txt" {
			t.Errorf("third request = %+v", third)
		}
	})
	t.Run("builtin-text-accepted", func(t *testing.T) {
		sender := &fakeSender{send: func(_ context.Context, p backend.Payload) error {
			if p.Text == "<b>s" {
				return rejected
			}
			return nil
		}}
		result := Deliver(context.Background(), []Target{newTarget(sender)}, testData(), nil)[0]
		if result.Status != OK || result.Err != nil || result.TemplateErr != rejected || result.TextRejected != nil || len(sender.sent) != 2 || sender.sent[1].Text != builtin {
			t.Errorf("result = %+v, sent %+v", result, sender.sent)
		}
	})
	t.Run("text-only-target-rejects-both", func(t *testing.T) {
		sender := &fakeSender{err: rejected}
		result := Deliver(context.Background(), []Target{newTarget(sender)}, testData(), nil)[0]
		if result.Status != Perm || result.TemplateErr != rejected || result.TextRejected != nil || len(sender.sent) != 2 {
			t.Errorf("result = %+v, %d sends", result, len(sender.sent))
		}
	})
}
