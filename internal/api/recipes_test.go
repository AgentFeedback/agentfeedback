package api

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// GET /skill?format=prompt serves the checked-in curl recipe with the request's
// base URL in place of the placeholder, so the recipe e2e runs is the one
// agents read.
func TestSkill_PromptIsTheCurlRecipe(t *testing.T) {
	recipe, err := os.ReadFile("../../docs/recipes/http-curl.md")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	r := e.do(t, http.MethodGet, "/skill?format=prompt", "", false)
	want := strings.ReplaceAll(string(recipe), "<server URL>", e.srv.URL)
	if r.status != http.StatusOK || string(r.body) != want {
		t.Fatalf("/skill?format=prompt %d is not docs/recipes/http-curl.md with base %s; run just skills", r.status, e.srv.URL)
	}
}
