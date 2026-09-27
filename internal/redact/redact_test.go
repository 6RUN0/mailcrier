package redact

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
)

const webhookURL = "https://user:hunter2-password@hooks.example.org/services/T0001/B0002/abcdefghijklmnopqrstuvwx?key=0123456789abcdef0123&lang=en"

func TestAddURLMasksQuotedParts(t *testing.T) {
	var r Redactor
	r.AddURL(webhookURL)
	cases := map[string]string{
		"whole":        `Post "` + webhookURL + `": EOF`,
		"request-uri":  `http2: Transport encoding header ":path" = "/services/T0001/B0002/abcdefghijklmnopqrstuvwx?key=0123456789abcdef0123&lang=en"`,
		"segment":      "channel abcdefghijklmnopqrstuvwx not found",
		"query-value":  "key 0123456789abcdef0123 revoked",
		"password":     "auth hunter2-password rejected",
		"path-no-host": "GET /services/T0001/B0002/abcdefghijklmnopqrstuvwx?key=0123456789abcdef0123&lang=en",
	}
	for name, text := range cases {
		got := r.String(text)
		for _, secret := range []string{"abcdefghijklmnopqrstuvwx", "0123456789abcdef0123", "hunter2-password"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: String(%q) = %q, still contains %q", name, text, got, secret)
			}
		}
		if !strings.Contains(got, Mask) {
			t.Errorf("%s: String(%q) = %q, want a mask", name, text, got)
		}
	}
	if got := r.String("POST to /services failed"); got != "POST to /services failed" {
		t.Errorf("short segments are masked: %q", got)
	}
}

func TestAddURLMasksLongHostLabels(t *testing.T) {
	var r Redactor
	r.AddURL("https://eo1a2b3c4d5e6f7g8h9i0j.m.pipedream.net/")
	got := r.String("dial tcp: lookup eo1a2b3c4d5e6f7g8h9i0j.m.pipedream.net: no such host; x509: not eo1a2b3c4d5e6f7g8h9i0j")
	if strings.Contains(got, "eo1a2b3c4d5e6f7g8h9i0j") {
		t.Errorf("host label not masked: %q", got)
	}
}

// TestAddURLKeepsShortParts pins that short parts of a URL stay readable
// elsewhere in the text: only the whole URL is masked.
func TestAddURLKeepsShortParts(t *testing.T) {
	var r Redactor
	r.AddURL("https://ntfy.example/s")
	if got := r.String("read /s from ntfy.example"); got != "read /s from ntfy.example" {
		t.Errorf("String() = %q", got)
	}
	if got := r.String("post https://ntfy.example/s"); got != "post ***" {
		t.Errorf("String() = %q", got)
	}
}

func TestLongestSecretWins(t *testing.T) {
	var r Redactor
	r.Add("abc")
	r.Add("abcdef")
	if got := r.String("x abcdef y abc"); got != "x *** y ***" {
		t.Errorf("String() = %q", got)
	}
}

func TestZeroValueMasksNothing(t *testing.T) {
	var r Redactor
	r.Add("")
	if got := r.String("plain text"); got != "plain text" {
		t.Errorf("String() = %q", got)
	}
}

// TestWriterMasksLogPackage covers the standard log package, which net/http
// and its HTTP/2 transport write their debug output to.
func TestWriterMasksLogPackage(t *testing.T) {
	t.Run("T-ADJ-35/log-format-arguments", func(t *testing.T) {
		var r Redactor
		r.Add("123456:SECRET-TOKEN")
		var buf bytes.Buffer
		logger := log.New(r.Writer(&buf), "", 0)
		logger.Printf("http2: Transport encoding header %q = %q", ":path", "/bot123456:SECRET-TOKEN/sendMessage")
		if got := buf.String(); strings.Contains(got, "SECRET") || !strings.Contains(got, "/bot***/sendMessage") {
			t.Errorf("log output = %q", got)
		}
	})
}

// TestHandlerMasksRecords covers the message, plain and grouped attributes,
// attributes attached with With, and errors passed as values.
func TestHandlerMasksRecords(t *testing.T) {
	t.Run("T-ADJ-35/slog-attributes", func(t *testing.T) {
		const secret = "123456:SECRET-TOKEN"
		var r Redactor
		var buf bytes.Buffer
		logger := slog.New(r.Handler(slog.NewTextHandler(&buf, nil))).With("url", "https://api.example.org/bot"+secret)
		// Registered after the logger exists: the configuration is loaded after
		// the first logger is created.
		r.Add(secret)
		logger.WithGroup("req").Error("failed "+secret,
			"err", errors.New("dial /bot"+secret+": refused"),
			slog.Group("target", "token", secret),
			"panic", []any{secret},
			"status", 502,
		)
		got := buf.String()
		if strings.Contains(got, "SECRET") {
			t.Errorf("log output contains the secret: %s", got)
		}
		if !strings.Contains(got, "req.status=502") {
			t.Errorf("non-string attribute lost: %s", got)
		}
	})
}
