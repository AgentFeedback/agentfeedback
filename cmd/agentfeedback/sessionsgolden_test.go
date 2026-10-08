package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
)

var updateSessionsGolden = flag.Bool("update-sessions-golden", false, "rewrite testdata/sessions/*.golden.json")

// goldenStores materialise one harness's fixture session store under a
// fresh home directory and return the working directory the sessions
// record; each harness registers its own from an init function.
var goldenStores = map[string]func(t *testing.T, home string) (cwd string){}

func registerGoldenStore(harness string, fn func(t *testing.T, home string) (cwd string)) {
	if _, ok := goldenStores[harness]; ok {
		panic("golden store for " + harness + " registered twice")
	}
	goldenStores[harness] = fn
}

// goldenWorld isolates the CLI with every harness store directory under
// one fresh home directory and returns that directory.
func goldenWorld(t *testing.T) string {
	t.Helper()
	isolateCLI(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	for _, name := range []string{"CODEX_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "OPENCODE_DB"} {
		t.Setenv(name, "")
	}

	return home
}

// encodeCwd is Claude Code's project directory name for cwd: every byte
// outside [A-Za-z0-9] becomes '-'.
func encodeCwd(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}

	return string(b)
}

// copyStore copies the fixture tree src under dst, filling {{SID}}, {{CWD}}
// and {{CWD_ENC}} in path segments and file contents with vars.
func copyStore(t *testing.T, src, dst string, vars map[string]string) {
	t.Helper()
	var pairs []string
	for k, v := range vars {
		pairs = append(pairs, k, v)
	}
	fill := strings.NewReplacer(pairs...)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, fill.Replace(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		return os.WriteFile(out, []byte(fill.Replace(string(b))), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSessionsGolden runs sessions list and sessions digest over every
// reader's fixture store and compares their JSON, with the home and the
// working directory written as <HOME>, <CWD> and <CWD_ENC>, with
// testdata/sessions/<harness>.{list,digest}.golden.json.
func TestSessionsGolden(t *testing.T) {
	for _, h := range sessions.Harnesses() {
		t.Run(h, func(t *testing.T) {
			materialise, ok := goldenStores[h]
			if !ok {
				t.Fatalf("%s has no golden store: every session reader needs a golden store fixture (registerGoldenStore)", h)
			}
			home := goldenWorld(t)
			cwd := materialise(t, home)
			// The working directory first, as written and in Claude Code's
			// project directory form: it may lie under the home.
			normalise := strings.NewReplacer(cwd, "<CWD>", encodeCwd(cwd), "<CWD_ENC>", home, "<HOME>")
			for _, run := range []struct {
				kind string
				args []string
			}{
				{"list", []string{"sessions", "list", "--json", "--harness", h}},
				{"digest", []string{"sessions", "digest", "--json", "--unprocessed", "--harness", h}},
			} {
				r := runCLI(t, "", run.args...)
				if r.code != 0 {
					t.Fatalf("%v: %+v", run.args, r)
				}
				var buf bytes.Buffer
				if err := json.Indent(&buf, []byte(r.stdout), "", "  "); err != nil {
					t.Fatalf("%v: %v: %q", run.args, err, r.stdout)
				}
				got := []byte(normalise.Replace(strings.TrimSpace(buf.String())) + "\n")
				golden := filepath.Join("testdata", "sessions", h+"."+run.kind+".golden.json")
				if *updateSessionsGolden {
					if err := os.WriteFile(golden, got, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%v differs from %s (rerun with -update-sessions-golden to accept)\n got:\n%s", run.args, golden, got)
				}
			}
		})
	}
}
