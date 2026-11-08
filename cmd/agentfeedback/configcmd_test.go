package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigGetUnsetAndUsage(t *testing.T) {
	isolate(t)
	t.Chdir(t.TempDir())
	for _, key := range []string{"api_key", "url", "model", "harness", "context.app"} {
		r := runCLI(t, "", "config", "get", key)
		if r.code != 3 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("unset %s: %+v", key, r)
		}
	}
	for _, key := range []string{"api_key", "url", "collect.nope"} {
		r := runCLI(t, "", "config", "set", key, "do-not-echo-this-secret")
		if r.code != 2 || strings.Contains(r.stderr, "do-not-echo-this-secret") {
			t.Fatalf("refused %s: %+v", key, r)
		}
		want := "collect.deny_paths"
		if key != "collect.nope" {
			want = "init"
		}
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("missing next step for %s: %+v", key, r)
		}
	}
	if r := runCLI(t, "", "config", "get", "collect.nope"); r.code != 2 || !strings.Contains(r.stderr, "collect.deny_paths") {
		t.Fatalf("unknown get: %+v", r)
	}
}

func TestConfigShowNarrowingAndMaskedKey(t *testing.T) {
	path, _ := isolate(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(root, ".agentfeedback.toml")
	repo := "[collect]\ndisabled = true\ndeny_paths = [\"private\"]\n[detect]\nnudge = false\n[sessions]\npush = false\n"
	if err := os.WriteFile(repoPath, []byte(repo), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	putFile(t, path, "api_key = \"private-test-key\"\n[collect]\ndisabled = false\n[sessions]\nquiet = \"7m\"\n", 0o600)
	r := runCLI(t, "", "config", "show", "--json", "--local")
	var view configView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &view) != nil {
		t.Fatalf("show: %+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, "private-test-key") {
		t.Fatal("key exposed")
	}
	if view.Capture.State != "not_applicable" {
		t.Fatalf("capture state: %+v", view.Capture)
	}
	if !view.Repository.Disabled || view.Repository.PushAllowed || !view.Repository.NudgeOff || view.Repository.Path != repoPath {
		t.Fatalf("repository narrowing: %+v", view.Repository)
	}
	for _, s := range view.Settings {
		if s.Key == "collect.disabled" && (s.Value != false || s.Source != "file") {
			t.Fatalf("machine value overwritten by repository: %+v", s)
		}
		if s.Key == "mode" && (s.Value != "local" || s.Source != "flag") {
			t.Fatalf("flag provenance: %+v", s)
		}
	}
	if r := runCLI(t, "", "config", "set", "collect.disabled", "true"); r.code != 0 {
		t.Fatalf("set: %+v", r)
	}
	body, err := os.ReadFile(repoPath)
	if err != nil || string(body) != repo {
		t.Fatalf("repository modified: %q, %v", body, err)
	}
	if r := runCLI(t, "", "config", "get", "collect.disabled"); r.code != 0 || r.stdout != "true\n" {
		t.Fatalf("get changed value: %+v", r)
	}
}
