package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// isolateServer points HOME and the XDG directories at temporary ones and
// clears the server's key variables.
func isolateServer(t *testing.T) (home string) {
	t.Helper()
	isolate(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("API_KEY", "")
	t.Setenv("API_KEY_FILE", "")

	return home
}

// serveInit runs serve --init and parses its last stdout line.
func serveInit(t *testing.T, args ...string) (serveInitOutcome, result) {
	t.Helper()
	r := runCLI(t, "", append([]string{"serve", "--init"}, args...)...)
	var out serveInitOutcome
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("outcome %q: %v", r.stdout, err)
	}

	return out, r
}

func TestServeInit(t *testing.T) {
	isolateServer(t)
	out, r := serveInit(t, "--port", "18188")
	if r.code != 0 || out.Status != "written" {
		t.Fatalf("%+v", r)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(out.APIKey) {
		t.Fatalf("key %q", out.APIKey)
	}
	if strings.Count(r.stdout, out.APIKey) != 1 || strings.Contains(r.stderr, out.APIKey) {
		t.Fatalf("the key is printed more than once: %+v", r)
	}
	wantDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentfeedback", "server")
	wantDB := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agentfeedback", "agentfeedback.db")
	if out.Dir != wantDir || out.Database != wantDB || out.Listen != "127.0.0.1:18188" {
		t.Fatalf("outcome %+v", out)
	}
	for _, p := range []string{out.KeyFile, out.EnvFile} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, info, err)
		}
	}
	if key, _ := os.ReadFile(out.KeyFile); string(key) != out.APIKey+"\n" {
		t.Fatalf("key file %q", key)
	}
	env, _ := os.ReadFile(out.EnvFile)
	for _, line := range []string{"API_KEY_FILE=" + out.KeyFile, "DATABASE_PATH=" + wantDB, "HTTP_LISTEN_ADDR=127.0.0.1:18188"} {
		if !strings.Contains(string(env), "\n"+line+"\n") {
			t.Fatalf("serve.env lacks %q:\n%s", line, env)
		}
	}
	if strings.Contains(string(env), out.APIKey) {
		t.Fatal("serve.env holds the key")
	}

	// serve reads the key from the file serve.env names.
	t.Setenv("API_KEY_FILE", out.KeyFile)
	t.Setenv("DATABASE_PATH", wantDB)
	cfg, err := loadConfig(true)
	if err != nil || cfg.APIKey != out.APIKey {
		t.Fatalf("loadConfig: %v", err)
	}
	t.Setenv("API_KEY_FILE", "")

	// A second run without --force changes nothing.
	keyBefore, _ := os.ReadFile(out.KeyFile)
	again, r2 := serveInit(t)
	if r2.code != 1 || again.Status != "error" || !strings.Contains(again.Message, "--force") {
		t.Fatalf("second run %+v", r2)
	}
	keyAfter, _ := os.ReadFile(out.KeyFile)
	envAfter, _ := os.ReadFile(out.EnvFile)
	if !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(env, envAfter) {
		t.Fatal("a refused run changed the files")
	}

	// --force rotates the key.
	rotated, r3 := serveInit(t, "--force", "--port", "18189")
	keyRotated, _ := os.ReadFile(out.KeyFile)
	if r3.code != 0 || rotated.APIKey == out.APIKey || string(keyRotated) != rotated.APIKey+"\n" || rotated.Listen != "127.0.0.1:18189" {
		t.Fatalf("--force %+v", r3)
	}
}

func TestServeInit_Refusals(t *testing.T) {
	home := isolateServer(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--dir", filepath.Join(home, "a b")}, 1, `contains ' '`},
		{[]string{"--dir", filepath.Join(home, "a\tb")}, 1, `contains '\t'`},
		{[]string{"--dir", filepath.Join(home, "a\x01b")}, 1, `contains '\x01'`},
		{[]string{"--db", filepath.Join(home, "x$y.db")}, 1, `contains '$'`},
		{[]string{"--dir", filepath.Join(home, `q"`)}, 1, `contains '"'`},
		{[]string{"--dir", filepath.Join(home, "q'")}, 1, `contains '\''`},
		{[]string{"--dir", filepath.Join(home, `q\`)}, 1, `contains '\\'`},
		{[]string{"--dir", filepath.Join(home, "q`")}, 1, "contains '`'"},
		{[]string{"--dir", filepath.Join(home, "q%")}, 1, `contains '%'`},
		{[]string{"--dir", filepath.Join(home, "a;b")}, 1, `contains ';'`},
		{[]string{"--db", filepath.Join(home, "a:b.db")}, 1, `contains ':'`},
		{[]string{"--dir", filepath.Join(home, "a&b")}, 1, `contains '&'`},
		{[]string{"--dir", filepath.Join(home, "a|b")}, 1, `contains '|'`},
		{[]string{"--dir", filepath.Join(home, "a<b")}, 1, `contains '<'`},
		{[]string{"--dir", filepath.Join(home, "a(b")}, 1, `contains '('`},
		{[]string{"--dir", filepath.Join(home, "a~b")}, 1, `contains '~'`},
		{[]string{"--dir", filepath.Join(home, "a*b")}, 1, `contains '*'`},
		{[]string{"--dir", filepath.Join(home, "a?b")}, 1, `contains '?'`},
		{[]string{"--dir", filepath.Join(home, "a[b")}, 1, `contains '['`},
		{[]string{"--dir", filepath.Join(home, "a#b")}, 1, `contains '#'`},
		{[]string{"--dir", filepath.Join(home, "a!b")}, 1, `contains '!'`},
		{[]string{"--dir", filepath.Join(home, "a{b")}, 1, `contains '{'`},
		{[]string{"--db", "/agentfeedback.db"}, 2, "directly under the filesystem root"},
		{[]string{"--db", home}, 2, "is a directory"},
		{[]string{"--dir", filepath.Join(home, "s"), "--db", filepath.Join(home, "s", "serve.env")}, 2, "is one of the server files"},
		{[]string{"--dir", filepath.Join(home, "s"), "--db", filepath.Join(home, "s", "api-key")}, 2, "is one of the server files"},
		{[]string{"--dir", filepath.Join(home, "s"), "--db", filepath.Join(home, "s", "compose.yaml")}, 2, "is one of the server files"},
		{[]string{"--port", "0"}, 2, "is not a TCP port"},
		{[]string{"--port", "65536"}, 2, "is not a TCP port"},
		{[]string{"extra"}, 2, "wrong number of arguments"},
		{[]string{"--bogus"}, 2, "invalid arguments"},
	} {
		out, r := serveInit(t, tc.args...)
		if r.code != tc.code || out.Status != "error" || !strings.Contains(out.Message, tc.want) {
			t.Fatalf("%v: %+v", tc.args, r)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("a refused run wrote %v", entries)
	}
	for _, args := range [][]string{{"--port", "1"}, {"--force"}, {"--dir", "/x"}} {
		r := runCLI(t, "", append([]string{"serve"}, args...)...)
		if r.code != 2 || !strings.Contains(r.stderr, "only apply to --init") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestLoadConfig_APIKeyFile(t *testing.T) {
	isolateServer(t)
	dir := t.TempDir()
	t.Setenv("DATABASE_PATH", filepath.Join(dir, "a.db"))
	keyFile := filepath.Join(dir, "api-key")

	writeFile(t, keyFile, "  k-123\n", 0o600)
	t.Setenv("API_KEY_FILE", keyFile)
	if cfg, err := loadConfig(true); err != nil || cfg.APIKey != "k-123" {
		t.Fatalf("loadConfig: %+v %v", cfg, err)
	}
	// backup and import ignore keys.
	if _, err := loadConfig(false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("API_KEY", "other")
	if _, err := loadConfig(true); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("both: %v", err)
	}
	t.Setenv("API_KEY", "")

	for content, want := range map[string]string{
		"\n\n":            "is empty",
		"a b\n":           "whitespace or control characters",
		"secret-\x01-key": "whitespace or control characters",
	} {
		writeFile(t, keyFile, content, 0o600)
		_, err := loadConfig(true)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), keyFile) ||
			strings.Contains(err.Error(), "secret") {
			t.Fatalf("%q: %v", content, err)
		}
	}
	t.Setenv("API_KEY_FILE", filepath.Join(dir, "missing"))
	if _, err := loadConfig(true); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("missing: %v", err)
	}
}

func TestServeInit_AcceptedCharacters(t *testing.T) {
	home := isolateServer(t)
	dir := filepath.Join(home, "Ab9_.-+@,=", "ünï")
	if out, r := serveInit(t, "--dir", dir); r.code != 0 || out.Dir != dir {
		t.Fatalf("%+v", r)
	}
}

func TestServeInit_Windows(t *testing.T) {
	home := isolateServer(t)
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })
	out, r := serveInit(t)
	if r.code != 2 || !strings.Contains(out.Message, "not supported on Windows") || !strings.Contains(out.Message, "API_KEY_FILE") {
		t.Fatalf("%+v", r)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("a refused run wrote %v", entries)
	}
}

func TestServeInit_ForceKeyWriteFails(t *testing.T) {
	isolateServer(t)
	first, r := serveInit(t)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	envBefore, _ := os.ReadFile(first.EnvFile)
	// Make the key path a non-empty directory holding the old key, so the
	// rename over it fails. serve.env is written first and replaced with
	// content equal to the old one (same dir, database and port); what sits
	// at the key path, the old key with it, must be untouched.
	if err := os.Remove(first.KeyFile); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(first.KeyFile, "old"), first.APIKey, 0o600)

	out, r := serveInit(t, "--force")
	if r.code != 1 || out.Status != "error" || !strings.Contains(out.Message, first.KeyFile) {
		t.Fatalf("%+v", r)
	}
	if envAfter, _ := os.ReadFile(first.EnvFile); string(envAfter) != string(envBefore) {
		t.Fatalf("serve.env changed:\n%s", envAfter)
	}
	if old, err := os.ReadFile(filepath.Join(first.KeyFile, "old")); err != nil || string(old) != first.APIKey {
		t.Fatalf("the old key is lost: %v", err)
	}
}

func TestServeInit_KeyWriteFailsLeavesNothing(t *testing.T) {
	home := isolateServer(t)
	dir := filepath.Join(home, "s")
	// Without force the key path is checked first, so no file can make the
	// key write fail afterwards; the failure is injected instead.
	orig := placeFileHook
	placeFileHook = func(path string) error {
		if filepath.Base(path) == keyFileName {
			return os.ErrPermission
		}

		return nil
	}
	t.Cleanup(func() { placeFileHook = orig })
	out, r := serveInit(t, "--dir", dir)
	if r.code != 1 || out.Status != "error" {
		t.Fatalf("%+v", r)
	}
	for _, name := range []string{keyFileName, serveEnvName} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was left behind: %v", name, err)
		}
	}
}
