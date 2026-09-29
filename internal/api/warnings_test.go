package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// hasMember reports whether v holds an object member named name at any depth.
func hasMember(v any, name string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if k == name || hasMember(c, name) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if hasMember(c, name) {
				return true
			}
		}
	}
	return false
}

func TestWarnings_OnlyOnTheCreateResponse(t *testing.T) {
	e := newEnv(t)
	body := `{"kind":"friction","summary":true,"machine":42,"payload":{"category":"tooling","details":"d"},"severity":"high"}`
	r := e.do(t, http.MethodPost, "/api/v1/submissions", body, true)
	if r.status != http.StatusCreated {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	v := r.json(t)
	warnings, _ := v["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("no warnings: %s", r.body)
	}
	sub := v["submission"].(map[string]any)
	if hasMember(sub, "warnings") {
		t.Errorf("submission carries warnings: %s", r.body)
	}
	id := fmt.Sprint(sub["id"])
	if loc := r.header.Get("Location"); loc != "/api/v1/submissions/"+id {
		t.Errorf("Location = %q", loc)
	}

	// The replay returns the warnings again, beside the same submission.
	r2 := e.do(t, http.MethodPost, "/api/v1/submissions", body, true)
	if r2.status != http.StatusOK || r2.header.Get("Location") != "/api/v1/submissions/"+id {
		t.Fatalf("replay = %d Location %q", r2.status, r2.header.Get("Location"))
	}
	if w, _ := r2.json(t)["warnings"].([]any); len(w) != len(warnings) {
		t.Errorf("replay warnings %v, want %v", w, warnings)
	}

	for _, path := range []string{"/api/v1/submissions/" + id, "/api/v1/submissions?include=payload", "/api/v1/submissions"} {
		g := e.do(t, http.MethodGet, path, "", true)
		if g.status != http.StatusOK {
			t.Fatalf("GET %s = %d", path, g.status)
		}
		if hasMember(g.json(t), "warnings") || strings.Contains(string(g.body), `"warnings"`) {
			t.Errorf("GET %s carries warnings: %s", path, g.body)
		}
	}
}
