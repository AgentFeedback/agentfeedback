package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAPIKey(t *testing.T) {
	t.Parallel()

	const apiKey = "s3cr3t"

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{name: "no key", headers: map[string]string{}, wantStatus: http.StatusUnauthorized},
		{name: "wrong key via bearer", headers: map[string]string{"Authorization": "Bearer wrong"}, wantStatus: http.StatusUnauthorized},
		{name: "wrong key via x-api-key", headers: map[string]string{"X-Api-Key": "wrong"}, wantStatus: http.StatusUnauthorized},
		{name: "bearer ok", headers: map[string]string{"Authorization": "Bearer " + apiKey}, wantStatus: http.StatusOK},
		{name: "x-api-key ok", headers: map[string]string{"X-Api-Key": apiKey}, wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/api/v1/submissions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			requireAPIKey(apiKey)(okHandler).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d (body: %s)", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}
