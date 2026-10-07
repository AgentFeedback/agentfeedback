// Package scrub replaces known secret formats in text with a marker,
// [REDACTED:<class>], and counts the replacements per class.
//
// It is defence in depth, not a secret detector: it finds the formats its
// rules name (private key blocks, JWTs, cloud and forge tokens, bearer
// tokens, URL credentials, secret-named environment lines and assignments)
// and nothing else. Rules apply in a fixed order (Classes). A value that is
// exactly a marker or exactly an upper-case variable reference ($NAME,
// ${NAME}) is left alone, so scrubbing scrubbed text changes nothing.
//
// String scrubs one text, Tree the string values of a decoded JSON tree in
// place, and JSON the string values of an encoded JSON value at the token
// level. In Tree and JSON a non-empty string under a secret-named member
// (password, token, api_key, ...) is replaced whole as an assignment.
// Object keys are never scrubbed.
package scrub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// markerPrefix starts every marker. Markers contain '[' and ':', which no
// token rule matches, so a marker is never matched again.
const markerPrefix = "[REDACTED:"

// Marker returns the marker for class.
func Marker(class string) string { return markerPrefix + class + "]" }

// Counts is the number of replacements per class.
type Counts map[string]int

// Add adds o's counts to c.
func (c Counts) Add(o Counts) {
	for k, v := range o {
		c[k] += v
	}
}

// Total is the sum of all counts.
func (c Counts) Total() int {
	n := 0
	for _, v := range c {
		n += v
	}

	return n
}

// String lists the non-zero counts as "class n", sorted by class name and
// joined by ", ": "bearer_token 1, jwt 2".
func (c Counts) String() string {
	names := make([]string, 0, len(c))
	for k, v := range c {
		if v > 0 {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s %d", k, c[k])
	}

	return strings.Join(parts, ", ")
}

// rule is one secret format: its patterns and the submatch groups that
// hold the value; the first group that took part in a match is replaced by
// the marker (0 for the whole match). nameGroup, when non-zero, is a
// submatch that must also match envName. A value that is exactly a marker
// or a variable reference is kept.
type rule struct {
	class     string
	res       []*regexp.Regexp
	groups    []int
	nameGroup int
}

var (
	// envName matches an upper-case name with an underscore segment that
	// equals or ends with a secret word: MY_API_KEY, APIKEY, DB_PASSWORD,
	// but not AUTHOR, KEYSTONE_PATH or PWD.
	envName = regexp.MustCompile(`(?:^|_)[A-Z0-9]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?|AUTH|PRIVATE)(?:_|$)`)
	// secretName matches a whole member name that names a secret.
	secretName = regexp.MustCompile(`(?i)^[A-Za-z0-9_.-]*(?:password|passwd|client_secret|secret|auth_token|token|api[_-]?key|access_key|private_key)$`)
	markerRe   = regexp.MustCompile(`^\[REDACTED:[a-z_]+\]$`)
	varRefRe   = regexp.MustCompile(`^\$\{?[A-Z_][A-Z0-9_]*\}?$`)
)

var rules = []rule{
	{class: "private_key", res: []*regexp.Regexp{
		regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)}},
	{class: "jwt", res: []*regexp.Regexp{
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)}},
	{class: "aws_access_key", res: []*regexp.Regexp{
		regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)}},
	{class: "gcp_api_key", res: []*regexp.Regexp{
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)}},
	{class: "github_token", res: []*regexp.Regexp{
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`)}},
	{class: "gitlab_token", res: []*regexp.Regexp{
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)}},
	{class: "slack_token", res: []*regexp.Regexp{
		regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)}},
	{class: "stripe_key", res: []*regexp.Regexp{
		regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`)}},
	{class: "anthropic_key", res: []*regexp.Regexp{
		regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)}},
	{class: "openai_key", res: []*regexp.Regexp{
		regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}`)}},
	{class: "bearer_token", groups: []int{2}, res: []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`)}},
	// The password runs to the last @ before the first / of the authority.
	{class: "url_credentials", groups: []int{2}, res: []*regexp.Regexp{
		regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)([^\s/@:]+:[^\s/]+)@`)}},
	// A quoted value runs to its closing quote on the same line; the
	// quotes are kept.
	{class: "env_line", groups: []int{3, 4, 5}, nameGroup: 2, res: []*regexp.Regexp{
		regexp.MustCompile(`(?m)^((?:export )?([A-Z][A-Z0-9_]*)=)(?:"([^"\r\n]*)"|'([^'\r\n]*)'|([^\r\n]+))`)}},
	{class: "assignment", groups: []int{5, 6, 7}, res: []*regexp.Regexp{
		regexp.MustCompile(`(?i)(["']?)([A-Za-z0-9_.-]*(?:password|passwd|client_secret|secret|auth_token|token|api[_-]?key|access_key|private_key))(["']?)(\s*[:=]\s*)(?:"([^"\n]*)"|'([^'\n]*)'|([^\s"'&,;]+))`)}},
}

// Classes returns the class names in the order the rules apply.
func Classes() []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.class
	}

	return out
}

// kept reports whether v is left alone: exactly one marker, or exactly an
// upper-case variable reference ($NAME, ${NAME}).
func kept(v string) bool {
	return markerRe.MatchString(v) || varRefRe.MatchString(v)
}

// replace replaces the value group of every match of re in s with the
// class marker, skipping empty and kept values and names envName refuses.
func (r rule) replace(re *regexp.Regexp, s string) (string, int) {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s, 0
	}
	groups := r.groups
	if groups == nil {
		groups = []int{0}
	}
	var b strings.Builder
	last, n := 0, 0
	for _, m := range matches {
		if r.nameGroup > 0 && !envName.MatchString(s[m[2*r.nameGroup]:m[2*r.nameGroup+1]]) {
			continue
		}
		start, end := -1, -1
		for _, g := range groups {
			if m[2*g] >= 0 {
				start, end = m[2*g], m[2*g+1]

				break
			}
		}
		if start < 0 || start >= end || kept(s[start:end]) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(Marker(r.class))
		last = end
		n++
	}
	if n == 0 {
		return s, 0
	}
	b.WriteString(s[last:])

	return b.String(), n
}

// member scrubs the string value of an object member called name: a
// non-empty value under a secret-named member is replaced whole, anything
// else goes through String.
func member(name, v string) (string, Counts) {
	if v != "" && secretName.MatchString(name) && !kept(v) {
		return Marker("assignment"), Counts{"assignment": 1}
	}

	return String(v)
}

// String applies every rule to s in order and returns the result and the
// counts. Counts is never nil.
func String(s string) (string, Counts) {
	counts := Counts{}
	for _, r := range rules {
		n := 0
		for _, re := range r.res {
			var k int
			s, k = r.replace(re, s)
			n += k
		}
		if n > 0 {
			counts[r.class] += n
		}
	}

	return s, counts
}

// Tree scrubs the string values of a decoded JSON tree (map[string]any,
// []any, string, json.Number, bool, nil) in place: maps and slices are
// mutated, object keys are never scrubbed. A bare string cannot be changed
// in place, so v must be a map or a slice for anything to change; Tree on a
// bare string returns zero counts.
func Tree(v any) Counts {
	counts := Counts{}
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok {
				out, c := member(k, s)
				t[k] = out
				counts.Add(c)
			} else {
				counts.Add(Tree(e))
			}
		}
	case []any:
		for i, e := range t {
			if s, ok := e.(string); ok {
				out, c := String(s)
				t[i] = out
				counts.Add(c)
			} else {
				counts.Add(Tree(e))
			}
		}
	}

	return counts
}

// frame is one open container while JSON rewrites a value.
type frame struct {
	object bool
	key    bool   // object: the next token is a member name
	name   string // object: the name of the member being read
	n      int    // members or items written
}

// JSON scrubs the string values of one JSON value at the token level and
// returns the rewritten value compactly. Member order, duplicate members and
// number spellings are kept; object keys are never scrubbed; rewritten
// strings are not HTML-escaped. When nothing matched it returns raw itself.
func JSON(raw []byte) ([]byte, Counts, error) {
	counts := Counts{}
	if !json.Valid(raw) {
		return nil, counts, errors.New("scrub: not one valid JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	var stack []frame
	// sep writes what goes before the next key or value.
	sep := func() {
		if len(stack) == 0 {
			return
		}
		f := &stack[len(stack)-1]
		switch {
		case f.object && !f.key:
			buf.WriteByte(':')
		case f.n > 0:
			buf.WriteByte(',')
		}
	}
	// done records a written key or value in the open container.
	done := func() {
		if len(stack) == 0 {
			return
		}
		f := &stack[len(stack)-1]
		if f.object && f.key {
			f.key = false

			return
		}
		f.key = f.object
		f.n++
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, counts, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				sep()
				buf.WriteByte(byte(t))
				stack = append(stack, frame{object: t == '{', key: t == '{'})
			default:
				stack = stack[:len(stack)-1]
				buf.WriteByte(byte(t))
				done()
			}
		case string:
			sep()
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].key {
				stack[len(stack)-1].name = t
				buf.Write(encodeString(t))
			} else {
				var out string
				var c Counts
				if len(stack) > 0 && stack[len(stack)-1].object {
					out, c = member(stack[len(stack)-1].name, t)
				} else {
					out, c = String(t)
				}
				counts.Add(c)
				buf.Write(encodeString(out))
			}
			done()
		case json.Number:
			sep()
			buf.WriteString(t.String())
			done()
		case bool:
			sep()
			if t {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}
			done()
		case nil:
			sep()
			buf.WriteString("null")
			done()
		}
	}
	if counts.Total() == 0 {
		return raw, counts, nil
	}

	return buf.Bytes(), counts, nil
}

// encodeString encodes s as a JSON string without HTML escaping.
func encodeString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return bytes.TrimRight(buf.Bytes(), "\n")
}
