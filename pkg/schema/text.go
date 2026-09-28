package schema

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsWhiteSpace reports whether r has the Unicode White_Space property, the
// set the contract uses for trimming and for the token rule (unchanged since
// Unicode 6.3). U+FEFF is not whitespace.
func IsWhiteSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r == 0x20, r == 0x85, r == 0xA0, r == 0x1680:
		return true
	case r >= 0x2000 && r <= 0x200A:
		return true
	case r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// Trim removes leading and trailing White_Space code points (x-trim).
func Trim(s string) string {
	return strings.TrimFunc(s, IsWhiteSpace)
}

// LowerSimple lower-cases every code point with the simple, context-free
// mapping: U+0130 becomes U+0069 alone, a capital sigma becomes U+03C3
// wherever it stands, KELVIN SIGN becomes k. s must be valid UTF-8.
func LowerSimple(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' || c >= utf8.RuneSelf {
			var b strings.Builder
			b.Grow(len(s))
			b.WriteString(s[:i])
			for _, r := range s[i:] {
				b.WriteRune(unicode.ToLower(r))
			}
			return b.String()
		}
	}
	return s
}

// Token applies x-normalize: token. Trim, lower-case, then every run of
// whitespace becomes one "-".
func Token(s string) string {
	s = LowerSimple(Trim(s))
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if IsWhiteSpace(r) {
			if !inSpace {
				b.WriteByte('-')
				inSpace = true
			}
			continue
		}
		b.WriteRune(r)
		inSpace = false
	}
	return b.String()
}

// NewlinesToSpaces is the summary rule: CR LF, a lone CR and a lone LF each
// become one space.
func NewlinesToSpaces(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			b.WriteByte(' ')
		case '\n':
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TruncateBytes cuts s at the last complete code point at or before limit
// UTF-8 bytes. It reports whether anything was cut.
func TruncateBytes(s string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
