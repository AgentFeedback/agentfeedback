package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// Detail codes and "?name" pointers of the REST query grammar, so a tool
// argument error reads like the matching query parameter error.
const (
	detailUnknown  = "unknown_field"
	detailEmpty    = "empty"
	detailRepeated = "duplicate_key"
	detailType     = "type_mismatch"
	detailFormat   = "invalid_format"
	detailRange    = "out_of_range"
	detailRequired = "required"
)

// Argument sets per tool, named and spelled as the REST query parameters.
var (
	filterArgs = []string{"kind", "schema_version", "key", "machine", "model", "project", "harness",
		"category", "fix_status", "exclude_kind", "verdict", "processed", "redacted", "content_hash",
		"since", "until", "on", "q"}
	listArgs   = append(slices.Clone(filterArgs), "before_id", "after_id", "limit", "include")
	statsArgs  = append(slices.Clone(filterArgs), "by", "top", "bucket")
	getArgs    = []string{"id"}
	schemaArgs = []string{"kind", "version"}
)

// without returns names minus drop.
func without(names []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == drop })
}

func invalid(code, pointer, message string) *core.Problem {
	return &core.Problem{Status: 400, Code: core.CodeValidation, Message: message,
		Details: []core.Detail{{Code: code, Pointer: pointer, Message: message}}}
}

func short(v string) string {
	const limit = 64
	if len(v) > limit {
		cut, _ := schema.TruncateBytes(v, limit)
		v = cut + "…"
	}
	return strconv.Quote(v)
}

// member is one top-level member of an arguments object, its value as the
// exact source bytes.
type member struct {
	name string
	raw  json.RawMessage
}

var errNotObject = &core.Problem{Status: 400, Code: core.CodeBadRequest, Message: "arguments must be a JSON object"}

// isAbsent reports arguments that were not sent or sent as null.
func isAbsent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// scan reads raw as one JSON object and returns its top-level members in
// source order. Numbers keep their spelling.
func scan(raw json.RawMessage) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var members []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		members = append(members, member{name, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errNotObject
	}
	return members, nil
}

// args are a tool's decoded arguments, checked against its argument set.
type args map[string]json.RawMessage

// readArgs decodes raw strictly: absent or null is no argument; anything
// but one object, a repeated or an unknown argument is an error. Names are
// checked in sorted order so the reported error is deterministic.
func readArgs(raw json.RawMessage, allowed []string) (args, error) {
	a := args{}
	if isAbsent(raw) {
		return a, nil
	}
	members, err := scan(raw)
	if err != nil {
		if p, ok := err.(*core.Problem); ok {
			return nil, p
		}
		return nil, &core.Problem{Status: 400, Code: core.CodeBadRequest, Message: "arguments are not valid JSON: " + err.Error()}
	}
	for _, m := range members {
		if _, dup := a[m.name]; dup {
			return nil, invalid(detailRepeated, "?"+m.name, fmt.Sprintf("argument %s appears more than once; send it once", short(m.name)))
		}
		a[m.name] = m.raw
	}
	names := make([]string, 0, len(a))
	for name := range a {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(allowed, name) {
			accepted := "none"
			if len(allowed) > 0 {
				accepted = strings.Join(allowed, ", ")
			}
			return nil, invalid(detailUnknown, "?"+name, fmt.Sprintf("argument %s is not accepted by this tool; accepted: %s", short(name), accepted))
		}
	}
	return a, nil
}

// jsonKind names the JSON type of a raw value.
func jsonKind(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "nothing"
	}
	switch t[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

func typeErr(name, want string, raw json.RawMessage) error {
	return invalid(detailType, "?"+name, fmt.Sprintf("%s must be %s, got %s", name, want, jsonKind(raw)))
}

func (a args) str(name string) (string, error) {
	raw, ok := a[name]
	if !ok {
		return "", nil
	}
	return stringValue(name, "?"+name, raw)
}

func stringValue(name, pointer string, raw json.RawMessage) (string, error) {
	var s string
	if jsonKind(raw) != "a string" || json.Unmarshal(raw, &s) != nil {
		return "", invalid(detailType, pointer, fmt.Sprintf("%s must be a string, got %s", name, jsonKind(raw)))
	}
	if s == "" {
		return "", invalid(detailEmpty, pointer, fmt.Sprintf("argument %s is an empty string; omit it or give a value", name))
	}
	return s, nil
}

var jsonInteger = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func (a args) int64(name string) (*int64, error) {
	raw, ok := a[name]
	if !ok {
		return nil, nil
	}
	t := string(bytes.TrimSpace(raw))
	if jsonKind(raw) != "a number" || !jsonInteger.MatchString(t) {
		return nil, invalid(detailType, "?"+name, fmt.Sprintf("%s must be a JSON integer (no fraction or exponent), got %s", name, short(t)))
	}
	if strings.HasPrefix(t, "-") {
		return nil, invalid(detailRange, "?"+name, fmt.Sprintf("%s must not be negative, got %s", name, t))
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return nil, invalid(detailRange, "?"+name, fmt.Sprintf("%s must be at most %d, got %s", name, int64(math.MaxInt64), short(t)))
	}
	return &n, nil
}

func (a args) int(name string) (*int, error) {
	n, err := a.int64(name)
	if n == nil || err != nil {
		return nil, err
	}
	if *n > math.MaxInt {
		return nil, invalid(detailRange, "?"+name, fmt.Sprintf("%s must be at most %d, got %d", name, math.MaxInt, *n))
	}
	v := int(*n)
	return &v, nil
}

func (a args) bool(name string) (*bool, error) {
	raw, ok := a[name]
	if !ok {
		return nil, nil
	}
	var b bool
	if jsonKind(raw) != "a boolean" || json.Unmarshal(raw, &b) != nil {
		return nil, typeErr(name, "true or false", raw)
	}
	return &b, nil
}

func (a args) time(name string) (*time.Time, error) {
	s, err := a.str(name)
	if s == "" || err != nil {
		return nil, err
	}
	t, ok := schema.ParseDateTime(s)
	if !ok {
		return nil, invalid(detailFormat, "?"+name, fmt.Sprintf("%s must be an RFC 3339 timestamp such as 2026-09-27T10:00:00Z, got %s", name, short(s)))
	}
	return &t, nil
}

func (a args) strings(name string) ([]string, error) {
	raw, ok := a[name]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if jsonKind(raw) != "an array" || json.Unmarshal(raw, &items) != nil {
		return nil, typeErr(name, "an array of strings", raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, err := stringValue(name, "?"+name, item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// filter reads the filter set list_submissions and stats share; preset,
// when set, is the project filter.
func (a args) filter(preset string) (core.Filter, error) {
	var f core.Filter
	var err error
	for _, s := range []struct {
		name string
		dst  *string
	}{{"kind", &f.Kind}, {"key", &f.Key}, {"machine", &f.Machine}, {"model", &f.Model},
		{"project", &f.Project}, {"harness", &f.Harness}, {"category", &f.Category},
		{"fix_status", &f.FixStatus}, {"verdict", &f.Verdict}, {"content_hash", &f.ContentHash},
		{"on", &f.On}, {"q", &f.Q}} {
		if *s.dst, err = a.str(s.name); err != nil {
			return f, err
		}
	}
	if preset != "" {
		f.Project = preset
	}
	if f.ExcludeKind, err = a.strings("exclude_kind"); err != nil {
		return f, err
	}
	if f.SchemaVersion, err = a.int64("schema_version"); err != nil {
		return f, err
	}
	if f.Processed, err = a.bool("processed"); err != nil {
		return f, err
	}
	if f.Redacted, err = a.bool("redacted"); err != nil {
		return f, err
	}
	if f.Since, err = a.time("since"); err != nil {
		return f, err
	}
	if f.Until, err = a.time("until"); err != nil {
		return f, err
	}
	return f, nil
}

func (a args) listParams(preset string) (core.ListParams, error) {
	var p core.ListParams
	var err error
	if p.Filter, err = a.filter(preset); err != nil {
		return p, err
	}
	if p.BeforeID, err = a.int64("before_id"); err != nil {
		return p, err
	}
	if p.AfterID, err = a.int64("after_id"); err != nil {
		return p, err
	}
	if p.Limit, err = a.int("limit"); err != nil {
		return p, err
	}
	inc, err := a.str("include")
	if err != nil {
		return p, err
	}
	switch inc {
	case "":
	case "payload":
		p.IncludePayload = true
	default:
		return p, invalid(detailRange, "?include", fmt.Sprintf("include must be payload, got %s", short(inc)))
	}
	return p, nil
}

func (a args) statsParams(preset string) (core.StatsParams, error) {
	var p core.StatsParams
	var err error
	if p.Filter, err = a.filter(preset); err != nil {
		return p, err
	}
	by, err := a.str("by")
	if err != nil {
		return p, err
	}
	if by != "" {
		p.By = strings.Split(by, ",")
	}
	if p.Top, err = a.int("top"); err != nil {
		return p, err
	}
	if p.Bucket, err = a.str("bucket"); err != nil {
		return p, err
	}
	return p, nil
}

// withPreset returns the create body with "project": preset inserted as its
// first member, the rest of the bytes untouched. A body that already has a
// top-level project is refused; one that is not an object passes unchanged
// for core to reject.
func withPreset(raw json.RawMessage, preset string) (json.RawMessage, error) {
	members, err := scan(raw)
	if err != nil {
		return raw, nil
	}
	for _, m := range members {
		if m.name == "project" {
			return nil, invalid(detailUnknown, "/project", "project is preset by this connection's path /mcp/{project}; omit it")
		}
	}
	q, err := json.Marshal(preset)
	if err != nil {
		return nil, err
	}
	i := bytes.IndexByte(raw, '{')
	out := make([]byte, 0, len(raw)+len(q)+12)
	out = append(out, raw[:i+1]...)
	out = append(out, `"project":`...)
	out = append(out, q...)
	if len(members) > 0 {
		out = append(out, ',')
	}
	return append(out, raw[i+1:]...), nil
}
