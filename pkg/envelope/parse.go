package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The parser reads RFC 8259 exactly: anything else is not JSON. It produces
// the JSON tree with numbers as json.Number in their source spelling and
// records the parse-time warnings (invalid_utf8, duplicate_key) in the
// details trie at the path of the member they concern.

const replacedMessage = "invalid UTF-8 or a lone surrogate escape replaced by U+FFFD"

type parser struct {
	data  []byte
	pos   int
	depth int
	det   *details
}

// frame is one container level of the parse: the path token under its
// parent and the trie node for that path, created only when a warning below
// it needs it, so a clean body allocates no trie at all.
type frame struct {
	parent *frame
	token  string
	node   *node
}

func (f *frame) get() *node {
	if f.node == nil {
		f.node = f.parent.get().child(f.token)
	}
	return f.node
}

// parse returns the body's value and records warnings into det. The error is
// a syntax error, never a warning.
func parse(data []byte, det *details) (any, error) {
	p := &parser{data: data, det: det}
	root := &frame{node: det.root}
	p.skipWS()
	v, err := p.value(root)
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("trailing bytes at offset %d", p.pos)
	}
	return v, nil
}

func (p *parser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) peek() (byte, error) {
	if p.pos >= len(p.data) {
		return 0, errors.New("unexpected end of body")
	}
	return p.data[p.pos], nil
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > MaxDepth {
		return fmt.Errorf("nested deeper than %d levels", MaxDepth)
	}
	return nil
}

func (p *parser) value(f *frame) (any, error) {
	c, err := p.peek()
	if err != nil {
		return nil, err
	}
	switch {
	case c == '{':
		return p.object(f)
	case c == '[':
		return p.array(f)
	case c == '"':
		s, replaced, err := p.str()
		if err != nil {
			return nil, err
		}
		if replaced {
			f.get().add("invalid_utf8", replacedMessage)
		}
		return s, nil
	case bytes.HasPrefix(p.data[p.pos:], []byte("true")):
		p.pos += 4
		return true, nil
	case bytes.HasPrefix(p.data[p.pos:], []byte("false")):
		p.pos += 5
		return false, nil
	case bytes.HasPrefix(p.data[p.pos:], []byte("null")):
		p.pos += 4
		return nil, nil
	}
	if n, ok := p.number(); ok {
		return n, nil
	}
	return nil, fmt.Errorf("unexpected byte 0x%02x at offset %d", c, p.pos)
}

func (p *parser) object(f *frame) (map[string]any, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	p.pos++
	result := map[string]any{}
	p.skipWS()
	c, err := p.peek()
	if err != nil {
		return nil, err
	}
	if c == '}' {
		p.pos++
		p.depth--
		return result, nil
	}
	for {
		p.skipWS()
		if c, err = p.peek(); err != nil {
			return nil, err
		}
		if c != '"' {
			return nil, fmt.Errorf("expected a member name at offset %d", p.pos)
		}
		name, replaced, err := p.str()
		if err != nil {
			return nil, err
		}
		member := &frame{parent: f, token: name}
		if _, dup := result[name]; dup {
			// Last value wins. Warnings about the discarded value point at
			// content that no longer exists, so they go with it.
			if f.node != nil {
				delete(f.node.children, name)
			}
			member.get().add("duplicate_key", fmt.Sprintf("member %q appears more than once; the last value is kept", name))
		}
		if replaced {
			member.get().add("invalid_utf8", "invalid UTF-8 in a member name replaced by U+FFFD")
		}
		p.skipWS()
		if c, err = p.peek(); err != nil {
			return nil, err
		}
		if c != ':' {
			return nil, fmt.Errorf("expected ':' at offset %d", p.pos)
		}
		p.pos++
		p.skipWS()
		v, err := p.value(member)
		if err != nil {
			return nil, err
		}
		result[name] = v
		p.skipWS()
		if c, err = p.peek(); err != nil {
			return nil, err
		}
		switch c {
		case ',':
			p.pos++
		case '}':
			p.pos++
			p.depth--
			return result, nil
		default:
			return nil, fmt.Errorf("expected ',' or '}' at offset %d", p.pos)
		}
	}
}

func (p *parser) array(f *frame) ([]any, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	p.pos++
	result := []any{}
	p.skipWS()
	c, err := p.peek()
	if err != nil {
		return nil, err
	}
	if c == ']' {
		p.pos++
		p.depth--
		return result, nil
	}
	for {
		p.skipWS()
		item := &frame{parent: f, token: strconv.Itoa(len(result))}
		v, err := p.value(item)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
		p.skipWS()
		if c, err = p.peek(); err != nil {
			return nil, err
		}
		switch c {
		case ',':
			p.pos++
		case ']':
			p.pos++
			p.depth--
			return result, nil
		default:
			return nil, fmt.Errorf("expected ',' or ']' at offset %d", p.pos)
		}
	}
}

// number scans -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? at the current
// position and returns it as a json.Number, or false when no number starts
// here. A fraction or exponent that does not complete is left unread, so
// the next token check reports it.
func (p *parser) number() (json.Number, bool) {
	start, i, n := p.pos, p.pos, len(p.data)
	digits := func() {
		for i < n && p.data[i] >= '0' && p.data[i] <= '9' {
			i++
		}
	}
	if i < n && p.data[i] == '-' {
		i++
	}
	switch {
	case i < n && p.data[i] == '0':
		i++
	case i < n && p.data[i] >= '1' && p.data[i] <= '9':
		digits()
	default:
		return "", false
	}
	if j := i + 1; i < n && p.data[i] == '.' && j < n && p.data[j] >= '0' && p.data[j] <= '9' {
		i = j
		digits()
	}
	if i < n && (p.data[i] == 'e' || p.data[i] == 'E') {
		j := i + 1
		if j < n && (p.data[j] == '+' || p.data[j] == '-') {
			j++
		}
		if j < n && p.data[j] >= '0' && p.data[j] <= '9' {
			i = j
			digits()
		}
	}
	p.pos = i
	return json.Number(p.data[start:i]), true
}

// str reads a string literal at the opening quote and reports whether any
// replacement happened: invalid UTF-8 in the raw bytes or an escaped lone
// surrogate. The raw bytes are repaired first, then the escapes resolved on
// the repaired text, so a backslash before a non-ASCII byte is an invalid
// escape.
func (p *parser) str() (string, bool, error) {
	p.pos++
	start := p.pos
	for {
		if p.pos >= len(p.data) {
			return "", false, errors.New("unterminated string")
		}
		c := p.data[p.pos]
		if c == '"' {
			break
		}
		if c == '\\' {
			p.pos += 2
			continue
		}
		if c < 0x20 {
			return "", false, fmt.Errorf("raw control character in string at offset %d", p.pos)
		}
		p.pos++
	}
	raw := p.data[start:p.pos]
	p.pos++
	text, replaced := repairUTF8(raw)
	text, lone, err := unescape(text)
	if err != nil {
		return "", false, err
	}
	return text, replaced || lone, nil
}

// repairUTF8 decodes b as UTF-8 with one U+FFFD per maximal subpart of an
// ill-formed subsequence (Unicode ch. 3, table 3-8), the WHATWG decoder's
// algorithm, and reports whether anything was replaced. Encoded surrogates,
// overlong forms and code points above U+10FFFF are ill-formed.
func repairUTF8(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return string(b), false
	}
	out := make([]byte, 0, len(b)+3)
	var cp rune
	needed, seen := 0, 0
	lower, upper := byte(0x80), byte(0xBF)
	for i := 0; i < len(b); i++ {
		c := b[i]
		if needed == 0 {
			switch {
			case c <= 0x7F:
				out = append(out, c)
			case c >= 0xC2 && c <= 0xDF:
				needed, cp = 1, rune(c&0x1F)
			case c >= 0xE0 && c <= 0xEF:
				if c == 0xE0 {
					lower = 0xA0
				}
				if c == 0xED {
					upper = 0x9F
				}
				needed, cp = 2, rune(c&0x0F)
			case c >= 0xF0 && c <= 0xF4:
				if c == 0xF0 {
					lower = 0x90
				}
				if c == 0xF4 {
					upper = 0x8F
				}
				needed, cp = 3, rune(c&0x07)
			default:
				out = append(out, "�"...)
			}
			continue
		}
		if c < lower || c > upper {
			// The sequence so far is a maximal subpart; this byte starts over.
			cp, needed, seen = 0, 0, 0
			lower, upper = 0x80, 0xBF
			out = append(out, "�"...)
			i--
			continue
		}
		lower, upper = 0x80, 0xBF
		cp = cp<<6 | rune(c&0x3F)
		seen++
		if seen != needed {
			continue
		}
		out = utf8.AppendRune(out, cp)
		cp, needed, seen = 0, 0, 0
	}
	if needed != 0 {
		out = append(out, "�"...)
	}
	return string(out), true
}

// unescape resolves JSON escapes in text (valid UTF-8) and reports whether a
// lone surrogate escape was replaced by U+FFFD. A high surrogate followed by
// an escaped low surrogate forms one code point; any other surrogate escape
// is lone. An escape that is not one RFC 8259 allows is a syntax error.
func unescape(text string) (string, bool, error) {
	if strings.IndexByte(text, '\\') < 0 {
		return text, false, nil
	}
	out := make([]byte, 0, len(text))
	lone := false
	for i, n := 0, len(text); i < n; {
		c := text[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= n {
			return "", false, errors.New("dangling backslash")
		}
		e := text[i+1]
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
			i += 2
			continue
		case 'b':
			out = append(out, '\b')
			i += 2
			continue
		case 'f':
			out = append(out, '\f')
			i += 2
			continue
		case 'n':
			out = append(out, '\n')
			i += 2
			continue
		case 'r':
			out = append(out, '\r')
			i += 2
			continue
		case 't':
			out = append(out, '\t')
			i += 2
			continue
		case 'u':
		default:
			return "", false, errors.New("invalid escape sequence")
		}
		cp, err := hex4(text, i+2)
		if err != nil {
			return "", false, err
		}
		i += 6
		switch {
		case cp >= 0xD800 && cp <= 0xDBFF:
			if i+1 < n && text[i] == '\\' && text[i+1] == 'u' {
				low, err := hex4(text, i+2)
				if err != nil {
					return "", false, err
				}
				if low >= 0xDC00 && low <= 0xDFFF {
					out = utf8.AppendRune(out, 0x10000+(cp-0xD800)<<10+(low-0xDC00))
					i += 6
					continue
				}
			}
			out = append(out, "�"...)
			lone = true
		case cp >= 0xDC00 && cp <= 0xDFFF:
			out = append(out, "�"...)
			lone = true
		default:
			out = utf8.AppendRune(out, cp)
		}
	}
	return string(out), lone, nil
}

func hex4(text string, at int) (rune, error) {
	if at+4 > len(text) {
		return 0, errors.New("invalid \\u escape")
	}
	var cp rune
	for _, c := range []byte(text[at : at+4]) {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, errors.New("invalid \\u escape")
		}
		cp = cp<<4 | rune(d)
	}
	return cp, nil
}
