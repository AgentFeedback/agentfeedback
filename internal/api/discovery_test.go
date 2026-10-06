package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

func TestDiscovery_Document(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, http.MethodGet, "/.well-known/agentfeedback.json", "", false)
	want := `{"service":"agentfeedback","version":"4.0.0","api_version":"1.0","openapi":"/api/v1/openapi.json","schemas":"/api/v1/schemas","mcp":"/mcp","skill":"/skill","auth":{"modes":["bearer","x-api-key"]},"docs":"https://agentfeedback.dev/docs"}`
	if r.status != http.StatusOK || string(r.body) != want {
		t.Fatalf("discovery %d: %s", r.status, r.body)
	}
	if r.header.Get("Content-Type") != "application/json" || r.header.Get("Cache-Control") != "" {
		t.Errorf("headers %v", r.header)
	}
}

func TestSkill_Formats(t *testing.T) {
	e := newEnv(t)
	for q, form := range map[string]string{"": skillgen.FormSkillMD, "?format=skill-md": skillgen.FormSkillMD,
		"?format=agents-md": skillgen.FormAgentsMD, "?format=prompt": skillgen.FormPrompt} {
		r := e.do(t, http.MethodGet, "/skill"+q, "", false)
		want, err := skillgen.Render(form, e.srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusOK || string(r.body) != string(want) {
			t.Errorf("/skill%s: %d, body differs from the %s render", q, r.status, form)
		}
		if r.header.Get("Content-Type") != "text/markdown; charset=utf-8" || r.header.Get("Cache-Control") != "" ||
			r.header.Get("Vary") != "Host, X-Forwarded-Proto" {
			t.Errorf("/skill%s headers %v", q, r.header)
		}
	}
	for q, detail := range map[string][2]string{
		"?format=cursor":                    {detailRange, "?format"},
		"?format=mcp":                       {detailRange, "?format"},
		"?format=":                          {detailEmpty, "?format"},
		"?format=prompt&format=prompt":      {detailRepeated, "?format"},
		"?other=1":                          {detailUnknown, "?other"},
		"?format=prompt&server=http://x.io": {detailUnknown, "?server"},
	} {
		assertDetail(t, e.do(t, http.MethodGet, "/skill"+q, "", false), detail[0], detail[1])
	}
	r := e.do(t, http.MethodPost, "/skill", "", false)
	assertError(t, r, http.StatusMethodNotAllowed, codeMethodNotAllowed)
	if !strings.Contains(r.header.Get("Allow"), "GET") {
		t.Errorf("Allow %q", r.header.Get("Allow"))
	}
}

func TestSkill_BaseURL(t *testing.T) {
	derived := newEnv(t)
	host := strings.TrimPrefix(derived.srv.URL, "http://")
	req := derived.request(t, http.MethodGet, "/skill?format=prompt", nil, false)
	req.Header.Set("X-Forwarded-Proto", "HTTPS")
	r := send(t, req)
	if !strings.Contains(string(r.body), "https://"+host+"/api/v1") {
		t.Errorf("prompt does not name https://%s: %.400s", host, r.body)
	}
	req = derived.request(t, http.MethodGet, "/skill?format=prompt", nil, false)
	req.Host = "bad host"
	if r := send(t, req); r.status != http.StatusOK || !strings.Contains(string(r.body), "<server URL>") {
		t.Errorf("an unusable Host should render the placeholder: %d %.400s", r.status, r.body)
	}

	req = derived.request(t, http.MethodGet, "/skill?format=prompt", nil, false)
	req.Host = strings.Repeat("a", 250) + ".example"
	if r := send(t, req); r.status != http.StatusOK || !strings.Contains(string(r.body), "<server URL>") || strings.Contains(string(r.body), "aaaa") {
		t.Errorf("a Host past the base URL limit should render the placeholder: %d %.400s", r.status, r.body)
	}

	fixed := newEnvWith(t, Config{PublicURL: "https://feedback.example.com"})
	req = fixed.request(t, http.MethodGet, "/skill?format=prompt", nil, false)
	req.Header.Set("X-Forwarded-Proto", "http")
	r = send(t, req)
	want, err := skillgen.Render(skillgen.FormPrompt, "https://feedback.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.body) != string(want) || !strings.Contains(string(r.body), "https://feedback.example.com") {
		t.Errorf("PUBLIC_URL not used: %.400s", r.body)
	}
	if r.header.Get("Vary") != "" {
		t.Errorf("Vary %q with PUBLIC_URL set", r.header.Get("Vary"))
	}
}
