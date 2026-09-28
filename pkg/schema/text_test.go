package schema

import "testing"

func TestTrimUsesWhiteSpaceProperty(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"ascii", "  a b  ", "a b"},
		{"tab and newline", "\t\na\r\n", "a"},
		{"NEL U+0085", "\u0085a\u0085", "a"},
		{"no-break space", "\u00a0a\u00a0", "a"},
		{"ideographic space", "\u3000a\u3000", "a"},
		{"line and paragraph separators", "\u2028a\u2029", "a"},
		{"en quad to hair space", "\u2000\u200aa", "a"},
		{"U+FEFF is not whitespace", "\uFEFFa\uFEFF", "\uFEFFa\uFEFF"},
		{"U+001C is not whitespace", "\x1ca", "\x1ca"},
		{"zero width space is not whitespace", "\u200ba", "\u200ba"},
		{"all whitespace", " \t\u3000", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Trim(tc.in); got != tc.want {
				t.Fatalf("Trim(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLowerSimple(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"ascii", "Claude Code", "claude code"},
		{"already lower", "abc", "abc"},
		{"U+0130 to i alone", "İ", "i"},
		{"final sigma stays U+03C3", "ΣΣ", "σσ"},
		{"kelvin sign to k", "K", "k"},
		{"latin-1", "É", "é"},
		{"non-letters untouched", "1-2_3", "1-2_3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := LowerSimple(tc.in); got != tc.want {
				t.Fatalf("LowerSimple(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToken(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"trim lower dash", "  Claude   Code ", "claude-code"},
		{"tabs and NEL are whitespace", "a\t\u0085b", "a-b"},
		{"interior run of many", "a \u3000\u2003 b", "a-b"},
		{"already a token", "opencode", "opencode"},
		{"empty after trim", " \t ", ""},
		{"U+FEFF kept", "\uFEFFX", "\uFEFFx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Token(tc.in); got != tc.want {
				t.Fatalf("Token(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewlinesToSpaces(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"crlf is one space", "a\r\nb", "a b"},
		{"lone cr", "a\rb", "a b"},
		{"lone lf", "a\nb", "a b"},
		{"lf cr is two spaces", "a\n\rb", "a  b"},
		{"none", "ab", "ab"},
		{"trailing crlf", "a\r\n", "a "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NewlinesToSpaces(tc.in); got != tc.want {
				t.Fatalf("NewlinesToSpaces(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
		cut   bool
	}{
		{"under limit", "abc", 5, "abc", false},
		{"at limit", "abc", 3, "abc", false},
		{"ascii cut", "abcdef", 3, "abc", true},
		{"cut inside a two-byte code point", "aé", 2, "a", true},
		{"cut after a two-byte code point", "aéb", 3, "aé", true},
		{"cut inside a four-byte code point", "\U0001f600x", 3, "", true},
		{"zero limit", "a", 0, "", true},
		{"negative limit", "a", -1, "", true},
		{"empty", "", 3, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, cut := TruncateBytes(tc.in, tc.limit)
			if got != tc.want || cut != tc.cut {
				t.Fatalf("TruncateBytes(%q, %d) = %q, %v; want %q, %v", tc.in, tc.limit, got, cut, tc.want, tc.cut)
			}
		})
	}
}

func TestEscapeToken(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"a/b", "a~1b"}, {"m~n", "m~0n"}, {"~/", "~0~1"}, {"~1", "~01"}, {"plain", "plain"}, {"", ""},
	}
	for _, tc := range tests {
		if got := EscapeToken(tc.in); got != tc.want {
			t.Errorf("EscapeToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
