// Package canonjson writes canonical JSON as the v1 contract defines it,
// byte by byte, from the write path's JSON tree.
//
// Canonical form is UTF-8 with no whitespace. Object members are sorted by
// the byte order of their UTF-8 keys, which is code point order, at every
// level. Arrays keep their order. Numbers are written verbatim from their
// json.Number spelling. Strings are escaped minimally: '"', '\\', LF, CR and
// TAB by name, the other C0 controls as \u00xx, everything else raw.
//
// The writer never uses encoding/json's encoder: it HTML-escapes and escapes
// U+2028 and U+2029, which canonical form forbids.
//
// The tree is map[string]any, []any, string, json.Number, bool and nil. Any
// other Go value is a programming error and panics, like pkg/schema; so is a
// json.Number that is not spelled as a JSON number or a string that is not
// valid UTF-8, since either would make the output not JSON. The decoder
// never produces them; the checks guard hand-built trees.
package canonjson

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// Marshal returns the canonical form of v.
func Marshal(v any) []byte {
	return Append(nil, v)
}

// Append appends the canonical form of v to dst and returns the result.
func Append(dst []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if x {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case json.Number:
		if !isNumber(string(x)) {
			panic(fmt.Sprintf("canonjson: json.Number %q is not a JSON number", string(x)))
		}
		return append(dst, x...)
	case string:
		if !utf8.ValidString(x) {
			panic("canonjson: string is not valid UTF-8")
		}
		return AppendString(dst, x)
	case []any:
		dst = append(dst, '[')
		for i, item := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = Append(dst, item)
		}
		return append(dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, k)
			dst = append(dst, ':')
			dst = Append(dst, x[k])
		}
		return append(dst, '}')
	default:
		panic(fmt.Sprintf("canonjson: not a JSON tree value: %T", v))
	}
}

// AppendString appends s as a canonical JSON string to dst and returns the
// result. It assumes s is valid UTF-8 and copies every byte that needs no
// escape through unchanged, so non-ASCII text is never re-encoded. Append
// checks validity before calling it; direct callers own that check.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

// isNumber reports whether s is spelled as an RFC 8259 number:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func isNumber(s string) bool {
	i, n := 0, len(s)
	if i < n && s[i] == '-' {
		i++
	}
	switch {
	case i < n && s[i] == '0':
		i++
	case i < n && s[i] >= '1' && s[i] <= '9':
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < n && s[i] == '.' {
		i++
		if i >= n || s[i] < '0' || s[i] > '9' {
			return false
		}
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < n && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= n || s[i] < '0' || s[i] > '9' {
			return false
		}
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	return i == n
}
