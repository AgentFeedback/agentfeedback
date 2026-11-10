package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
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

// goldenSkillSessions add to one harness's materialised store the sessions
// that use skills, from testdata/skills of internal/sessions; they are kept
// apart from the stores, which other tests read whole. Every reader with a
// semantic digest registers one.
var goldenSkillSessions = map[string]func(t *testing.T, home, cwd string){}

func registerGoldenSkillSessions(harness string, fn func(t *testing.T, home, cwd string)) {
	if _, ok := goldenSkillSessions[harness]; ok {
		panic("golden skill sessions for " + harness + " registered twice")
	}
	goldenSkillSessions[harness] = fn
}

// skillsFixtures is internal/sessions' tree of skill-using sessions.
var skillsFixtures = filepath.Join("..", "..", "internal", "sessions", "testdata", "skills")

// writeSkill writes a SKILL.md named name under root/dir.
func writeSkill(t *testing.T, root, dir, name string) {
	t.Helper()
	p := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nname: "+name+"\ndescription: The "+name+" fixture skill.\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goldenSkills installs the golden world's skills: golden-review and
// golden-lint user-level (~/.agents/skills, read by Codex), and
// golden-build in the project at cwd when it exists (.claude/skills).
func goldenSkills(t *testing.T, home, cwd string) {
	t.Helper()
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "golden-review", "golden-review")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "golden-lint", "golden-lint")
	if info, err := os.Stat(cwd); err == nil && info.IsDir() {
		writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "golden-build", "golden-build")
	}
}

// execGoldenSQL executes the statements of file, its placeholders filled
// from vars, in the SQLite database at db.
func execGoldenSQL(t *testing.T, db, file string, vars map[string]string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for k, v := range vars {
		pairs = append(pairs, k, v)
	}
	conn, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(db)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(strings.NewReplacer(pairs...).Replace(string(b))); err != nil {
		t.Fatal(err)
	}
}

// goldenWorld isolates the CLI with every harness store directory under
// one fresh home directory and returns that directory.
func goldenWorld(t *testing.T) string {
	t.Helper()
	isolateCLI(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
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

// TestSessionsGolden compares each reader's list and digest (or storage-only
// capture preview) with fixtures, normalising home and working directory paths.
func TestSessionsGolden(t *testing.T) {
	for _, h := range sessions.Harnesses() {
		t.Run(h, func(t *testing.T) {
			materialise, ok := goldenStores[h]
			if !ok {
				t.Fatalf("%s has no golden store: every session reader needs a golden store fixture (registerGoldenStore)", h)
			}
			home := goldenWorld(t)
			cwd := materialise(t, home)
			if add, ok := goldenSkillSessions[h]; ok {
				goldenSkills(t, home, cwd)
				add(t, home, cwd)
			} else if h != "cursor" {
				t.Fatalf("%s has no golden skill sessions (registerGoldenSkillSessions)", h)
			}
			// The working directory first, as written and in Claude Code's
			// project directory form: it may lie under the home.
			normalise := strings.NewReplacer(cwd, "<CWD>", encodeCwd(cwd), "<CWD_ENC>", home, "<HOME>")
			if h == "cursor" {
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
						t.Errorf("dry-run attempted unexpected request: %s", r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				t.Setenv(envURL, srv.URL)
				t.Setenv(envAPIKey, testKey)
			}
			for _, run := range []struct {
				kind string
				args []string
			}{
				{"list", []string{"sessions", "list", "--json", "--harness", h}},
				{"digest", []string{"sessions", "digest", "--json", "--unprocessed", "--harness", h}},
			} {
				if h == "cursor" && run.kind == "digest" {
					run.kind = "dry-run"
					run.args = []string{"sessions", "push", "cursor:cli", "cursor:ide", "--since", "2020-01-01T00:00:00Z", "--dry-run", "--json"}
				}
				r := runCLI(t, "", run.args...)
				if r.code != 0 {
					t.Fatalf("%v: %+v", run.args, r)
				}
				if run.kind == "dry-run" {
					var rows []map[string]any
					if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if _, ok := row["bytes"]; ok {
							row["bytes"] = "<ENVELOPE_BYTES>"
						}
					}
					encoded, err := json.Marshal(rows)
					if err != nil {
						t.Fatal(err)
					}
					r.stdout = string(encoded)
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
