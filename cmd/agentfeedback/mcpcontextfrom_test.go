package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
)

const cfStdin = `{"summary":"deploy failed","key":"mine","project":"stdin-project","occurred_at":"2020-01-01T00:00:00Z","payload":{"category":"tooling","details":"d","suggested_fix":"f"}}`

// cfArgs is cfStdin as submit_feedback arguments with kind and context_from
// and, when ordinal is not 0, ordinal.
func cfArgs(t *testing.T, ref string, ordinal int) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(cfStdin), &v); err != nil {
		t.Fatal(err)
	}
	v["kind"], v["context_from"] = "friction", ref
	if ordinal != 0 {
		v["ordinal"] = ordinal
	}

	return v
}

// TestMcp_ContextFromBodyIsTheCLIs: submit_feedback's context_from builds
// exactly the body submit friction --stdin --context-from builds from the
// same members, ordinal included; a bad ref is a validation problem.
func TestMcp_ContextFromBodyIsTheCLIs(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "session-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	putSession(t, cfg, "s1", cwd)
	processRepo(t)
	svc := &contextFromService{stderr: &strings.Builder{}}
	for _, ordinal := range []int{0, 2} {
		args := []string{"submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3", "--dry-run"}
		if ordinal != 0 {
			args = append(args, "--ordinal", "2")
		}
		r := runCLI(t, cfStdin, args...)
		if r.code != 0 {
			t.Fatalf("dry run: %+v", r)
		}
		var want, got map[string]any
		decodeJSON(t, strings.Split(r.stdout, "\n")[0], &want)
		raw, _ := json.Marshal(cfArgs(t, "claude-code:s1#u-3", ordinal))
		body, err := svc.Body(context.Background(), raw)
		if err != nil {
			t.Fatalf("ordinal %d: %v", ordinal, err)
		}
		decodeJSON(t, string(body), &got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ordinal %d:\nmcp %s\ncli %s", ordinal, body, strings.Split(r.stdout, "\n")[0])
		}
		if n := max(ordinal, 1); got["key"] != sessions.Key("claude-code", "s1", "u-3", n, sessions.DetectorVersion) {
			t.Errorf("key %v", got["key"])
		}
	}
	for _, args := range []map[string]any{
		{"kind": "friction", "summary": "s", "context_from": "claude-code:s1"},
		{"kind": "friction", "summary": "s", "context_from": "claude-code:s1#nope"},
		{"kind": "friction", "summary": "s", "ordinal": 2},
		{"kind": "friction", "summary": "s", "context_from": "claude-code:s1#u-3", "ordinal": 0},
		{"summary": "s", "context_from": "claude-code:s1#u-3"},
		{"kind": "friction", "context_from": "claude-code:s1#u-3"},
	} {
		raw, _ := json.Marshal(args)
		_, err := svc.Body(context.Background(), raw)
		var p *core.Problem
		if !errors.As(err, &p) || p.Code != core.CodeValidation || len(p.Details) != 1 || p.Details[0].Pointer != "?"+mcp.ArgContextFrom {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// cfProcessWorld is a binary environment with a Claude Code session s1 in
// an existing directory, plus extra.
func cfProcessWorld(t *testing.T, extra ...string) []string {
	t.Helper()
	cfg := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "session-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	putSession(t, cfg, "s1", cwd)

	return binaryEnv(t, append([]string{"CLAUDE_CONFIG_DIR=" + cfg}, extra...)...)
}

// cfFileFromCLI files cfStdin through submit --context-from in env and
// returns the outcome.
func cfFileFromCLI(t *testing.T, env []string) string {
	t.Helper()
	cmd := binaryCommand(t, env, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3")
	cmd.Stdin = strings.NewReader(cfStdin)
	out, _ := cmd.Output()
	var o struct{ Outcome string }
	_ = json.Unmarshal(out, &o)

	return o.Outcome
}

// TestMcp_ContextFromFiles: in local mode and through the relay,
// submit_feedback with context_from files under the span's key, and the
// same span filed through the CLI afterwards is a duplicate: the body is
// the same.
func TestMcp_ContextFromFiles(t *testing.T) {
	wantKey := sessions.Key("claude-code", "s1", "u-3", 1, sessions.DetectorVersion)
	t.Run("local", func(t *testing.T) {
		env := cfProcessWorld(t)
		cs, _ := mcpProcess(t, env)
		sub := toolText(t, cs, "submit_feedback", cfArgs(t, "claude-code:s1#u-3", 0))
		if sub["submission"].(map[string]any)["key"] != wantKey {
			t.Fatalf("stored %+v", sub)
		}
		_ = cs.Close()
		if o := cfFileFromCLI(t, env); o != "duplicate" {
			t.Fatalf("the CLI filing of the same span: %q", o)
		}
	})
	t.Run("relay", func(t *testing.T) {
		url, svc := relayServer(t)
		env := cfProcessWorld(t, envURL+"="+url, envAPIKey+"="+testKey)
		cs, stderr := mcpProcess(t, env)
		sub := toolText(t, cs, "submit_feedback", cfArgs(t, "claude-code:s1#u-3", 0))
		id := int64(sub["submission"].(map[string]any)["id"].(float64))
		if rec, err := svc.Get(context.Background(), id); err != nil || rec.Key != wantKey {
			t.Fatalf("row on the server: %+v %v", rec, err)
		}
		_ = cs.Close()
		if o := cfFileFromCLI(t, env); o != "duplicate" {
			t.Fatalf("the CLI filing of the same span: %q", o)
		}
		noKeyIn(t, stderr.String())
	})
}
