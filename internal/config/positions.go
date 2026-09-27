package config

import (
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// keySeparator joins key parts in keyIndex; TOML keys may contain dots when
// quoted, but never this control character.
const keySeparator = "\x1f"

// keyIndex maps every key path present in the document to the position of
// its first appearance. Validation needs it twice: to tell a key that is
// set from one left at its zero value, and to report where a bad key is.
type keyIndex map[string]unstable.Position

// indexKeys walks the document with the go-toml parser. The document has
// already been decoded, so a parse error here is unexpected.
func indexKeys(doc []byte) (keyIndex, error) {
	keys := keyIndex{}
	var parser unstable.Parser
	parser.Reset(doc)
	var table []string
	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = keyParts(expr.Key())
			keys.record(&parser, table, expr.Key())
		case unstable.KeyValue:
			keys.recordKeyValue(&parser, table, expr)
		}
	}
	return keys, parser.Error()
}

func (k keyIndex) recordKeyValue(parser *unstable.Parser, table []string, kv *unstable.Node) {
	path := append(append([]string{}, table...), keyParts(kv.Key())...)
	k.record(parser, path, kv.Key())
	value := kv.Value()
	if value.Kind != unstable.InlineTable {
		return
	}
	children := value.Children()
	for children.Next() {
		k.recordKeyValue(parser, path, children.Node())
	}
}

// record stores, for path and each of its prefixes, the position of the key
// part that ends it, so that "target.mm" is known after
// "target.mm.type = ..." and points at "mm". The key nodes cover the tail
// of path; the head comes from the enclosing table header.
func (k keyIndex) record(parser *unstable.Parser, path []string, key unstable.Iterator) {
	var positions []unstable.Position
	for key.Next() {
		positions = append(positions, parser.Shape(key.Node().Raw).Start)
	}
	head := len(path) - len(positions)
	for i, pos := range positions {
		joined := strings.Join(path[:head+i+1], keySeparator)
		if _, seen := k[joined]; !seen {
			k[joined] = pos
		}
	}
}

func keyParts(key unstable.Iterator) []string {
	var parts []string
	for key.Next() {
		parts = append(parts, string(key.Node().Data))
	}
	return parts
}

// has reports whether the key path is present in the document.
func (k keyIndex) has(path ...string) bool {
	_, ok := k[strings.Join(path, keySeparator)]
	return ok
}

// position returns where the key path appears, falling back to its parent
// when the key itself is absent or empty.
func (k keyIndex) position(path ...string) unstable.Position {
	for len(path) > 0 {
		if path[len(path)-1] != "" {
			if pos, ok := k[strings.Join(path, keySeparator)]; ok {
				return pos
			}
		}
		path = path[:len(path)-1]
	}
	return unstable.Position{}
}

// children returns the direct child keys of the table at path, sorted.
func (k keyIndex) children(path ...string) []string {
	prefix := strings.Join(path, keySeparator) + keySeparator
	var names []string
	for joined := range k {
		rest, ok := strings.CutPrefix(joined, prefix)
		if ok && !strings.Contains(rest, keySeparator) {
			names = append(names, rest)
		}
	}
	sort.Strings(names)
	return names
}
