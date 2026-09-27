package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFormatParseRoundTrip(t *testing.T) {
	archive := Archive{
		Comment: "request produced by the preset\n",
		Sections: []Section{
			{Name: "headers", Data: []byte("Content-Type: application/json\n")},
			{Name: "body", Data: []byte(`{"subject":"s"}` + "\n")},
			{Name: "empty", Data: []byte{}},
		},
	}
	got := Parse(Format(archive))
	if !reflect.DeepEqual(got, archive) {
		t.Errorf("Parse(Format(a)) = %#v, want %#v", got, archive)
	}
}

func TestParseKeepsNearSectionLines(t *testing.T) {
	data := "-- body --\n-- not a marker\n--  --\nend\n"
	got := Parse([]byte(data))
	want := Archive{Sections: []Section{{Name: "body", Data: []byte("-- not a marker\n--  --\nend\n")}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() = %#v, want %#v", got, want)
	}
}

func TestFormatAddsTrailingNewline(t *testing.T) {
	got := string(Format(Archive{Sections: []Section{{Name: "body", Data: []byte("x")}}}))
	if want := "-- body --\nx\n"; got != want {
		t.Errorf("Format() = %q, want %q", got, want)
	}
}

// errorRecorder counts failures instead of failing the enclosing test.
type errorRecorder struct {
	testing.TB
	errors []string
}

func (r *errorRecorder) Helper() {}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *errorRecorder) Fatalf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestCheckReportsCommentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.txtar")
	stored := Archive{Comment: "old comment\n", Sections: []Section{{Name: "body", Data: []byte("b\n")}}}
	if err := os.WriteFile(path, Format(stored), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &errorRecorder{TB: t}
	Check(recorder, path, Archive{Comment: "new comment\n", Sections: stored.Sections})
	if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], "comment differs") {
		t.Errorf("Check() errors = %q, want one comment difference", recorder.errors)
	}
	recorder.errors = nil
	Check(recorder, path, stored)
	if len(recorder.errors) != 0 {
		t.Errorf("Check() errors = %q on identical archive, want none", recorder.errors)
	}
}
