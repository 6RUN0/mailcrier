package config

import (
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// keySeparator joins key parts in keyIndex; TOML keys may contain dots when
// quoted, but never this control character.
const keySeparator = "\x1f"

// keyIndex maps every key path present in the document to the position of
// its first appearance. Validation needs it twice: to tell a key that is
// set from one left at its zero value, and to report where a bad key is.
// The path of a key in an element of an array of tables carries the index
// of the element after the name of the array (see elementPath), so that
// each element has its own keys.
type keyIndex map[string]unstable.Position

// indexKeys walks the document with the go-toml parser. The document has
// already been decoded, so a parse error here is unexpected.
func indexKeys(doc []byte) (keyIndex, error) {
	keys := keyIndex{}
	// elements counts the elements of each array of tables, by the path
	// of the array.
	elements := map[string]int{}
	var parser unstable.Parser
	parser.Reset(doc)
	var table []string
	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = keys.recordTable(&parser, expr, elements)
		case unstable.KeyValue:
			keys.recordKeyValue(&parser, table, expr)
		}
	}
	return keys, parser.Error()
}

// recordTable records the parts of a table header and returns the path of
// the table. A header of an array of tables adds an element to the array
// and the index of the element to the path; any other header that runs
// through an array, such as [[a.b]] or [a.c] after [[a]], continues its
// last element, as TOML defines.
func (k keyIndex) recordTable(parser *unstable.Parser, expr *unstable.Node, elements map[string]int) []string {
	var path []string
	key := expr.Key()
	for key.Next() {
		pos := parser.Shape(key.Node().Raw).Start
		path = append(path, string(key.Node().Data))
		k.recordFirst(path, pos)
		joined := strings.Join(path, keySeparator)
		count, isArray := elements[joined]
		switch {
		case expr.Kind == unstable.ArrayTable && key.IsLast():
			elements[joined] = count + 1
			path = append(path, strconv.Itoa(count))
		case isArray:
			path = append(path, strconv.Itoa(count-1))
		default:
			continue
		}
		k.recordFirst(path, pos)
	}
	return path
}

// recordFirst stores pos for path unless the path is known.
func (k keyIndex) recordFirst(path []string, pos unstable.Position) {
	joined := strings.Join(path, keySeparator)
	if _, seen := k[joined]; !seen {
		k[joined] = pos
	}
}

// elementPath returns the path of key in the index-th element, from 0, of
// the array of tables named array.
func elementPath(array string, index int, key ...string) []string {
	return append([]string{array, strconv.Itoa(index)}, key...)
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
		k.recordFirst(path[:head+i+1], pos)
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

// keyPosition is position for a key whose last part may be the empty
// string, a valid TOML key such as "" = 1, which position takes for "the
// table itself".
func (k keyIndex) keyPosition(path ...string) unstable.Position {
	if pos, ok := k[strings.Join(path, keySeparator)]; ok {
		return pos
	}
	return k.position(path...)
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
