package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSessionsPushSelectionValidation(t *testing.T) {
	sessionsWorld(t)
	for _, args := range [][]string{
		{},
		{"--due", "claude-code:s1"},
		{"--due", "--limit", "-1"},
		{"--due", "--since", ""},
		{"--due", "--since", "yesterday"},
		{"not-a-reference"},
	} {
		if r := runCLI(t, "", append([]string{"sessions", "push"}, args...)...); r.code != 2 {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if r := runCLI(t, "", "sessions", "push", "--due"); r.code != 1 {
		t.Fatalf("local capture must not upload: %+v", r)
	}
}

func TestSessionsPushRefusalAndPreviewDoNotUpload(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "project")
	putSession(t, cfg, "s1", cwd)
	var uploads atomic.Int32
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/openapi.json":
			fmt.Fprint(w, `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`)
		case "/api/v1/sessions/policy":
			fmt.Fprint(w, `{"org_id":"11111111-1111-4111-8111-111111111111","enabled":true,"notice_version":"1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`)
		case "/api/v1/meta":
			fmt.Fprint(w, `{}`)
		default:
			uploads.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
	r := runCLI(t, "", "sessions", "push", "claude-code:s1", "--since", "2020-01-01T00:00:00Z", "--dry-run", "--json")
	var preview []struct {
		Status  string `json:"status"`
		Preview string `json:"preview"`
	}
	decodeJSON(t, r.stdout, &preview)
	if r.code != 0 || len(preview) != 1 || preview[0].Status != "dry-run" || !strings.Contains(preview[0].Preview, "The deploy is still failing") {
		t.Fatalf("dry-run: %+v", r)
	}
	writeClientConfig(t, "[collect]\ndeny_paths = ["+string(mustJSONString(cwd))+"]\n")
	r = runCLI(t, "", "sessions", "push", "claude-code:s1", "--since", "2020-01-01T00:00:00Z", "--json")
	var refused []struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &refused); err != nil || r.code != 1 || len(refused) != 1 || refused[0].Status != "skipped" {
		t.Fatalf("denied named push: %+v", r)
	}
	if uploads.Load() != 0 {
		t.Fatalf("refusal/dry-run made %d upload requests", uploads.Load())
	}
}
