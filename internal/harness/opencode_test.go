package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpenCodeVersionDetection(t *testing.T) {
	for _, tt := range []struct {
		version  string
		major    int
		detected string
	}{
		{"1.18.35\n", 1, "binary"},
		{"2.0.0\n", 2, "no"},
		{"not a version", 0, "no"},
		{"v1.18.35", 1, "binary"},
		{"warning: using fallback config\nopencode v1.18.35\n", 1, "binary"},
		{"OpenCode\nwarning: update available\n2.0.1\n", 2, "no"},
		{"1.18.35\n2.0.1\n", 0, "no"},
		{"1.18.35\nv1.18.35\n", 1, "binary"},
		{"1.not-a-version\n", 0, "no"},
		{"warning: requires 1.18.35 or newer\n", 0, "no"},
	} {
		t.Run(tt.version, func(t *testing.T) {
			e := testEnv(t)
			put(t, filepath.Join(e.opencodeDir(), "opencode.json"), "{}")
			e.LookPath = func(string) (string, error) { return "/stub/opencode", nil }
			e.Exec = func(ctx context.Context, env map[string]string, bin string, args ...string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing version timeout")
				}
				if bin != "/stub/opencode" || !slices.Equal(args, []string{"--version"}) {
					t.Fatalf("probe %s %v", bin, args)
				}
				if key, ok := env["AGENT_FEEDBACK_API_KEY"]; !ok || key != "" {
					t.Fatal("API credential not cleared")
				}
				return []byte(tt.version), nil
			}
			if got := e.OpenCodeMajor(); got != tt.major {
				t.Fatalf("major %d", got)
			}
			for _, name := range []string{"opencode", "opencode-v1"} {
				if got := e.Detect(name); got != tt.detected {
					t.Fatalf("%s detection %s", name, got)
				}
			}
			if tt.major == 0 {
				assertOpenCodeUnknownDiagnostic(t, e)
			}
			if tt.major == 2 {
				if _, err := e.Run(cliRequest("opencode-v1")); err != nil {
					t.Fatal(err)
				}
				statuses, err := e.Status()
				if err != nil {
					t.Fatal(err)
				}
				for _, st := range statuses {
					if st.Name == "opencode-v1" && (len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "uninstall opencode-v1")) {
						t.Fatalf("diagnosis %+v", st)
					}
				}
			}
		})
	}
	e := testEnv(t)
	put(t, filepath.Join(e.opencodeDir(), "opencode.json"), "{}")
	if got := e.Detect("opencode-v1"); got != "dir" {
		t.Fatalf("config-only detection %s", got)
	}
	e.LookPath = func(string) (string, error) { return "/stub/opencode", nil }
	e.Exec = func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		return nil, errors.New("failed")
	}
	if e.OpenCodeMajor() != 0 || e.Detect("opencode-v1") != "no" {
		t.Fatal("failed probe must not fall back to config")
	}
	assertOpenCodeUnknownDiagnostic(t, e)
}

func assertOpenCodeUnknownDiagnostic(t *testing.T, e Env) {
	t.Helper()
	statuses, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		if st.Name == "opencode-v1" {
			if st.Detected != "no" || !slices.Contains(st.Problems, OpenCodeUnknownVersion) {
				t.Fatalf("unknown version diagnosis: %+v", st)
			}
			return
		}
	}
	t.Fatal("missing OpenCode status")
}

func TestOpenCodeVersionCache(t *testing.T) {
	e := testEnv(t)
	e.LookPath = func(string) (string, error) { return "/stub/opencode", nil }
	probes := 0
	e.Exec = func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		probes++
		return []byte("warning: config ignored\n1.18.35\n"), nil
	}
	e = e.WithOpenCodeVersionCache()
	copy := e
	if e.OpenCodeMajor() != 1 || copy.Detect("opencode-v1") != "binary" {
		t.Fatal("cached version detection failed")
	}
	if _, err := copy.Status(); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("version probes: %d; want 1", probes)
	}
	e.Exec = func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		probes++
		return []byte("2.0.1\n"), nil
	}
	e = e.WithOpenCodeVersionCache()
	if e.OpenCodeMajor() != 2 || probes != 2 || copy.OpenCodeMajor() != 1 {
		t.Fatal("fresh cache must use replaced Exec without changing the original")
	}

	manual := testEnv(t)
	manual.LookPath = e.LookPath
	manual.Exec = e.Exec
	if manual.OpenCodeMajor() != 2 {
		t.Fatal("manual Env probe failed")
	}
	manual.Exec = func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		return []byte("1.18.35\n"), nil
	}
	if manual.OpenCodeMajor() != 1 {
		t.Fatal("manual Env must probe replaced Exec")
	}
}

func TestOpenCodeLegacyManifestUninstall(t *testing.T) {
	e := testEnv(t)
	if slices.Contains(Names(), "opencode") || !slices.Contains(Names(), "opencode-v1") || !Known("opencode") {
		t.Fatal(Names())
	}
	if _, err := e.Run(cliRequest("opencode")); err != nil {
		t.Fatal(err)
	}
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	m.Harnesses["opencode"] = m.Harnesses["opencode-v1"]
	delete(m.Harnesses, "opencode-v1")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	put(t, e.ManifestPath(), string(data))
	loaded, raw, err := LoadManifest(e.ManifestPath())
	if err != nil || loaded.Harnesses["opencode-v1"] == nil || loaded.Harnesses["opencode"] != nil || string(raw) != string(data) {
		t.Fatalf("migration %+v %v", loaded, err)
	}
	if _, err := e.Run(cliRequest("opencode")); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(e.ManifestPath())
	if err != nil || strings.Contains(string(persisted), `"opencode":`) || !strings.Contains(string(persisted), `"opencode-v1":`) {
		t.Fatalf("next install did not persist canonical record: %s %v", persisted, err)
	}
	// Exercise uninstall directly against the pre-migration manifest too.
	put(t, e.ManifestPath(), string(data))
	if _, err := e.Run(Request{Harnesses: []string{"opencode"}, Uninstall: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy plugin remains: %v", err)
	}
}

func TestOpenCodeManifestMergesOwnership(t *testing.T) {
	e := testEnv(t)
	put(t, e.ManifestPath(), `{"version":1,"harnesses":{"opencode":{"mode":"cli","items":[{"kind":"plugin_file","file":"old"}]},"opencode-v1":{"mode":"cli","items":[{"kind":"plugin_file","file":"new"}]}},"files":{}}`)
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Harnesses) != 1 || len(m.Harnesses["opencode-v1"].Items) != 2 {
		t.Fatalf("lost ownership: %+v", m.Harnesses)
	}
}
