package message

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRead(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantSubject string
		wantBody    string
	}{
		{"headers-and-body", "Subject: disk full\nTo: root\n\n/dev/sda1 99%\n", "disk full", "/dev/sda1 99%\n"},
		{"no-subject", "To: root\n\nbody\n", "", "body\n"},
		{"not-a-header-block", "just text\nmore text\n", "", "just text\nmore text\n"},
		{"empty-input", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := Read(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if msg.Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", msg.Subject, tc.wantSubject)
			}
			if msg.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", msg.Body, tc.wantBody)
			}
			if string(msg.Raw) != tc.input {
				t.Errorf("Raw = %q, want %q", msg.Raw, tc.input)
			}
		})
	}
}

func TestReadReportsInputError(t *testing.T) {
	cause := errors.New("broken pipe")
	if _, err := Read(iotest.ErrReader(cause)); !errors.Is(err, cause) {
		t.Fatalf("Read() error = %v, want %v", err, cause)
	}
}
