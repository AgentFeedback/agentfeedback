package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

const testKey = "test-key-3f9a1c"

// isolate clears every variable the client reads and points the config and
// cache directories at fresh temporary ones, so the real environment never
// leaks into a test. It returns the config file path and the cache dir.
func isolate(t *testing.T) (cfgPath, cache string) {
	t.Helper()
	for _, name := range []string{envURL, envAPIKey, envMachine, envModel, envHarness} {
		t.Setenv(name, "")
	}
	cfgHome, cacheHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	return filepath.Join(cfgHome, "agentfeedback", "config.toml"), filepath.Join(cacheHome, "agentfeedback")
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut)

	return result{code, out.String(), errOut.String()}
}

func TestRun_HelpAndRouting(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		r := runCLI(t, "", args...)
		if r.code != 0 || !strings.Contains(r.stdout, "doctor") || !strings.Contains(r.stdout, envAPIKey) ||
			!strings.Contains(r.stdout, "config.toml") || !strings.Contains(r.stdout, "DATABASE_PATH") {
			t.Fatalf("%v: code %d, stdout %q", args, r.code, r.stdout)
		}
	}
	for _, args := range [][]string{{"--json"}, {"help", "--json"}} {
		r := runCLI(t, "", args...)
		var help struct {
			Commands []struct{ Name, Summary string } `json:"commands"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &help) != nil || len(help.Commands) != len(commands) {
			t.Fatalf("%v: code %d, stdout %q", args, r.code, r.stdout)
		}
	}

	r := runCLI(t, "", "nope")
	if r.code != 2 || !strings.Contains(r.stderr, `unknown command "nope"`) {
		t.Fatalf("unknown command: %+v", r)
	}
	if r := runCLI(t, "", "version", "--bogus"); r.code != 2 {
		t.Fatalf("bad flag: %+v", r)
	}
	if r := runCLI(t, "", "backup"); r.code != 2 {
		t.Fatalf("backup without dest: %+v", r)
	}
	if r := runCLI(t, "", "import", "a", "b"); r.code != 2 {
		t.Fatalf("import with two files: %+v", r)
	}
}

func TestRun_ServeWithoutAPIKeyFails(t *testing.T) {
	isolate(t)
	t.Setenv("API_KEY", "")
	r := runCLI(t, "", "serve")
	if r.code != 1 || !strings.Contains(r.stderr, "agentfeedback serve: API_KEY is not set; export it") {
		t.Fatalf("%+v", r)
	}
}

func TestResolveClient_Precedence(t *testing.T) {
	file := fileConfig{URL: "cfg-url", APIKey: "cfg-key", Machine: "cfg-m", Model: "cfg-model", Harness: "cfg-h"}
	env := map[string]string{
		envURL: "env-url", envAPIKey: "env-key", envMachine: "env-m", envModel: "env-model", envHarness: "env-h",
	}
	flags := flagConfig{URL: "flag-url", Machine: "flag-m", Model: "flag-model", Harness: "flag-h"}
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	tests := []struct {
		name  string
		flags flagConfig
		env   map[string]string
		file  fileConfig
		want  clientSettings
	}{
		{"flag beats env and config", flags, env, file, clientSettings{
			URL: resolvedValue{"flag-url", sourceFlag}, APIKey: resolvedValue{"env-key", sourceEnv},
			Machine: resolvedValue{"flag-m", sourceFlag}, Model: resolvedValue{"flag-model", sourceFlag},
			Harness: resolvedValue{"flag-h", sourceFlag},
		}},
		{"env beats config", flagConfig{}, env, file, clientSettings{
			URL: resolvedValue{"env-url", sourceEnv}, APIKey: resolvedValue{"env-key", sourceEnv},
			Machine: resolvedValue{"env-m", sourceEnv}, Model: resolvedValue{"env-model", sourceEnv},
			Harness: resolvedValue{"env-h", sourceEnv},
		}},
		{"empty env is unset", flagConfig{}, map[string]string{envURL: "", envAPIKey: ""}, file, clientSettings{
			URL: resolvedValue{"cfg-url", sourceConfig}, APIKey: resolvedValue{"cfg-key", sourceConfig},
			Machine: resolvedValue{"cfg-m", sourceConfig}, Model: resolvedValue{"cfg-model", sourceConfig},
			Harness: resolvedValue{"cfg-h", sourceConfig},
		}},
		{"flags alone, key has no flag", flags, nil, fileConfig{}, clientSettings{
			URL: resolvedValue{"flag-url", sourceFlag}, Machine: resolvedValue{"flag-m", sourceFlag},
			Model: resolvedValue{"flag-model", sourceFlag}, Harness: resolvedValue{"flag-h", sourceFlag},
		}},
		{"nothing set", flagConfig{}, nil, fileConfig{}, clientSettings{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveClient(tt.flags, getenv(tt.env), tt.file); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadFileConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, exists, err := loadFileConfig(path); exists || err != nil {
		t.Fatalf("missing file: exists %v, err %v", exists, err)
	}

	writeFile(t, path, "url = \"https://x\"\nfuture = 1\n[collect]\nx = true\n", 0o600)
	cfg, exists, err := loadFileConfig(path)
	if err != nil || !exists || cfg.URL != "https://x" {
		t.Fatalf("unknown keys: %+v %v %v", cfg, exists, err)
	}

	for _, body := range []string{"url = ", "url = 5\n"} {
		writeFile(t, path, body, 0o600)
		_, _, err := loadFileConfig(path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "doctor --init --force") {
			t.Fatalf("%q: %v", body, err)
		}
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func metaServer(t *testing.T, status int, minVersion string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meta" || r.Header.Get("Authorization") != "Bearer "+testKey ||
			r.Header.Get("Accept") != "application/json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "agentfeedback/") {
			http.Error(w, "unexpected request", http.StatusTeapot)

			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"service_version":"v4.0.0","api_version":"1.0","client":{"min_version":"` +
			minVersion + `","latest_known":"v4.0.0"}}`))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func doctorJSON(t *testing.T, args ...string) (doctorReport, result) {
	t.Helper()
	r := runCLI(t, "", append([]string{"doctor", "--json"}, args...)...)
	var rep doctorReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%+v", err, r)
	}
	if strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatalf("doctor printed the key: %+v", r)
	}

	return rep, r
}

func hasProblem(rep doctorReport, part string) bool {
	return slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, part) })
}

func TestDoctor_OK(t *testing.T) {
	isolate(t)
	srv := metaServer(t, http.StatusOK, "v0.0.1")
	t.Setenv(envAPIKey, testKey)
	rep, r := doctorJSON(t, "--url", srv.URL+"/")
	if r.code != 0 || rep.Status != "ok" || len(rep.Problems) != 0 {
		t.Fatalf("%+v", r)
	}
	if !rep.Meta.OK || rep.Meta.HTTPStatus != 200 || rep.Meta.ServiceVersion != "v4.0.0" || rep.Meta.APIVersion != "1.0" ||
		rep.Meta.ClientMinVersion != "v0.0.1" || rep.Meta.ClientLatestKnown != "v4.0.0" {
		t.Fatalf("meta %+v", rep.Meta)
	}
	if rep.URL != (resolvedValue{srv.URL + "/", sourceFlag}) || rep.APIKey != (keyCheck{true, sourceEnv}) || rep.Config.Exists {
		t.Fatalf("settings %+v %+v %+v", rep.URL, rep.APIKey, rep.Config)
	}

	human := runCLI(t, "", "doctor", "--url", srv.URL)
	if human.code != 0 || strings.Contains(human.stdout, testKey) || !strings.Contains(human.stdout, "status:   ok") {
		t.Fatalf("human: %+v", human)
	}
}

func TestDoctor_Problems(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	deadURL := unreachable.URL
	unreachable.Close()

	tests := []struct {
		name    string
		status  int
		min     string
		key     string
		noURL   bool
		dead    bool
		problem string
	}{
		{name: "401", status: 401, key: testKey, problem: "rejected the API key"},
		{name: "500", status: 500, key: testKey, problem: "check the URL points at an AgentFeedback v1 server"},
		{name: "unreachable", dead: true, key: testKey, problem: "check the server is running and reachable"},
		{name: "missing url", noURL: true, key: testKey, problem: "no server URL is set"},
		{name: "missing key", status: 200, problem: "no API key is set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv(envAPIKey, tt.key)
			var args []string
			switch {
			case tt.dead:
				args = []string{"--url", deadURL}
			case !tt.noURL:
				args = []string{"--url", metaServer(t, tt.status, "v0.0.1").URL}
			}
			rep, r := doctorJSON(t, args...)
			if r.code != 1 || rep.Status != "error" || !hasProblem(rep, tt.problem) {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestDoctor_ClientTooOld(t *testing.T) {
	isolate(t)
	t.Setenv(envAPIKey, testKey)
	srv := metaServer(t, http.StatusOK, "v99.0.0")
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })

	rep, r := doctorJSON(t, "--url", srv.URL)
	if r.code != 1 || rep.Versions.Compare != "too_old" || !hasProblem(rep, "upgrade the agentfeedback binary") {
		t.Fatalf("%+v", r)
	}

	version = "dev"
	rep, r = doctorJSON(t, "--url", srv.URL)
	if r.code != 0 || rep.Versions.Compare != "unknown" {
		t.Fatalf("dev build: %+v", r)
	}
}

func TestDoctor_SpoolLogAndMode(t *testing.T) {
	cfgPath, cache := isolate(t)
	writeFile(t, cfgPath, "url = \"http://127.0.0.1:1\"\n", 0o644)
	for _, name := range []string{
		"spool/a.json", "spool/b.json", "spool/.tmp-c.json", "spool/d.json.rejected", "rejected/e.json", "rejected/f.json",
	} {
		writeFile(t, filepath.Join(cache, name), "{}", 0o600)
	}
	var log strings.Builder
	for i := range 7 {
		log.WriteString(`{"n":` + string(rune('0'+i)) + "}\n")
	}
	log.WriteString("not json\n")
	writeFile(t, filepath.Join(cache, "log", "client.jsonl"), log.String(), 0o600)

	rep, r := doctorJSON(t)
	if r.code != 1 || rep.Config.Mode != "0644" || !hasProblem(rep, "chmod 600 "+cfgPath) {
		t.Fatalf("mode: %+v", r)
	}
	if rep.URL.Source != sourceConfig || rep.Meta.Checked {
		t.Fatalf("url from config, meta skipped without a key: %+v %+v", rep.URL, rep.Meta)
	}
	if rep.Spool != (spoolCheck{Pending: 2, Rejected: 3}) {
		t.Fatalf("spool %+v", rep.Spool)
	}
	if rep.Recent.Invalid != 1 || len(rep.Recent.Lines) != 4 || string(rep.Recent.Lines[3]) != `{"n":6}` {
		t.Fatalf("recent %+v", rep.Recent)
	}
	if _, err := os.Stat(filepath.Join(cache, "log")); err != nil {
		t.Fatal(err)
	}
}

func TestDoctor_CreatesNothing(t *testing.T) {
	cfgPath, cache := isolate(t)
	if r := runCLI(t, "", "doctor"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
	for _, p := range []string{filepath.Dir(cfgPath), cache} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s was created", p)
		}
	}
}

func lastJSONLine(t *testing.T, stdout string) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	var out map[string]string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		t.Fatalf("last line is not a JSON outcome: %q", stdout)
	}

	return out
}

func TestDoctorInit(t *testing.T) {
	cfgPath, _ := isolate(t)
	r := runCLI(t, "  "+testKey+"\n", "doctor", "--init", "--url", "https://feedback.example", "--key-from-stdin")
	if out := lastJSONLine(t, r.stdout); r.code != 0 || out["status"] != "written" || out["path"] != cfgPath {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatal("init printed the key")
	}
	info, err := os.Stat(cfgPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	file, _, err := loadFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := resolveClient(flagConfig{}, os.Getenv, file)
	if got.URL != (resolvedValue{"https://feedback.example", sourceConfig}) || got.APIKey != (resolvedValue{testKey, sourceConfig}) {
		t.Fatalf("round trip %+v", got)
	}
	before, _ := os.ReadFile(cfgPath)

	r = runCLI(t, "other-key", "doctor", "--init", "--url", "https://other.example", "--key-from-stdin")
	out := lastJSONLine(t, r.stdout)
	if r.code != 1 || out["status"] != "error" || !strings.Contains(out["message"], "pass --force to replace it") {
		t.Fatalf("existing: %+v", r)
	}
	if after, _ := os.ReadFile(cfgPath); !bytes.Equal(before, after) {
		t.Fatal("existing config changed without --force")
	}

	r = runCLI(t, "other-key", "doctor", "--init", "--url", "https://other.example", "--key-from-stdin", "--force", "--json")
	if out := lastJSONLine(t, r.stdout); r.code != 0 || out["status"] != "written" {
		t.Fatalf("force: %+v", r)
	}
	file, _, _ = loadFileConfig(cfgPath)
	if file.URL != "https://other.example" || file.APIKey != "other-key" {
		t.Fatalf("force did not replace: %+v", file)
	}
	if info, _ := os.Stat(cfgPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after force %v", info.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(cfgPath))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestDoctorInit_Refusals(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
		code  int
		msg   string
	}{
		{"no key-from-stdin", testKey, []string{"--url", "https://x.example"}, 2, "pass --key-from-stdin"},
		{"no url", testKey, []string{"--key-from-stdin"}, 2, "needs the server URL"},
		{"ftp url", testKey, []string{"--url", "ftp://x.example", "--key-from-stdin"}, 2, "not usable"},
		{"no host", testKey, []string{"--url", "https://", "--key-from-stdin"}, 2, "not usable"},
		{"userinfo", testKey, []string{"--url", "https://u:p@x.example", "--key-from-stdin"}, 2, "not usable"},
		{"query", testKey, []string{"--url", "https://x.example/?a=1", "--key-from-stdin"}, 2, "not usable"},
		{"fragment", testKey, []string{"--url", "https://x.example/#f", "--key-from-stdin"}, 2, "not usable"},
		{"relative", testKey, []string{"--url", "x.example", "--key-from-stdin"}, 2, "not usable"},
		{"empty key", " \n", []string{"--url", "https://x.example", "--key-from-stdin"}, 1, "is empty"},
		{"inner space", "a b", []string{"--url", "https://x.example", "--key-from-stdin"}, 1, "whitespace or control"},
		{"control", "a\x01b", []string{"--url", "https://x.example", "--key-from-stdin"}, 1, "whitespace or control"},
		{"bad flag", testKey, []string{"--bogus"}, 2, "invalid arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath, _ := isolate(t)
			r := runCLI(t, tt.stdin, append([]string{"doctor", "--init"}, tt.args...)...)
			out := lastJSONLine(t, r.stdout)
			if r.code != tt.code || out["status"] != "error" || !strings.Contains(out["message"], tt.msg) {
				t.Fatalf("%+v", r)
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Fatal("config written on a refusal")
			}
		})
	}
}

func TestVersionFromBuildInfo(t *testing.T) {
	setting := func(k, v string) debug.BuildSetting { return debug.BuildSetting{Key: k, Value: v} }
	tests := []struct {
		name          string
		info          *debug.BuildInfo
		ver, revision string
	}{
		{"nil", nil, "dev", "unknown"},
		{"devel", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev", "unknown"},
		{"module version", &debug.BuildInfo{Main: debug.Module{Version: "v4.1.0"},
			Settings: []debug.BuildSetting{setting("vcs.revision", "abc"), setting("vcs.modified", "false")}}, "v4.1.0", "abc"},
		{"dirty", &debug.BuildInfo{Settings: []debug.BuildSetting{setting("vcs.revision", "abc"), setting("vcs.modified", "true")}},
			"dev", "abc-dirty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v, r := versionFromBuildInfo(tt.info); v != tt.ver || r != tt.revision {
				t.Fatalf("got %s %s", v, r)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	isolate(t)
	old, oldCommit := version, commit
	version, commit = "v4.0.0", "deadbeef"
	t.Cleanup(func() { version, commit = old, oldCommit })

	r := runCLI(t, "", "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "agentfeedback v4.0.0 (commit deadbeef, go") {
		t.Fatalf("%+v", r)
	}
	r = runCLI(t, "", "version", "--json")
	var v buildVersion
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || v.Version != "v4.0.0" || v.Commit != "deadbeef" || v.Go == "" {
		t.Fatalf("%+v", r)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.2.3", "1.2.3", 0, true},
		{"v1.2.3", "v1.10.0", -1, true},
		{"v2.0.0-rc.1", "v1.9.9+build", 1, true},
		{"dev", "v1.0.0", 0, false},
		{"v1.2", "v1.2.0", 0, false},
		{"v1.0.2-0.20260929105946-6e0e23d761c5+dirty", "v4.0.0", 0, false},
		{"v0.0.0-20260929105946-6e0e23d761c5", "v4.0.0", 0, false},
		{"v4.0.1-rc.1.0.20260929105946-6e0e23d761c5", "v4.0.0", 0, false},
		{"v4.0.0+dirty", "v4.0.0", 0, true},
		{"v4.0.0-rc.1", "v4.0.0", -1, true},
		{"v4.0.0", "v4.0.0-rc.1", 1, true},
		{"v4.0.0-rc.1", "v4.0.0-rc.2", -1, true},
		{"v4.0.0-rc.2", "v4.0.0-rc.10", -1, true},
		{"1.0.0-alpha", "1.0.0-beta", -1, true},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1, true},
		{"1.0.0-rc.1+b1", "1.0.0-rc.1+b2", 0, true},
	}
	for _, tt := range tests {
		if cmp, ok := compareVersions(tt.a, tt.b); cmp != tt.cmp || ok != tt.ok {
			t.Fatalf("%s vs %s: %d %v", tt.a, tt.b, cmp, ok)
		}
	}
}

func TestSchemaCommand(t *testing.T) {
	isolate(t)
	want, ok := schema.Document("friction", 1)
	if !ok {
		t.Fatal("friction 1 not shipped")
	}
	for _, args := range [][]string{{"schema", "friction", "1"}, {"schema", " Friction "}} {
		r := runCLI(t, "", args...)
		if r.code != 0 || strings.TrimSuffix(r.stdout, "\n") != strings.TrimSuffix(string(want), "\n") ||
			len(r.stdout) > len(want)+1 {
			t.Fatalf("%v: code %d, %d bytes", args, r.code, len(r.stdout))
		}
	}

	r := runCLI(t, "", "schema")
	if r.code != 0 || !strings.Contains(r.stdout, "friction") || !strings.Contains(r.stdout, "envelope") {
		t.Fatalf("list: %+v", r)
	}
	r = runCLI(t, "", "schema", "--json")
	var entries []schema.Entry
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &entries) != nil || len(entries) != len(schema.List()) {
		t.Fatalf("list json: %+v", r)
	}

	for _, args := range [][]string{{"schema", "nope"}, {"schema", "friction", "9"}, {"schema", "friction", "0"}, {"schema", "friction", "x"}} {
		r := runCLI(t, "", args...)
		if r.code != 2 || !strings.Contains(r.stderr, "run agentfeedback schema to list them") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestDoctor_URLCredentialsNeverShown(t *testing.T) {
	isolate(t)
	t.Setenv(envAPIKey, testKey)
	for _, args := range [][]string{{"doctor", "--json", "--url", "http://user:s3cret@127.0.0.1:1"}, {"doctor", "--url", "http://user:s3cret@127.0.0.1:1"}, {"doctor", "--url", "http://s3cret@127.0.0.1:1"}} {
		r := runCLI(t, "", args...)
		if r.code != 1 || strings.Contains(r.stdout+r.stderr, "s3cret") || !strings.Contains(r.stdout, "carries credentials") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	rep, _ := doctorJSON(t, "--url", "http://user:s3cret@127.0.0.1:1")
	if !strings.Contains(rep.URL.Value, "//REDACTED@") {
		t.Fatalf("url %q", rep.URL.Value)
	}
	for _, p := range rep.Problems {
		if strings.Count(p, "http://") > 1 {
			t.Fatalf("URL repeated: %q", p)
		}
	}
}

func TestDoctor_RedirectAndBadMeta(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		problem string
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://u:s3cret@other.example/api/v1/meta", http.StatusMovedPermanently)
		}, "redirected (HTTP 301) to https://REDACTED@other.example/api/v1/meta; set the server URL"},
		{"null", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("null")) }, "not AgentFeedback metadata"},
		{"empty object", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }, "not AgentFeedback metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv(envAPIKey, testKey)
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			rep, r := doctorJSON(t, "--url", srv.URL)
			if r.code != 1 || rep.Meta.OK || !hasProblem(rep, tt.problem) || strings.Contains(r.stdout, "s3cret") {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestDoctor_UnreadableSpoolAndLog(t *testing.T) {
	_, cache := isolate(t)
	writeFile(t, filepath.Join(cache, "spool"), "not a dir", 0o600)
	if err := os.MkdirAll(filepath.Join(cache, "log", "client.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	rep, r := doctorJSON(t, "--url", "http://127.0.0.1:1")
	if r.code != 1 || !hasProblem(rep, "cannot read "+filepath.Join(cache, "spool")) ||
		!hasProblem(rep, "cannot read "+filepath.Join(cache, "log", "client.jsonl")) || !hasProblem(rep, "check its permissions") {
		t.Fatalf("%+v", rep.Problems)
	}
}

func TestDoctor_LogLinesWithKeyAreDropped(t *testing.T) {
	_, cache := isolate(t)
	t.Setenv(envAPIKey, testKey)
	writeFile(t, filepath.Join(cache, "log", "client.jsonl"), `{"ok":1}`+"\n"+`{"key":"`+testKey+`"}`+"\n", 0o600)
	rep, r := doctorJSON(t, "--url", "http://127.0.0.1:1")
	if rep.Recent.Redacted != 1 || len(rep.Recent.Lines) != 1 {
		t.Fatalf("%+v", rep.Recent)
	}
	human := runCLI(t, "", "doctor", "--url", "http://127.0.0.1:1")
	if strings.Contains(r.stdout+human.stdout+human.stderr, testKey) {
		t.Fatal("key printed")
	}
}

func TestDoctor_LogTailWindow(t *testing.T) {
	_, cache := isolate(t)
	pad := strings.Repeat("x", 20<<10)
	var log strings.Builder
	for i := range 10 {
		log.WriteString(`{"n":` + strconv.Itoa(i) + `,"pad":"` + pad + `"}` + "\n")
	}
	if log.Len() <= logTailBytes {
		t.Fatal("log not larger than the window")
	}
	writeFile(t, filepath.Join(cache, "log", "client.jsonl"), log.String(), 0o600)
	rep, _ := doctorJSON(t)
	// The 64 KiB window holds lines 7-9 whole and cuts line 6, which is dropped.
	if rep.Recent.Invalid != 0 || len(rep.Recent.Lines) != 3 {
		t.Fatalf("invalid %d, lines %d", rep.Recent.Invalid, len(rep.Recent.Lines))
	}
	for i, line := range rep.Recent.Lines {
		var v struct{ N int }
		if json.Unmarshal(line, &v) != nil || v.N != 7+i {
			t.Fatalf("line %d: %.40s", i, line)
		}
	}

	log.Reset()
	for i := range 2000 {
		log.WriteString(`{"n":` + strconv.Itoa(i) + `,"pad":"` + strings.Repeat("y", 40) + `"}` + "\n")
	}
	writeFile(t, filepath.Join(cache, "log", "client.jsonl"), log.String(), 0o600)
	rep, _ = doctorJSON(t)
	if rep.Recent.Invalid != 0 || len(rep.Recent.Lines) != 5 {
		t.Fatalf("invalid %d, lines %d", rep.Recent.Invalid, len(rep.Recent.Lines))
	}
	for i, line := range rep.Recent.Lines {
		var v struct{ N int }
		if json.Unmarshal(line, &v) != nil || v.N != 1995+i {
			t.Fatalf("line %d: %s", i, line)
		}
	}
}

func TestRun_LeadingFlagAndServeArgs(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--json", "doctor"}, {"-x"}, {"--url", "http://x"}} {
		r := runCLI(t, "", args...)
		if r.code != 2 || !strings.Contains(r.stderr, "run agentfeedback <command> --json") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if r := runCLI(t, "", "--json"); r.code != 0 || !strings.HasPrefix(r.stdout, `{"commands":`) {
		t.Fatalf("bare --json: %+v", r)
	}

	r := runCLI(t, "", "serve", "-h")
	if r.code != 0 || !strings.Contains(r.stderr, "agentfeedback serve") || r.stdout != "" {
		t.Fatalf("serve -h: %+v", r)
	}
	for _, args := range [][]string{{"serve", "extra"}, {"serve", "--port", "1"}} {
		if r := runCLI(t, "", args...); r.code != 2 || !strings.Contains(r.stderr, "agentfeedback serve: ") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestRun_FlagErrorPrintedOnce(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"version", "schema", "doctor", "import", "serve"} {
		r := runCLI(t, "", cmd, "--bogus")
		lines := strings.Split(strings.TrimRight(r.stderr, "\n"), "\n")
		if r.code != 2 || len(lines) != 1 || !strings.HasPrefix(lines[0], "agentfeedback "+cmd+": ") {
			t.Fatalf("%s: %+v", cmd, r)
		}
	}
	for _, args := range [][]string{{"--init", "--bogus"}, {"--init=true", "--bogus"}, {"-init", "--bogus"}} {
		r := runCLI(t, "", append([]string{"doctor"}, args...)...)
		lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
		if r.code != 2 || len(lines) != 1 || r.stderr != "" {
			t.Fatalf("%v: %+v", args, r)
		}
		if out := lastJSONLine(t, r.stdout); out["status"] != "error" {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}
