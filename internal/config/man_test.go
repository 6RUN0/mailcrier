package config

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestManPageListsKeys pins that the manual page names every key of the
// configuration: a top-level table as it is written, any other key as
// .Ic, so that a key added to the schema is documented in the same
// change. The free keys of headers and query are not keys of the schema.
func TestManPageListsKeys(t *testing.T) {
	page, err := os.ReadFile("../../docs/slendmail.8")
	if err != nil {
		t.Fatal(err)
	}
	man := string(page)
	tables := map[string]string{
		"general": `.Li "[general]"`, "spool": `.Li "[spool]"`, "strings": `.Li "[strings]"`,
		"target": `.Li "[target.NAME]"`, "route": `.Li "[[route]]"`, "suppress": `.Li "[[suppress]]"`,
	}
	for _, field := range reflect.VisibleFields(reflect.TypeFor[Config]()) {
		key := field.Tag.Get("toml")
		want, ok := tables[key]
		if !ok {
			t.Errorf("top-level table %q has no expected markup", key)
			continue
		}
		if !strings.Contains(man, want) {
			t.Errorf("manual page lacks %s", want)
		}
	}
	for _, schema := range []reflect.Type{
		reflect.TypeFor[General](), reflect.TypeFor[Spool](), reflect.TypeFor[Strings](),
		reflect.TypeFor[Target](), reflect.TypeFor[Route](), reflect.TypeFor[Suppression](),
	} {
		for _, field := range reflect.VisibleFields(schema) {
			key := field.Tag.Get("toml")
			if key == "" || key == "-" {
				continue
			}
			if !regexp.MustCompile(`(?m)\bIc ` + regexp.QuoteMeta(key) + `( |,|$)`).MatchString(man) {
				t.Errorf("manual page lacks .Ic %s (%s)", key, schema.Name())
			}
		}
	}
}
