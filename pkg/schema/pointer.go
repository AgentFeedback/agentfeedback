package schema

import "strings"

// EscapeToken applies RFC 6901 escaping to one reference token: ~ becomes ~0,
// then / becomes ~1.
func EscapeToken(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}
