package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireAPIKey returns middleware that authenticates requests against apiKey.
// It accepts the key via "Authorization: Bearer <key>" or "X-Api-Key: <key>",
// comparing in constant time. Requests missing or presenting a wrong key get
// 401 with the standard error shape.
func requireAPIKey(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := extractAPIKey(r)
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// extractAPIKey reads the API key from "Authorization: Bearer <key>" or
// "X-Api-Key: <key>".
func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if key, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return key
		}
	}

	return r.Header.Get("X-Api-Key")
}
