package spool

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/message"
)

var testNow = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func TestNextAttempt(t *testing.T) {
	cases := []struct {
		name       string
		attempts   int
		retryAfter time.Duration
		want       time.Duration
	}{
		{"T-ADJ-23/first-failure-waits-60s", 1, 0, time.Minute},
		{"second-failure-doubles", 2, 0, 2 * time.Minute},
		{"tenth-failure", 10, 0, 512 * time.Minute},
		{"eleventh-failure-below-cap", 11, 0, 1024 * time.Minute},
		{"T-ADJ-23/capped-at-24h", 12, 0, 24 * time.Hour},
		{"capped-far-out", 1000, 0, 24 * time.Hour},
		{"zero-attempts-as-first", 0, 0, time.Minute},
		{"retry-after-later-wins", 1, 5 * time.Minute, 5 * time.Minute},
		{"retry-after-earlier-ignored", 3, time.Minute, 4 * time.Minute},
		{"retry-after-beyond-cap", 20, 48 * time.Hour, 48 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextAttempt(testNow, tc.attempts, tc.retryAfter).Sub(testNow); got != tc.want {
				t.Errorf("NextAttempt(%d, %v) = now + %v, want now + %v", tc.attempts, tc.retryAfter, got, tc.want)
			}
		})
	}
}

func TestExpired(t *testing.T) {
	e := &Entry{CreatedAt: testNow}
	const ttl = 7 * 24 * time.Hour
	if Expired(e, testNow.Add(ttl), ttl) {
		t.Error("expired at exactly the TTL")
	}
	if !Expired(e, testNow.Add(ttl+time.Second), ttl) {
		t.Error("not expired past the TTL")
	}
	e.ReleasedAt = testNow.Add(6 * 24 * time.Hour)
	if Expired(e, testNow.Add(ttl+time.Second), ttl) {
		t.Error("released entry expired counting from CreatedAt")
	}
	if !Expired(e, e.ReleasedAt.Add(ttl+time.Second), ttl) {
		t.Error("released entry not expired past the TTL from ReleasedAt")
	}
}

func TestEntryMarks(t *testing.T) {
	e := NewEntry(NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a", "b", "c"})
	e.MarkRetry("a", testNow, 0, "temp", "status 503")
	e.MarkRetry("a", testNow, 0, "temp", strings.Repeat("x", 600)+"é")
	if got := e.Targets["a"]; got.State != Pending || got.Attempts != 2 || !got.NextAt.Equal(testNow.Add(2*time.Minute)) || len(got.LastError) != maxErrorLength {
		t.Errorf("a = %+v, want pending, 2 attempts, next in 2m, error cut to %d bytes", got, maxErrorLength)
	}
	e.MarkDone("a")
	e.MarkFailed("b", "perm", "status 400")
	if got := e.Targets["a"]; got.State != Done || got.Attempts != 2 || got.LastError != "" {
		t.Errorf("a = %+v, want done after 2 attempts", got)
	}
	if !e.IsPending() {
		t.Error("c is pending")
	}
	e.MarkDone("c")
	if e.IsPending() {
		t.Error("nothing is pending")
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID(testNow), NewID(testNow)
	if a == b || !validID.MatchString(a) || !strings.HasPrefix(a, "1790503200000000000-") {
		t.Errorf("NewID = %q, %q", a, b)
	}
	if compareIDs(NewID(testNow), NewID(testNow.Add(time.Nanosecond))) >= 0 {
		t.Error("ids do not sort by time")
	}
	if compareIDs("9-0000000000000000", "10-0000000000000000") >= 0 {
		t.Error("ids sort as text, not as numbers")
	}
}

func TestEncodeDecode(t *testing.T) {
	e := NewEntry(NewID(testNow), 1000, testNow, testNow.Add(-time.Second),
		message.Envelope{Sender: "root@example.org", SenderName: "Cron", Recipients: []string{"a@example.org"}}, []string{"a", "b"})
	e.MarkRetry("a", testNow, 0, "temp", "status 503")
	data, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Errorf("Decode(Encode(e)) = %+v, want %+v", got, e)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid := `{"version":1,"id":"1-0123456789abcdef","owner_uid":0,"created_at":"2026-09-27T10:00:00Z","received_at":"2026-09-27T10:00:00Z","envelope":{}`
	cases := []struct {
		name, doc string
		want      error
	}{
		{"newer-version", `{"version":2,"future":true}`, ErrUnknownVersion},
		{"no-version", `{"id":"1-0123456789abcdef"}`, ErrUnknownVersion},
		{"not-json", `{"version":1`, ErrCorrupt},
		{"unknown-field", valid + `,"extra":1}`, ErrCorrupt},
		{"bad-id", strings.Replace(valid, "0123456789abcdef", "../../etc/x", 1) + `}`, ErrCorrupt},
		{"unknown-state", valid + `,"targets":{"a":{"state":"sent"}}}`, ErrCorrupt},
		{"null-state", valid + `,"targets":{"a":null}}`, ErrCorrupt},
		{"trailing-data", valid + `}{}`, ErrCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.doc)); !errors.Is(err, tc.want) {
				t.Errorf("Decode() error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := Decode([]byte(valid + `}`)); err != nil {
		t.Errorf("Decode(valid) error = %v", err)
	}
}

// FuzzDecode pins that Decode never panics and that an accepted sidecar
// survives Encode and Decode unchanged, so that a rewrite by a queue run
// never alters an entry it does not mean to.
func FuzzDecode(f *testing.F) {
	e := NewEntry("1-0123456789abcdef", 1000, testNow, testNow, message.Envelope{Recipients: []string{"a@example.org"}}, []string{"a"})
	e.MarkRetry("a", testNow, time.Minute, "temp", "status 429")
	seed, err := Encode(e)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"version":1,"id":"1-0123456789abcdef","targets":{"x":{"state":"done"}}}`))
	f.Add([]byte(`{"version":2}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := Decode(data)
		if err != nil {
			return
		}
		again, err := Encode(e)
		if err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		decoded, err := Decode(again)
		if err != nil {
			t.Fatalf("Decode(Encode()) error = %v", err)
		}
		first, _ := json.Marshal(e)
		second, _ := json.Marshal(decoded)
		if string(first) != string(second) {
			t.Errorf("round trip changed the entry:\n%s\n%s", first, second)
		}
	})
}
