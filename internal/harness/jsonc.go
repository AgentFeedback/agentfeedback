package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The JSON editor works on the bytes of a user's file and never
// re-serialises it: every edit splices text into or out of the original, so
// comments, trailing commas, line endings and formatting elsewhere survive.

// ErrExists is returned by InsertMember when the key is already a member.
var ErrExists = errors.New("member already exists")

// DuplicateKeyError is a document with a key twice in one object; the
// editor refuses it, since harnesses disagree on which copy wins.
type DuplicateKeyError struct{ Key string }

func (e *DuplicateKeyError) Error() string {
	return "the key " + e.Key + " appears twice in one object"
}

// jnode is one parsed value with its byte span [start, end).
type jnode struct {
	kind    byte // '{', '[' or 'v' (a scalar)
	start   int
	end     int
	entries []jentry
}

// jentry is one object member or array element. start is the key's first
// byte for a member, the value's for an element; comma is the index of the
// comma that follows it, or -1.
type jentry struct {
	key   string
	start int
	val   *jnode
	comma int
}

type jparser struct {
	b []byte
	i int
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// skip passes whitespace and comments.
func (p *jparser) skip() error {
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case isSpace(c):
			p.i++
		case c == '/' && p.i+1 < len(p.b) && p.b[p.i+1] == '/':
			for p.i < len(p.b) && p.b[p.i] != '\n' {
				p.i++
			}
		case c == '/' && p.i+1 < len(p.b) && p.b[p.i+1] == '*':
			end := bytes.Index(p.b[p.i+2:], []byte("*/"))
			if end < 0 {
				return errors.New("unterminated comment")
			}
			p.i += end + 4
		default:
			return nil
		}
	}

	return nil
}

func (p *jparser) str() (string, error) {
	start := p.i
	p.i++
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '\\':
			p.i += 2

			continue
		case '"':
			p.i++
			var s string
			if err := json.Unmarshal(p.b[start:p.i], &s); err != nil {
				return "", fmt.Errorf("invalid string at byte %d", start)
			}

			return s, nil
		}
		p.i++
	}

	return "", fmt.Errorf("unterminated string at byte %d", start)
}

func (p *jparser) value() (*jnode, error) {
	if err := p.skip(); err != nil {
		return nil, err
	}
	if p.i >= len(p.b) {
		return nil, errors.New("unexpected end of input")
	}
	start := p.i
	switch c := p.b[p.i]; c {
	case '{', '[':
		n := &jnode{kind: c, start: start}
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		p.i++
		for {
			if err := p.skip(); err != nil {
				return nil, err
			}
			if p.i >= len(p.b) {
				return nil, errors.New("unexpected end of input")
			}
			if p.b[p.i] == closer {
				p.i++
				n.end = p.i

				return n, nil
			}
			e := jentry{start: p.i, comma: -1}
			if c == '{' {
				if p.b[p.i] != '"' {
					return nil, fmt.Errorf("expected a member name at byte %d", p.i)
				}
				key, err := p.str()
				if err != nil {
					return nil, err
				}
				for _, prev := range n.entries {
					if prev.key == key {
						return nil, &DuplicateKeyError{Key: key}
					}
				}
				e.key = key
				if err := p.skip(); err != nil {
					return nil, err
				}
				if p.i >= len(p.b) || p.b[p.i] != ':' {
					return nil, fmt.Errorf("expected ':' at byte %d", p.i)
				}
				p.i++
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			e.val = v
			if err := p.skip(); err != nil {
				return nil, err
			}
			if p.i < len(p.b) && p.b[p.i] == ',' {
				e.comma = p.i
				p.i++
			} else if p.i < len(p.b) && p.b[p.i] != closer {
				return nil, fmt.Errorf("expected ',' or '%c' at byte %d", closer, p.i)
			}
			n.entries = append(n.entries, e)
		}
	case '"':
		if _, err := p.str(); err != nil {
			return nil, err
		}

		return &jnode{kind: 'v', start: start, end: p.i}, nil
	default:
		for p.i < len(p.b) && strings.IndexByte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.+-", p.b[p.i]) >= 0 {
			p.i++
		}
		if p.i == start {
			return nil, fmt.Errorf("unexpected %q at byte %d", c, start)
		}

		return &jnode{kind: 'v', start: start, end: p.i}, nil
	}
}

// parse reads a whole JSONC document whose top level is an object, and
// checks it is valid JSON once comments and trailing commas are removed.
func parse(doc []byte) (*jnode, error) {
	if err := validJSONC(doc); err != nil {
		return nil, err
	}
	p := &jparser{b: doc}
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	if err := p.skip(); err != nil {
		return nil, err
	}
	if p.i != len(doc) {
		return nil, fmt.Errorf("unexpected text after the document at byte %d", p.i)
	}
	if n.kind != '{' {
		return nil, errors.New("the top level is not an object")
	}

	return n, nil
}

// Standard returns doc with comments and trailing commas blanked out, which
// is standard JSON when doc is valid JSONC. Line breaks are kept, so byte
// offsets stay the same.
func Standard(doc []byte) []byte {
	out := bytes.Clone(doc)
	blank := func(from, to int) {
		for k := from; k < to; k++ {
			if out[k] != '\n' && out[k] != '\r' {
				out[k] = ' '
			}
		}
	}
	skipString := func(b []byte, i int) int {
		for i++; i < len(b) && b[i] != '"'; i++ {
			if b[i] == '\\' {
				i++
			}
		}

		return i
	}
	for i := 0; i < len(doc); i++ {
		switch c := doc[i]; {
		case c == '"':
			i = skipString(doc, i)
		case c == '/' && i+1 < len(doc) && doc[i+1] == '/':
			j := i
			for j < len(doc) && doc[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j - 1
		case c == '/' && i+1 < len(doc) && doc[i+1] == '*':
			j := len(doc)
			if end := bytes.Index(doc[i+2:], []byte("*/")); end >= 0 {
				j = i + 2 + end + 2
			}
			blank(i, j)
			i = j - 1
		}
	}
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '"':
			i = skipString(out, i)
		case ',':
			j := i + 1
			for j < len(out) && isSpace(out[j]) {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}

	return out
}

func validJSONC(doc []byte) error {
	if !json.Valid(Standard(doc)) {
		return errors.New("not valid JSON or JSONC")
	}

	return nil
}

// style is the formatting an insertion copies from the document.
type style struct {
	unit string
	nl   string
}

func docStyle(doc []byte) style {
	s := style{unit: "  ", nl: "\n"}
	if bytes.Contains(doc, []byte("\r\n")) {
		s.nl = "\r\n"
	}
	for _, line := range bytes.Split(doc, []byte("\n")) {
		k := 0
		for k < len(line) && (line[k] == ' ' || line[k] == '\t') {
			k++
		}
		if k > 0 && k < len(line) && line[k] != '\r' {
			s.unit = string(line[:k])

			break
		}
	}

	return s
}

func lineStart(doc []byte, pos int) int {
	return bytes.LastIndexByte(doc[:pos], '\n') + 1
}

func lineIndent(doc []byte, pos int) string {
	ls := lineStart(doc, pos)
	k := ls
	for k < len(doc) && (doc[k] == ' ' || doc[k] == '\t') {
		k++
	}

	return string(doc[ls:k])
}

// prevEnd is the index just past the last non-blank byte before pos.
func prevEnd(doc []byte, pos int) int {
	for pos > 0 && isSpace(doc[pos-1]) {
		pos--
	}

	return pos
}

func (n *jnode) member(key string) (int, *jnode) {
	if n.kind != '{' {
		return -1, nil
	}
	for i, e := range n.entries {
		if e.key == key {
			return i, e.val
		}
	}

	return -1, nil
}

// walk follows path through objects; it returns the deepest node reached and
// how many segments were found. A segment that names a non-object is an
// error.
func walk(root *jnode, path []string) (*jnode, int, error) {
	cur := root
	for i, seg := range path {
		_, v := cur.member(seg)
		if v == nil {
			return cur, i, nil
		}
		if v.kind != '{' {
			return nil, 0, fmt.Errorf("%s is not an object", strings.Join(path[:i+1], "."))
		}
		cur = v
	}

	return cur, len(path), nil
}

func compact(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, Standard(b)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func keyJSON(k string) string {
	b, _ := json.Marshal(k)

	return string(b)
}

// indentValue formats value at indentation ind with the document's unit.
func indentValue(value []byte, ind string, st style) (string, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, value, ind, st.unit); err != nil {
		return "", err
	}

	return strings.ReplaceAll(buf.String(), "\n", st.nl), nil
}

type splice struct {
	at, to int
	text   string
}

func apply(doc []byte, edits ...splice) []byte {
	// Edits are given in any order and never overlap; apply from the back.
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[j].at > edits[i].at {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	out := bytes.Clone(doc)
	for _, e := range edits {
		out = append(out[:e.at:e.at], append([]byte(e.text), out[e.to:]...)...)
	}

	return out
}

// insertEntry adds text (a member "k": v, or an element, unindented JSON in
// both cases) as the last entry of container n.
func insertEntry(doc []byte, n *jnode, key *string, value []byte) ([]byte, error) {
	st := docStyle(doc)
	open, closeAt := n.start, n.end-1
	braceInd := lineIndent(doc, open)
	ind := braceInd + st.unit
	if len(n.entries) > 0 && lineStart(doc, n.entries[0].start) != lineStart(doc, open) {
		ind = lineIndent(doc, n.entries[0].start)
	}
	v, err := indentValue(value, ind, st)
	if err != nil {
		return nil, err
	}
	text := v
	if key != nil {
		text = keyJSON(*key) + ": " + v
	}
	inner := doc[open+1 : closeAt]
	if len(n.entries) == 0 {
		if len(bytes.TrimSpace(inner)) == 0 {
			return apply(doc, splice{open + 1, closeAt, st.nl + ind + text + st.nl + braceInd}), nil
		}

		return apply(doc, splice{open + 1, open + 1, st.nl + ind + text}), nil
	}
	last := n.entries[len(n.entries)-1]
	q := prevEnd(doc, closeAt)
	tail := ""
	if !bytes.Contains(doc[q:closeAt], []byte("\n")) {
		tail = st.nl + braceInd
	}
	if last.comma >= 0 {
		return apply(doc, splice{q, q, st.nl + ind + text + "," + tail}), nil
	}

	return apply(doc,
		splice{q, q, st.nl + ind + text + tail},
		splice{last.val.end, last.val.end, ","}), nil
}

// removeEntry cuts entry idx of n and the one comma that separated it.
func removeEntry(doc []byte, n *jnode, idx int) []byte {
	e := n.entries[idx]
	from := prevEnd(doc, e.start)
	if idx == 0 {
		from = max(from, n.start+1)
	}
	if e.comma >= 0 {
		return apply(doc, splice{from, e.comma + 1, ""})
	}
	closeAt := n.end - 1
	to := e.val.end
	if idx == 0 {
		// The only entry: an object or array left with blanks only collapses.
		if len(bytes.TrimSpace(doc[n.start+1:from])) == 0 && len(bytes.TrimSpace(doc[to:closeAt])) == 0 {
			return apply(doc, splice{n.start + 1, closeAt, ""})
		}

		return apply(doc, splice{from, to, ""})
	}
	prev := n.entries[idx-1]
	edits := []splice{{prev.comma, prev.comma + 1, ""}}
	// An inline container that an insertion expanded closes inline again.
	if lineStart(doc, prev.val.end) == lineStart(doc, n.start) && len(bytes.TrimSpace(doc[to:closeAt])) == 0 {
		to = closeAt
	}
	edits = append(edits, splice{from, to, ""})

	return apply(doc, edits...)
}

func checkResult(out []byte) ([]byte, error) {
	if _, err := parse(out); err != nil {
		return nil, fmt.Errorf("the edited document would not be valid: %w", err)
	}

	return out, nil
}

// InsertMember adds "key": value as the last member of the object at path,
// creating the objects along path that are missing. createdFrom is the
// number of path segments that already existed. value must be JSON.
func InsertMember(doc []byte, path []string, key string, value []byte) (out []byte, createdFrom int, err error) {
	root, err := parse(doc)
	if err != nil {
		return nil, 0, err
	}
	if !json.Valid(value) {
		return nil, 0, errors.New("the value to insert is not JSON")
	}
	cur, found, err := walk(root, path)
	if err != nil {
		return nil, 0, err
	}
	if found == len(path) {
		if i, _ := cur.member(key); i >= 0 {
			return nil, 0, ErrExists
		}
		out, err = insertEntry(doc, cur, &key, value)
	} else {
		out, err = insertNested(doc, cur, path[found], path[found+1:], key, value)
	}
	if err != nil {
		return nil, 0, err
	}
	out, err = checkResult(out)

	return out, found, err
}

// insertNested inserts seg: {rest...: {key: value}} into cur.
func insertNested(doc []byte, cur *jnode, seg string, rest []string, key string, value []byte) ([]byte, error) {
	inner := "{" + keyJSON(key) + ":" + string(value) + "}"
	for i := len(rest) - 1; i >= 0; i-- {
		inner = "{" + keyJSON(rest[i]) + ":" + inner + "}"
	}

	return insertEntry(doc, cur, &seg, []byte(inner))
}

// AppendElement appends value to the array at path, creating the array and
// the objects along path that are missing. createdFrom is the number of
// path segments (the array's own included) that already existed.
func AppendElement(doc []byte, path []string, value []byte) (out []byte, createdFrom int, err error) {
	if len(path) == 0 {
		return nil, 0, errors.New("no array path")
	}
	root, err := parse(doc)
	if err != nil {
		return nil, 0, err
	}
	if !json.Valid(value) {
		return nil, 0, errors.New("the value to insert is not JSON")
	}
	parent, found, err := walk(root, path[:len(path)-1])
	if err != nil {
		return nil, 0, err
	}
	last := path[len(path)-1]
	if found == len(path)-1 {
		if _, arr := parent.member(last); arr != nil {
			if arr.kind != '[' {
				return nil, 0, fmt.Errorf("%s is not an array", strings.Join(path, "."))
			}
			out, err = insertEntry(doc, arr, nil, value)
			if err != nil {
				return nil, 0, err
			}
			out, err = checkResult(out)

			return out, len(path), err
		}
	}
	arr := "[" + string(value) + "]"
	segs := path[found:]
	inner := arr
	for i := len(segs) - 1; i >= 1; i-- {
		inner = "{" + keyJSON(segs[i]) + ":" + inner + "}"
	}
	out, err = insertEntry(doc, parent, &segs[0], []byte(inner))
	if err != nil {
		return nil, 0, err
	}
	out, err = checkResult(out)

	return out, found, err
}

// prune removes the members along path from index createdFrom on that are
// left empty, deepest first.
func prune(doc []byte, path []string, createdFrom int) ([]byte, error) {
	for i := len(path) - 1; i >= createdFrom && i >= 0; i-- {
		root, err := parse(doc)
		if err != nil {
			return nil, err
		}
		parent, found, err := walk(root, path[:i])
		if err != nil || found != i {
			return doc, err
		}
		idx, child := parent.member(path[i])
		if child == nil || len(child.entries) > 0 {
			return doc, nil
		}
		doc = removeEntry(doc, parent, idx)
	}

	return doc, nil
}

// RemoveMember removes key from the object at path, then the objects along
// path from createdFrom on that are left empty. A missing member is not an
// error: the document comes back unchanged.
func RemoveMember(doc []byte, path []string, key string, createdFrom int) ([]byte, error) {
	root, err := parse(doc)
	if err != nil {
		return nil, err
	}
	cur, found, err := walk(root, path)
	if err != nil {
		return nil, err
	}
	if found != len(path) {
		return doc, nil
	}
	idx, _ := cur.member(key)
	if idx < 0 {
		return doc, nil
	}
	out := removeEntry(doc, cur, idx)
	if out, err = prune(out, path, createdFrom); err != nil {
		return nil, err
	}

	return checkResult(out)
}

// RemoveElement removes the last element of the array at path equal to
// value (compared as compact JSON), then the containers from createdFrom on
// that are left empty.
func RemoveElement(doc []byte, path []string, value []byte, createdFrom int) ([]byte, error) {
	root, err := parse(doc)
	if err != nil {
		return nil, err
	}
	arr, idx, err := findElement(doc, root, path, value)
	if err != nil || arr == nil {
		if err != nil {
			return nil, err
		}

		return doc, nil
	}
	out := removeEntry(doc, arr, idx)
	if out, err = prune(out, path, createdFrom); err != nil {
		return nil, err
	}

	return checkResult(out)
}

func findElement(doc []byte, root *jnode, path []string, value []byte) (*jnode, int, error) {
	if len(path) == 0 {
		return nil, -1, errors.New("no array path")
	}
	want, err := compact(value)
	if err != nil {
		return nil, -1, err
	}
	parent, found, err := walk(root, path[:len(path)-1])
	if err != nil || found != len(path)-1 {
		return nil, -1, err
	}
	_, arr := parent.member(path[len(path)-1])
	if arr == nil || arr.kind != '[' {
		return nil, -1, nil
	}
	for i := len(arr.entries) - 1; i >= 0; i-- {
		e := arr.entries[i]
		got, err := compact(doc[e.val.start:e.val.end])
		if err == nil && bytes.Equal(got, want) {
			return arr, i, nil
		}
	}

	return nil, -1, nil
}

// GetMember returns the raw bytes of key in the object at path, or nil.
func GetMember(doc []byte, path []string, key string) ([]byte, error) {
	root, err := parse(doc)
	if err != nil {
		return nil, err
	}
	cur, found, err := walk(root, path)
	if err != nil || found != len(path) {
		return nil, err
	}
	_, v := cur.member(key)
	if v == nil {
		return nil, nil
	}

	return doc[v.start:v.end], nil
}

// HasElement reports whether the array at path holds an element equal to
// value.
func HasElement(doc []byte, path []string, value []byte) (bool, error) {
	root, err := parse(doc)
	if err != nil {
		return false, err
	}
	arr, _, err := findElement(doc, root, path, value)

	return arr != nil, err
}
