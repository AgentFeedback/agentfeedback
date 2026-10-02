package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// loadContract loads docs/openapi.yaml with its external schema refs.
func loadContract(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	return doc
}

// TestRouter_EveryContractOperationIsRouted sends every operation of the
// contract this transport serves with a valid key and fails when the router
// itself answers (404 no such route, or 405).
func TestRouter_EveryContractOperationIsRouted(t *testing.T) {
	e := newEnv(t)
	doc := loadContract(t)
	n := 0
	for path, item := range doc.Paths.Map() {
		concrete := strings.NewReplacer("{id}", "1", "{kind}", "envelope", "{version}", "1", "{project}", "p").Replace(path)
		for method := range item.Operations() {
			n++
			var body string
			switch {
			case strings.HasPrefix(path, "/mcp"):
				body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
			case method == http.MethodPost || method == http.MethodPatch:
				body = "{}"
			}
			r := e.do(t, method, concrete, body, true)
			if r.status == http.StatusMethodNotAllowed {
				t.Errorf("%s %s: 405 %s", method, path, r.body)
			}
			if r.status == http.StatusNotFound && strings.Contains(string(r.body), "no such route") {
				t.Errorf("%s %s: router 404 %s", method, path, r.body)
			}
		}
	}
	if n < 16 {
		t.Fatalf("walked %d operations; the contract has more", n)
	}
	for _, path := range []string{"/health", "/ready", "/metrics"} {
		if r := e.do(t, http.MethodGet, path, "", false); r.status != http.StatusOK {
			t.Errorf("GET %s = %d %s", path, r.status, r.body)
		}
	}
}

func TestRouter_OperationalBodies(t *testing.T) {
	e := newEnv(t)
	if r := e.do(t, http.MethodGet, "/health", "", false); string(r.body) != "OK" {
		t.Errorf("/health body %q", r.body)
	}
	if r := e.do(t, http.MethodGet, "/ready", "", false); string(r.body) != "READY" {
		t.Errorf("/ready body %q", r.body)
	}
}
