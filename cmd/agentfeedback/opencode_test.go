package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInstallOpenCodeGuardKeepsOtherHarnesses(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "dry-run"}[dryRun], func(t *testing.T) {
			e := newInstallEnv(t)
			putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf '2.0.1\\n'\n", 0o755)
			before := snapshot(t, e.home)
			args := []string{"install", "opencode", "claude-code", "--server", "local"}
			wantStatus := "installed"
			if dryRun {
				args = append(args, "--dry-run")
				wantStatus = "dry_run"
			}
			r, out := installRun(t, args...)
			if r.code != 0 || out["status"] != wantStatus || harnessField(t, out, "claude-code", "hook") != "wired" {
				t.Fatalf("mixed request: %+v %v", r, out)
			}
			if !strings.Contains(r.stderr, "opencode-v2") {
				t.Fatalf("missing warning: %+v", r)
			}
			warnings, ok := out["warnings"].([]any)
			if !ok || len(warnings) == 0 || !strings.Contains(warnings[0].(string), "opencode-v2") {
				t.Fatalf("missing outcome warning: %v", out)
			}
			if _, err := os.Stat(filepath.Join(e.home, ".config", "opencode")); !os.IsNotExist(err) {
				t.Fatalf("V2 alias created OpenCode wiring: %v", err)
			}
			if dryRun {
				sameTree(t, "mixed dry-run", snapshot(t, e.home), before)
			}
		})
	}
}

func TestInstallOpenCodeUnknownVersionGuard(t *testing.T) {
	for _, script := range []string{
		"printf 'unrecognized version\\n'\n",
		"printf 'probe failed\\n' >&2\nexit 1\n",
	} {
		t.Run(script, func(t *testing.T) {
			e := newInstallEnv(t)
			putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\n"+script, 0o755)
			before := snapshot(t, e.home)
			r, out := installRun(t, "install", "opencode", "--server", "local")
			if r.code != 0 || out["status"] != "unchanged" || !strings.Contains(r.stderr, "run agentfeedback install opencode-v1") {
				t.Fatalf("unknown version guard: %+v %v", r, out)
			}
			sameTree(t, "unknown version guard", snapshot(t, e.home), before)
			r, out = installRun(t, "install", "opencode-v1", "--server", "local")
			if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "opencode-v1", "hook") != "wired" {
				t.Fatalf("verified explicit V1: %+v %v", r, out)
			}
		})
	}
}

func TestInstallOpenCodeAliasOnlySkipsLockAndResolution(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "dry-run"}[dryRun], func(t *testing.T) {
			e := newInstallEnv(t)
			putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf '2.0.1\\n'\n", 0o755)
			env, err := harnessEnv()
			if err != nil {
				t.Fatal(err)
			}
			// Lock would fail opening this home; neither it nor resolution
			// is needed for an unsupported alias-only request.
			env.Home = filepath.Join(e.home, "missing")
			before := snapshot(t, e.home)
			pos := []string{"opencode"}
			var stderr bytes.Buffer
			out, err := wireHarnesses(env, pos, wireOptions{dryRun: dryRun}, func() (string, bool, error) {
				t.Fatal("unexpected server resolution")
				return "", false, nil
			}, "install", &stderr)
			wantStatus := "unchanged"
			if dryRun {
				wantStatus = "dry_run"
			}
			if err != nil || out.Status != wantStatus || len(out.Warnings) != 1 || !strings.Contains(stderr.String(), "opencode-v2") {
				t.Fatalf("alias-only outcome: %+v %v %s", out, err, stderr.String())
			}
			if !slices.Equal(pos, []string{"opencode"}) {
				t.Fatalf("caller requests mutated: %v", pos)
			}
			sameTree(t, "alias-only", snapshot(t, e.home), before)
		})
	}
}

func TestInstallOpenCodeVersionGuard(t *testing.T) {
	e := newInstallEnv(t)
	binary := filepath.Join(e.bin, "opencode")
	putFile(t, binary, "#!/bin/sh\nprintf '2.0.1\\n'\n", 0o755)
	before := snapshot(t, e.home)
	r, out := installRun(t, "install", "opencode", "--server", "local")
	if r.code != 0 || out["status"] != "unchanged" || !strings.Contains(r.stderr, "opencode-v2") {
		t.Fatalf("V2 alias guard: %+v", r)
	}
	warnings, ok := out["warnings"].([]any)
	if !ok || len(warnings) != 1 || !strings.Contains(warnings[0].(string), "opencode-v2") {
		t.Fatalf("V2 warning missing from JSON: %v", out)
	}
	sameTree(t, "V2 alias guard", snapshot(t, e.home), before)

	if r, out = installRun(t, "install", "opencode-v1", "--server", "local"); r.code != 0 || out["status"] != "installed" || harnessField(t, out, "opencode-v1", "hook") != "wired" {
		t.Fatalf("explicit V1: %+v", r)
	}
	rep, err := diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil || !hasProblem(rep, "uninstall opencode-v1") {
		t.Fatalf("incompatible plugin diagnosis: %+v %v", rep.Problems, err)
	}
	if r, _ := installRun(t, "uninstall", "opencode"); r.code != 0 {
		t.Fatalf("legacy uninstall on V2: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config", "opencode", "plugins", "agentfeedback.js")); !os.IsNotExist(err) {
		t.Fatalf("plugin not removed: %v", err)
	}

	putFile(t, binary, "#!/bin/sh\nprintf '1.18.35\\n'\n", 0o755)
	if r, out = installRun(t, "install", "opencode", "--server", "local"); r.code != 0 || out["status"] != "installed" || harnessField(t, out, "opencode-v1", "hook") != "wired" {
		t.Fatalf("V1 alias: %+v", r)
	}
}

func TestSubmitOpenCodeAlias(t *testing.T) {
	isolateCLI(t)
	for _, args := range [][]string{
		{"--harness", "opencode"},
		{"--stdin"},
	} {
		r := runCLI(t, `{"harness":"opencode"}`, append([]string{"submit", "friction", "--dry-run", "--category", "tooling", "--summary", "failure", "--suggested-fix", "repair"}, args...)...)
		if r.code != 0 {
			t.Fatalf("submit: %+v", r)
		}
		var body struct {
			Harness string `json:"harness"`
		}
		if err := json.NewDecoder(strings.NewReader(r.stdout)).Decode(&body); err != nil || body.Harness != "opencode-v1" {
			t.Fatalf("canonical submission: %s %v", r.stdout, err)
		}
	}
}
