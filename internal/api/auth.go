package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireAPIKey returns middleware that authenticates requests against
// apiKey. A request is authorised when either "Authorization: <scheme> <key>"
// with the scheme bearer (any case) or "X-Api-Key: <key>" carries the key;
// both are checked, so neither shadows the other. Comparison is constant
// time and an empty configured key authorises nothing. Failure is 401
// unauthorized with WWW-Authenticate: Bearer.
func requireAPIKey(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authorised(r, apiKey) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "missing or invalid API key; send Authorization: Bearer <key> or X-Api-Key: <key>", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authorised(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return false
	}
	ok := false
	for _, v := range r.Header.Values("Authorization") {
		scheme, cred, found := strings.Cut(v, " ")
		if found && strings.EqualFold(scheme, "bearer") && keyEqual(strings.TrimLeft(cred, " "), apiKey) {
			ok = true
		}
	}
	for _, v := range r.Header.Values("X-Api-Key") {
		if keyEqual(v, apiKey) {
			ok = true
		}
	}
	return ok
}

// keyEqual compares the SHA-256 digests of both keys in constant time, so
// neither the key's content nor its length leaks through timing. Callers
// reject an empty configured key before calling.
func keyEqual(provided, apiKey string) bool {
	p, k := sha256.Sum256([]byte(provided)), sha256.Sum256([]byte(apiKey))
	return subtle.ConstantTimeCompare(p[:], k[:]) == 1
}
