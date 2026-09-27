package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/6RUN0/slendmail/internal/backend"
)

type fakeSender struct {
	err  error
	sent []backend.Payload
}

func (f *fakeSender) Caps() backend.Caps { return backend.Caps{} }

func (f *fakeSender) Send(_ context.Context, p backend.Payload) error {
	f.sent = append(f.sent, p)
	return f.err
}

func TestDeliver(t *testing.T) {
	temporary := &backend.Error{Class: backend.Temporary, Err: errors.New("timeout")}
	permanent := &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("bad request")}
	contractBreach := errors.New("plain error")
	senders := []*fakeSender{{}, {err: temporary}, {err: permanent}, {err: contractBreach}}
	targets := []Target{
		{ID: "ok", Sender: senders[0]},
		{ID: "temp", Sender: senders[1]},
		{ID: "perm", Sender: senders[2]},
		{ID: "plain", Sender: senders[3]},
	}
	payload := backend.Payload{Title: "t", Text: "b"}

	results := Deliver(context.Background(), targets, payload)

	want := []Result{
		{TargetID: "ok", Status: OK},
		{TargetID: "temp", Status: Temp, Err: temporary},
		{TargetID: "perm", Status: Perm, Err: permanent},
		{TargetID: "plain", Status: Perm, Err: contractBreach},
	}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(want))
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("results[%d] = %+v, want %+v", i, results[i], want[i])
		}
		if len(senders[i].sent) != 1 || senders[i].sent[0] != payload {
			t.Errorf("target %q got payloads %+v, want exactly %+v", want[i].TargetID, senders[i].sent, payload)
		}
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
