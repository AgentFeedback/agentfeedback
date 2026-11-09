package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

func TestInstallOpenCodeAliasV2KeepsOtherHarnesses(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "dry-run"}[dryRun], func(t *testing.T) {
			e := newInstallEnv(t)
			putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'opencode v2.2.6\\n'\n", 0o755)
			before := snapshot(t, e.home)
			args := []string{"install", "opencode", "claude-code", "--server", "local"}
			wantStatus := "installed"
			if dryRun {
				args = append(args, "--dry-run")
				wantStatus = "dry_run"
			}
			r, out := installRun(t, args...)
			if r.code != 0 || out["status"] != wantStatus || harnessField(t, out, "claude-code", "hook") != "wired" || harnessField(t, out, "opencode-v2", "hook") != "wired" {
				t.Fatalf("mixed request: %+v %v", r, out)
			}
			if strings.Contains(fmt.Sprint(out["warnings"]), "opencode") {
				t.Fatalf("unexpected OpenCode warning: %v", out["warnings"])
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
			if r.code != 0 || out["status"] != "unchanged" || !strings.Contains(r.stderr, "name opencode-v1 or opencode-v2 explicitly") {
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
			putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'unrecognized version\\n'\n", 0o755)
			env, err := harnessEnv()
			if err != nil {
				t.Fatal(err)
			}
			// Lock would fail opening this home; neither it nor resolution
			// is needed for an alias-only request of an unknown version.
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
			if err != nil || out.Status != wantStatus || len(out.Warnings) != 1 || !strings.Contains(stderr.String(), "name opencode-v1 or opencode-v2 explicitly") {
				t.Fatalf("alias-only outcome: %+v %v %s", out, err, stderr.String())
			}
			if !slices.Equal(pos, []string{"opencode"}) {
				t.Fatalf("caller requests mutated: %v", pos)
			}
			sameTree(t, "alias-only", snapshot(t, e.home), before)
		})
	}
}

func TestInstallOpenCodeV2(t *testing.T) {
	e := newInstallEnv(t)
	putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'opencode v2.2.6\\n'\n", 0o755)
	oc := filepath.Join(e.home, ".config", "opencode")
	plugin := filepath.Join(oc, "plugins", "agentfeedback.js")
	before := snapshot(t, e.home)

	list := listJSON(t)
	if list["opencode-v2"].Detected != "binary" || list["opencode-v1"].Detected != "no" {
		t.Fatalf("list: v1 %+v v2 %+v", list["opencode-v1"], list["opencode-v2"])
	}

	for _, name := range []string{"opencode-v2", "opencode"} {
		r, out := installRun(t, "install", name, "--server", "local")
		if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "opencode-v2", "hook") != "wired" || strings.Contains(fmt.Sprint(out["warnings"]), "opencode") {
			t.Fatalf("install %s: %+v %v", name, r, out)
		}
		data, err := os.ReadFile(plugin)
		if err != nil || !strings.Contains(string(data), `const HARNESS = "opencode-v2";`) || !strings.Contains(string(data), "ctx.session.instructions.entry.put") {
			t.Fatalf("plugin %v:\n%s", err, data)
		}
		if r, _ := installRun(t, "uninstall", name); r.code != 0 {
			t.Fatalf("uninstall %s: %+v", name, r)
		}
		sameTree(t, "after uninstall "+name, snapshot(t, e.home), before)
	}

	r, out := installRun(t, "install", "opencode", "--mcp", "--server", "local")
	if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "opencode-v2", "mcp") != "stdio" {
		t.Fatalf("install --mcp: %+v %v", r, out)
	}
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	data, err := os.ReadFile(filepath.Join(oc, "opencode.json"))
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		t.Fatalf("config %v:\n%s", err, data)
	}
	want := `{"agentfeedback":{"type":"local","command":[` + string(mustJSONString(e.exe)) + `,"mcp"]}}`
	if len(cfg.MCP) != 1 || strings.Join(strings.Fields(string(cfg.MCP["servers"])), "") != want {
		t.Fatalf("mcp member:\n%s\nwant servers %s", data, want)
	}
	if r, _ := installRun(t, "uninstall", "opencode"); r.code != 0 {
		t.Fatalf("uninstall --mcp: %+v", r)
	}
	sameTree(t, "after uninstall --mcp", snapshot(t, e.home), before)

	// V1 wiring under an OpenCode 2 binary: doctor points at opencode-v2,
	// whose install replaces it.
	if r, out = installRun(t, "install", "opencode-v1", "--server", "local"); r.code != 0 || out["status"] != "installed" {
		t.Fatalf("explicit V1: %+v", r)
	}
	rep, err := diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil || !hasProblem(rep, "run agentfeedback install opencode-v2, which replaces it") {
		t.Fatalf("incompatible plugin diagnosis: %+v %v", rep.Problems, err)
	}
	r, out = installRun(t, "install", "opencode", "--server", "local")
	notes, _ := harnessField(t, out, "opencode-v2", "notes").([]any)
	if r.code != 0 || !slices.ContainsFunc(notes, func(n any) bool { return strings.HasPrefix(n.(string), "replaced the opencode-v1 wiring") }) {
		t.Fatalf("replace V1: %+v %v", r, out)
	}
	rep, err = diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil || hasProblem(rep, "opencode-v1") {
		t.Fatalf("diagnosis after replace: %+v %v", rep.Problems, err)
	}
	if r, out = installRun(t, "install", "opencode-v1", "opencode-v2", "--server", "local"); r.code == 0 || !strings.Contains(fmt.Sprint(out["message"]), "share one configuration directory") {
		t.Fatalf("both variants: %+v %v", r, out)
	}
	if r, _ := installRun(t, "uninstall", "opencode"); r.code != 0 {
		t.Fatalf("alias uninstall: %+v", r)
	}
	sameTree(t, "after alias uninstall", snapshot(t, e.home), before)
}

func TestInstallOpenCodeV2OnV1(t *testing.T) {
	e := newInstallEnv(t)
	putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf '1.19.5\\n'\n", 0o755)
	list := listJSON(t)
	if list["opencode-v2"].Detected != "no" || list["opencode-v1"].Detected != "binary" {
		t.Fatalf("list: v1 %+v v2 %+v", list["opencode-v1"], list["opencode-v2"])
	}
	r, out := installRun(t, "install", "opencode-v2", "--server", "local")
	notes, _ := harnessField(t, out, "opencode-v2", "notes").([]any)
	if r.code != 0 || out["status"] != "installed" || !slices.Contains(notes, any("opencode-v2 was not detected on this machine; it was wired anyway")) {
		t.Fatalf("named V2 on V1: %+v %v", r, out)
	}
	// The alias follows the record on uninstall, whatever the version.
	if r, _ := installRun(t, "uninstall", "opencode"); r.code != 0 {
		t.Fatalf("alias uninstall: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config", "opencode", "plugins", "agentfeedback.js")); !os.IsNotExist(err) {
		t.Fatalf("plugin not removed: %v", err)
	}
	if r, out = installRun(t, "install", "opencode", "--server", "local"); r.code != 0 || harnessField(t, out, "opencode-v1", "hook") != "wired" {
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

func TestInstallCheckOpenCodeAliasV2(t *testing.T) {
	e := newInstallEnv(t)
	putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'opencode v2.2.6\\n'\n", 0o755)
	r := runCLI(t, "", "install", "--check", "--json", "opencode")
	if !strings.Contains(r.stdout, `"name":"opencode-v2"`) || strings.Contains(r.stdout, `"opencode-v1"`) {
		t.Fatalf("check: %+v", r)
	}
}

func TestManualStepsOpenCodeAliasV2(t *testing.T) {
	e := newInstallEnv(t)
	putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'opencode v2.2.6\\n'\n", 0o755)
	var stderr bytes.Buffer
	steps, _ := manualSteps("install", []string{"opencode"}, "local", &stderr)
	if len(steps) != 1 || steps[0].Harness != "opencode-v2" {
		t.Fatalf("steps %+v\n%s", steps, stderr.String())
	}
}

// OpenCode 3.x is no verified variant: every path that resolves the
// opencode alias skips it with the unknown-version warning.
func TestOpenCodeAliasMajor3(t *testing.T) {
	e := newInstallEnv(t)
	putFile(t, filepath.Join(e.bin, "opencode"), "#!/bin/sh\nprintf 'opencode v3.0.0\\n'\n", 0o755)
	before := snapshot(t, e.home)
	list := listJSON(t)
	if list["opencode-v1"].Detected != "no" || list["opencode-v2"].Detected != "no" {
		t.Fatalf("list: v1 %+v v2 %+v", list["opencode-v1"], list["opencode-v2"])
	}
	r, out := installRun(t, "install", "opencode", "--server", "local")
	if r.code != 0 || out["status"] != "unchanged" || !strings.Contains(r.stderr, "name opencode-v1 or opencode-v2 explicitly") {
		t.Fatalf("install: %+v %v", r, out)
	}
	sameTree(t, "install opencode on 3.x", snapshot(t, e.home), before)
	r = runCLI(t, "", "install", "--check", "--json", "opencode")
	if r.code != 1 || strings.Contains(r.stdout, `"current"`) || !strings.Contains(r.stdout, "name opencode-v1 or opencode-v2 explicitly") {
		t.Fatalf("check: %+v", r)
	}
	var stderr bytes.Buffer
	steps, warnings := manualSteps("install", []string{"opencode"}, "local", &stderr)
	if len(steps) != 0 || !slices.Contains(warnings, harness.OpenCodeUnknownVersion) || strings.Count(stderr.String(), harness.OpenCodeUnknownVersion) != 1 {
		t.Fatalf("manual steps %+v %v\n%s", steps, warnings, stderr.String())
	}
}
