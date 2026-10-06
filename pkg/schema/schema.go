package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/schemas"
)

// Detail is one warning or error detail item: the code from the closed list
// in conformance/warnings.json, an RFC 6901 pointer into the stored record,
// and an advisory message.
type Detail struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// Schema is one compiled schema node. It is immutable after init: every
// accessor returns a copy or a pointer to another immutable node.
type Schema struct {
	types         []string
	enum          []any
	minimum       *bound
	maximum       *bound
	minLength     *int
	minItems      *int
	maxItems      *int
	maxProperties *int
	format        string
	maxBytes      int
	trim          bool
	normalize     string
	onViolation   string
	recommended   []string
	uniqueBy      string
	properties    map[string]*Schema
	additional    *Schema
	items         *Schema
	empty         bool // the node had no keywords at all
}

type bound struct {
	spelling string
	dec      decimal
}

// Types returns the JSON types the node allows; nil means any.
func (s *Schema) Types() []string { return slices.Clone(s.types) }

// Property returns the subschema of a listed member.
func (s *Schema) Property(name string) (*Schema, bool) {
	p, ok := s.properties[name]
	return p, ok
}

// PropertyNames returns the listed member names, sorted.
func (s *Schema) PropertyNames() []string {
	return slices.Sorted(maps.Keys(s.properties))
}

// AdditionalProperties returns the subschema for unlisted members, or nil
// when the keyword is absent or a boolean.
func (s *Schema) AdditionalProperties() *Schema { return s.additional }

// Items returns the subschema for array items, or nil.
func (s *Schema) Items() *Schema { return s.items }

// Format returns the format keyword ("date-time") or "".
func (s *Schema) Format() string { return s.format }

// MaxBytes returns x-max-bytes, or 0 when unset.
func (s *Schema) MaxBytes() int { return s.maxBytes }

// Trim reports x-trim. Normalize "token" implies trimming without it.
func (s *Schema) Trim() bool { return s.trim }

// Normalize returns x-normalize ("token") or "".
func (s *Schema) Normalize() string { return s.normalize }

// OnViolation returns x-on-violation: "warn" (the default), "truncate" or
// "move".
func (s *Schema) OnViolation() string {
	if s.onViolation == "" {
		return "warn"
	}
	return s.onViolation
}

// Recommended returns x-recommended, a copy.
func (s *Schema) Recommended() []string { return slices.Clone(s.recommended) }

// UniqueBy returns x-unique-by or "".
func (s *Schema) UniqueBy() string { return s.uniqueBy }

// MinLength returns minLength when set.
func (s *Schema) MinLength() (int, bool) { return optInt(s.minLength) }

// MinItems returns minItems when set.
func (s *Schema) MinItems() (int, bool) { return optInt(s.minItems) }

// MaxItems returns maxItems when set.
func (s *Schema) MaxItems() (int, bool) { return optInt(s.maxItems) }

// MaxProperties returns maxProperties when set.
func (s *Schema) MaxProperties() (int, bool) { return optInt(s.maxProperties) }

func optInt(p *int) (int, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

// Entry is one line of List: a kind and the versions the server ships.
type Entry struct {
	Kind     string   `json:"kind"`
	Versions []uint64 `json:"versions"`
}

// EnvelopeKind is the kind under which List and Document expose the
// envelope schema.
const EnvelopeKind = "envelope"

type compiled struct {
	schema *Schema
	doc    []byte
}

type registry struct {
	envelope compiled
	kinds    map[string]map[uint64]compiled
}

var reg = mustLoad()

func mustLoad() *registry {
	r, err := load(schemas.FS)
	if err != nil {
		panic("schema: " + err.Error())
	}
	return r
}

// Envelope returns the compiled envelope schema.
func Envelope() *Schema { return reg.envelope.schema }

// Kind returns the compiled schema of (kind, version) when the server ships it.
func Kind(kind string, version uint64) (*Schema, bool) {
	c, ok := reg.kinds[kind][version]
	return c.schema, ok
}

// List returns every schema the server ships, the envelope included, sorted
// by kind with versions ascending.
func List() []Entry {
	out := []Entry{{Kind: EnvelopeKind, Versions: []uint64{1}}}
	for kind, versions := range reg.kinds {
		out = append(out, Entry{Kind: kind, Versions: slices.Sorted(maps.Keys(versions))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Document returns a copy of the schema file for (kind, version), the
// envelope under EnvelopeKind version 1, to be served verbatim.
func Document(kind string, version uint64) ([]byte, bool) {
	if kind == EnvelopeKind {
		if version != 1 {
			return nil, false
		}
		return bytes.Clone(reg.envelope.doc), true
	}
	c, ok := reg.kinds[kind][version]
	if !ok {
		return nil, false
	}
	return bytes.Clone(c.doc), true
}

func load(fsys fs.FS) (*registry, error) {
	r := &registry{kinds: map[string]map[uint64]compiled{}}
	raw, err := fs.ReadFile(fsys, "envelope.v1.json")
	if err != nil {
		return nil, err
	}
	s, err := compile(raw, modeEnvelope)
	if err != nil {
		return nil, fmt.Errorf("envelope.v1.json: %w", err)
	}
	r.envelope = compiled{schema: s, doc: raw}
	entries, err := fs.ReadDir(fsys, "kinds")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		kind, version, ok := parseKindFile(name)
		if !ok {
			return nil, fmt.Errorf("kinds/%s: not a <kind>.v<version>.json file", name)
		}
		raw, err := fs.ReadFile(fsys, "kinds/"+name)
		if err != nil {
			return nil, err
		}
		s, err := compile(raw, modeKind)
		if err != nil {
			return nil, fmt.Errorf("kinds/%s: %w", name, err)
		}
		if r.kinds[kind] == nil {
			r.kinds[kind] = map[uint64]compiled{}
		}
		r.kinds[kind][version] = compiled{schema: s, doc: raw}
	}
	return r, nil
}

var versionSpelling = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseKindFile reads <kind>.v<version>.json. The kind must already be in
// token form and must not be the envelope's name.
func parseKindFile(name string) (string, uint64, bool) {
	rest, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return "", 0, false
	}
	i := strings.LastIndex(rest, ".v")
	if i <= 0 {
		return "", 0, false
	}
	kind, v := rest[:i], rest[i+2:]
	if kind == EnvelopeKind || Token(kind) != kind || !versionSpelling.MatchString(v) {
		return "", 0, false
	}
	version, err := strconv.ParseUint(v, 10, 64)
	if err != nil || version > 1<<53-1 {
		return "", 0, false
	}
	return kind, version, true
}

type mode int

const (
	modeKind mode = iota
	modeEnvelope
)

// keywords is the allow-list: every keyword the shipped schemas may use.
// Anything else is a compile error, so nothing is silently ignored.
var keywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "examples": true, "default": true,
	"type": true, "enum": true, "minimum": true, "maximum": true,
	"minLength": true, "minItems": true, "maxItems": true, "maxProperties": true,
	"properties": true, "additionalProperties": true, "items": true, "format": true,
	"x-max-bytes": true, "x-trim": true, "x-normalize": true,
	"x-recommended": true, "x-unique-by": true, "x-on-violation": true,
}

var typeNames = map[string]bool{
	"null": true, "boolean": true, "object": true, "array": true, "number": true, "integer": true, "string": true,
}

// compile parses one schema file and checks every node against the
// allow-list and the value shapes the contract gives the x- keywords.
func compile(raw []byte, m mode) (*Schema, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the schema")
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("schema is not an object")
	}
	if name, dup := firstDuplicateKey(raw); dup {
		return nil, fmt.Errorf("member %q appears more than once", name)
	}
	return (&compiler{mode: m}).node(obj, "")
}

// firstDuplicateKey walks the token stream of a JSON document and returns
// the first member name that repeats within one object. encoding/json keeps
// the last value silently; a schema file must not rely on that.
func firstDuplicateKey(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		seen      map[string]bool // nil for an array
		expectKey bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // io.EOF, or a syntax error that Decode reports
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				stack = append(stack, &frame{seen: map[string]bool{}, expectKey: true})
				continue
			case '[':
				stack = append(stack, &frame{})
				continue
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return "", false
			}
			top = stack[len(stack)-1]
		} else if top != nil && top.seen != nil && top.expectKey {
			name, _ := tok.(string) // the decoder guarantees a string here
			if top.seen[name] {
				return name, true
			}
			top.seen[name] = true
			top.expectKey = false
			continue
		}
		// A value just ended; an object parent expects a member name next.
		if top != nil && top.seen != nil {
			top.expectKey = true
		}
	}
}

type compiler struct{ mode mode }

func (c *compiler) node(obj map[string]any, where string) (*Schema, error) {
	at := where
	if at == "" {
		at = "/"
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", at, fmt.Sprintf(format, args...))
	}
	s := &Schema{empty: len(obj) == 0}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !keywords[k] {
			return nil, fail("unsupported keyword %q", k)
		}
	}
	var err error
	if v, ok := obj["type"]; ok {
		if s.types, err = typeList(v); err != nil {
			return nil, fail("type: %v", err)
		}
	}
	if v, ok := obj["enum"]; ok {
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return nil, fail("enum must be a non-empty array")
		}
		s.enum = list
	}
	for _, kw := range []string{"minimum", "maximum"} {
		v, ok := obj[kw]
		if !ok {
			continue
		}
		n, isNum := v.(json.Number)
		d, parsed := parseDecimal(string(n))
		if !isNum || !parsed {
			return nil, fail("%s must be a number", kw)
		}
		b := &bound{spelling: string(n), dec: d}
		if kw == "minimum" {
			s.minimum = b
		} else {
			s.maximum = b
		}
	}
	for kw, dst := range map[string]**int{
		"minLength": &s.minLength, "minItems": &s.minItems, "maxItems": &s.maxItems, "maxProperties": &s.maxProperties,
	} {
		v, ok := obj[kw]
		if !ok {
			continue
		}
		n, ok := nonNegativeInt(v)
		if !ok {
			return nil, fail("%s must be a non-negative integer", kw)
		}
		*dst = &n
	}
	if v, ok := obj["format"]; ok {
		if v != "date-time" {
			return nil, fail("unsupported format %v", v)
		}
		s.format = "date-time"
	}
	if v, ok := obj["properties"]; ok {
		props, ok := v.(map[string]any)
		if !ok {
			return nil, fail("properties must be an object")
		}
		s.properties = make(map[string]*Schema, len(props))
		for _, name := range slices.Sorted(maps.Keys(props)) {
			sub, ok := props[name].(map[string]any)
			if !ok {
				return nil, fail("properties/%s must be a schema object", name)
			}
			if s.properties[name], err = c.node(sub, where+"/properties/"+EscapeToken(name)); err != nil {
				return nil, err
			}
		}
	}
	if v, ok := obj["additionalProperties"]; ok {
		switch sub := v.(type) {
		case bool:
		case map[string]any:
			if s.additional, err = c.node(sub, where+"/additionalProperties"); err != nil {
				return nil, err
			}
		default:
			return nil, fail("additionalProperties must be a boolean or a schema object")
		}
	}
	if v, ok := obj["items"]; ok {
		sub, ok := v.(map[string]any)
		if !ok {
			return nil, fail("items must be a schema object")
		}
		if s.items, err = c.node(sub, where+"/items"); err != nil {
			return nil, err
		}
	}
	if v, ok := obj["x-max-bytes"]; ok {
		n, ok := nonNegativeInt(v)
		if !ok || n == 0 {
			return nil, fail("x-max-bytes must be a positive integer")
		}
		s.maxBytes = n
	}
	if v, ok := obj["x-trim"]; ok {
		if v != true {
			return nil, fail("x-trim must be true")
		}
		s.trim = true
	}
	if v, ok := obj["x-normalize"]; ok {
		if v != "token" {
			return nil, fail("x-normalize must be \"token\"")
		}
		if s.trim {
			return nil, fail("x-normalize implies x-trim; declare one")
		}
		s.normalize = "token"
	}
	if v, ok := obj["x-recommended"]; ok {
		list, ok := v.([]any)
		if !ok {
			return nil, fail("x-recommended must be an array of member names")
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, fail("x-recommended must be an array of member names")
			}
			if _, declared := s.properties[name]; !declared {
				return nil, fail("x-recommended names %q, which properties does not declare", name)
			}
			s.recommended = append(s.recommended, name)
		}
	}
	if v, ok := obj["x-unique-by"]; ok {
		member, ok := v.(string)
		if !ok || member == "" || member == "key" {
			return nil, fail("x-unique-by must name a member other than key")
		}
		if !slices.Equal(s.types, []string{"array"}) {
			return nil, fail("x-unique-by on a non-array")
		}
		s.uniqueBy = member
	}
	if v, ok := obj["x-on-violation"]; ok {
		switch v {
		case "warn", "truncate":
		case "move":
			if c.mode != modeEnvelope || where != "/additionalProperties" {
				return nil, fail("x-on-violation move is only for the envelope's additionalProperties")
			}
		default:
			return nil, fail("x-on-violation must be warn, truncate or move")
		}
		s.onViolation = v.(string)
	}
	return s, nil
}

func typeList(v any) ([]string, error) {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, item := range t {
			name, ok := item.(string)
			if !ok {
				return nil, errors.New("must be a type name or an array of type names")
			}
			names = append(names, name)
		}
	default:
		return nil, errors.New("must be a type name or an array of type names")
	}
	if len(names) == 0 {
		return nil, errors.New("must name at least one type")
	}
	for _, n := range names {
		if !typeNames[n] {
			return nil, fmt.Errorf("unknown type %q", n)
		}
	}
	return names, nil
}

var plainInt = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)

func nonNegativeInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || !plainInt.MatchString(string(n)) {
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	return i, err == nil
}
