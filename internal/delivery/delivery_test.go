package delivery

import (
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
		if ExitCode(results) != 0 {
			t.Errorf("results = %+v", results)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deliver calls the targets one after another")
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
		if results[1].Status != OK || ExitCode(results) != 0 {
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
	if out := sender.sent[0].Text; len(out) > 200 || !strings.HasSuffix(out, "[truncated]") {
		t.Errorf("text of %d bytes: %q", len(out), out)
	}
	sender = &fakeSender{caps: backend.Caps{MaxText: 100}}
	results = Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, testData(), nil)
	if results[0].IsTruncated {
		t.Errorf("short text marked as truncated: %+v", results[0])
	}
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
		name      string
		caps      backend.Caps
		wantSent  []string
		wantMarks int
	}{
		{"T-LIM-09/at-most-max-files", backend.Caps{MaxFiles: 2}, []string{"f0.log", "f1.log"}, 3},
		{"T-LIM-07/file-over-size-skipped", backend.Caps{MaxFiles: 10, MaxFileSize: 20}, []string{"f0.log", "f2.log", "attachment-4", "f4.log"}, 1},
		{"T-LIM-09/request-over-total-skipped", backend.Caps{MaxFiles: 10, MaxFilesSize: 60}, []string{"f0.log", "f1.log", "f2.log"}, 2},
		{"text-only-target", backend.Caps{}, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{caps: tc.caps}
			Deliver(context.Background(), []Target{{ID: "t", Sender: sender, Template: plainTemplate(t)}}, d, files)
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

// TestExitCode has one subtest per row of the exit status matrix that
// applies without a spool, named after the situation.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name     string
		statuses []Status
		want     int
	}{
		{"all-ok", []Status{OK, OK}, 0},
		{"ok-and-suppressed", []Status{Suppressed, OK}, 0},
		{"all-suppressed", []Status{Suppressed, Suppressed}, 0},
		{"ok-and-perm", []Status{OK, Perm}, 0},
		{"ok-and-temp-without-spool", []Status{Temp, OK}, 0},
		{"all-perm", []Status{Perm, Perm}, 69},
		{"perm-and-temp", []Status{Temp, Perm}, 69},
		{"suppressed-and-perm", []Status{Suppressed, Perm}, 69},
		{"all-temp-without-spool", []Status{Temp, Temp}, 69},
		{"no-results", nil, 69},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]Result, len(tc.statuses))
			for i, s := range tc.statuses {
				results[i] = Result{Status: s}
			}
			if got := ExitCode(results); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.statuses, got, tc.want)
			}
		})
	}
}
