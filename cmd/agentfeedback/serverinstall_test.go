package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installSetup runs serve --init in an isolated home and stands in a binary
// path; it returns the serve --init outcome and the binary path.
func installSetup(t *testing.T) (serveInitOutcome, string) {
	t.Helper()
	isolateServer(t)
	out, r := serveInit(t, "--port", "18190")
	if r.code != 0 {
		t.Fatalf("serve --init: %+v", r)
	}
	bin := filepath.Join(t.TempDir(), "agentfeedback")
	writeFile(t, bin, "", 0o755)
	bin, _ = filepath.EvalSymlinks(bin)
	orig := executable
	executable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executable = orig })

	return out, bin
}

func serverInstall(t *testing.T, args ...string) (serverOutcome, result) {
	t.Helper()
	r := runCLI(t, "", append([]string{"server", "install"}, args...)...)
	var out serverOutcome
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("outcome %q: %v", r.stdout, err)
	}

	return out, r
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	orig := version
	version = v
	t.Cleanup(func() { version = orig })
}

func TestServerInstall(t *testing.T) {
	si, bin := installSetup(t)
	setVersion(t, "v4.1.0")
	home := os.Getenv("HOME")
	cases := []struct {
		mode, path string
		want       []string
		next       []string
	}{
		{"systemd", filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "agentfeedback.service"), []string{
			"\nEnvironmentFile=" + si.EnvFile + "\n",
			"\nExecStart=" + bin + " serve\n",
			"\nRestart=on-failure\n",
			"\nWantedBy=default.target\n",
		}, []string{"systemctl --user enable --now agentfeedback.service", "systemctl --user status agentfeedback.service", " backup ", "systemctl --user restart agentfeedback.service"}},
		{"launchd", filepath.Join(home, "Library", "LaunchAgents", "dev.agentfeedback.serve.plist"), []string{
			"<string>" + bin + "</string>\n\t\t<string>serve</string>",
			"<key>API_KEY_FILE</key>\n\t\t<string>" + si.KeyFile + "</string>",
			"<key>DATABASE_PATH</key>\n\t\t<string>" + si.Database + "</string>",
			"<key>HTTP_LISTEN_ADDR</key>\n\t\t<string>127.0.0.1:18190</string>",
			"<string>" + filepath.Join(home, "Library", "Logs", "agentfeedback", "serve.log") + "</string>",
		}, []string{"launchctl bootstrap gui/$(id -u) ", "launchctl kickstart -k gui/$(id -u)/dev.agentfeedback.serve", "launchctl print", " backup ", "launchctl bootout gui/$(id -u)/dev.agentfeedback.serve && launchctl bootstrap"}},
		{"compose", filepath.Join(si.Dir, "compose.yaml"), []string{
			"image: ghcr.io/agentfeedback/agentfeedback:4.1.0\n",
			fmt.Sprintf("user: \"%d:%d\"\n", os.Getuid(), os.Getgid()),
			`- "127.0.0.1:18190:8080"`,
			"- " + filepath.Dir(si.Database) + ":/data\n",
			"DATABASE_PATH: /data/agentfeedback.db\n",
			"API_KEY_FILE: /run/secrets/api_key\n",
			"file: " + si.KeyFile + "\n",
		}, []string{" up -d", " ps", " logs -f agentfeedback", "exec agentfeedback /opt/agentfeedback backup /data/", " up -d --force-recreate"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			out, r := serverInstall(t, "--"+tc.mode)
			if r.code != 0 || out.Status != "written" || out.Mode != tc.mode || out.Path != tc.path {
				t.Fatalf("%+v", r)
			}
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "network-online") {
				t.Fatalf("%s names network-online.target", tc.path)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(data), w) {
					t.Fatalf("%s lacks %q:\n%s", tc.path, w, data)
				}
			}
			if strings.Contains(string(data), si.APIKey) || strings.Contains(r.stdout+r.stderr, si.APIKey) {
				t.Fatal("the key leaked into the service definition or the output")
			}
			joined := strings.Join(out.Next, "\n")
			for _, w := range tc.next {
				if !strings.Contains(joined, w) {
					t.Fatalf("next lacks %q: %v", w, out.Next)
				}
			}

			// An existing file is refused without --force and replaced with it.
			again, r2 := serverInstall(t, "--"+tc.mode)
			if r2.code != 1 || again.Status != "error" || !strings.Contains(again.Message, "--force") {
				t.Fatalf("second run %+v", r2)
			}
			if _, r3 := serverInstall(t, "--"+tc.mode, "--force"); r3.code != 0 {
				t.Fatalf("--force %+v", r3)
			}
		})
	}
}

func TestServerInstall_ComposeImage(t *testing.T) {
	si, _ := installSetup(t)
	setVersion(t, "dev")
	out, r := serverInstall(t, "--compose")
	if r.code != 1 || !strings.Contains(out.Message, "pass --image") {
		t.Fatalf("dev build: %+v", r)
	}
	if _, r := serverInstall(t, "--compose", "--image", "registry.example/af:1"); r.code != 0 {
		t.Fatalf("--image: %+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(si.Dir, "compose.yaml"))
	if !strings.Contains(string(data), "image: registry.example/af:1\n") {
		t.Fatalf("compose.yaml:\n%s", data)
	}
}

func TestServerInstall_Refusals(t *testing.T) {
	si, _ := installSetup(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "exactly one of"},
		{[]string{"--systemd", "--compose"}, 2, "exactly one of"},
		{[]string{"--systemd", "--image", "x"}, 2, "--image only applies to --compose"},
		{[]string{"--compose", "--image", "x y"}, 1, `contains ' '`},
		{[]string{"--compose", "--image", "x;y"}, 1, `contains ';'`},
		{[]string{"--systemd", "--dir", t.TempDir()}, 1, "run agentfeedback serve --init first"},
		{[]string{"--systemd", "extra"}, 2, "wrong number of arguments"},
	} {
		out, r := serverInstall(t, tc.args...)
		if r.code != tc.code || out.Status != "error" || !strings.Contains(out.Message, tc.want) {
			t.Fatalf("%v: %+v", tc.args, r)
		}
	}
	if r := runCLI(t, "", "server", "start"); r.code != 2 || !strings.Contains(r.stderr, "unknown server subcommand") {
		t.Fatalf("server start: %+v", r)
	}

	writeFile(t, si.EnvFile, "API_KEY_FILE=/k\nDATABASE_PATH=/d/a.db\nHTTP_LISTEN_ADDR=127.0.0.1:1\nEXTRA=1\n", 0o600)
	if out, r := serverInstall(t, "--systemd"); r.code != 1 || !strings.Contains(out.Message, `sets "EXTRA"`) {
		t.Fatalf("unknown key: %+v", r)
	}
	writeFile(t, si.EnvFile, "API_KEY_FILE=/k k\nDATABASE_PATH=/d/a.db\nHTTP_LISTEN_ADDR=127.0.0.1:1\n", 0o600)
	if out, r := serverInstall(t, "--systemd"); r.code != 1 || !strings.Contains(out.Message, `contains ' '`) {
		t.Fatalf("unsafe path: %+v", r)
	}
}

func TestServerInstall_SystemdRefusesRoot(t *testing.T) {
	installSetup(t)
	orig := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = orig })
	out, r := serverInstall(t, "--systemd")
	if r.code != 1 || !strings.Contains(out.Message, "refuses to run as root") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd")); !os.IsNotExist(err) {
		t.Fatalf("a refused run wrote: %v", err)
	}
}

func TestServerInstall_ImageTag(t *testing.T) {
	si, _ := installSetup(t)
	for v, want := range map[string]string{
		"v4.1.0":        "4.1.0",
		"v4.1.0-rc.2":   "4.1.0-rc.2",
		"v4.1.0-dirty":  "",
		"v4.1.0-beta.1": "",
		"v4.1.0-rc.x":   "",
		"v4.1.0+dirty":  "",
		"dev":           "",
	} {
		setVersion(t, v)
		out, r := serverInstall(t, "--compose", "--force")
		if want == "" {
			if r.code != 1 || !strings.Contains(out.Message, "pass --image") {
				t.Fatalf("%s: %+v", v, r)
			}

			continue
		}
		data, _ := os.ReadFile(filepath.Join(si.Dir, "compose.yaml"))
		if r.code != 0 || !strings.Contains(string(data), "image: ghcr.io/agentfeedback/agentfeedback:"+want+"\n") {
			t.Fatalf("%s: %+v\n%s", v, r, data)
		}
	}
}

func TestServerInstall_PortNormalised(t *testing.T) {
	si, _ := installSetup(t)
	writeFile(t, si.EnvFile, "API_KEY_FILE="+si.KeyFile+"\nDATABASE_PATH="+si.Database+"\nHTTP_LISTEN_ADDR=127.0.0.1:+8080\n", 0o600)
	if _, r := serverInstall(t, "--compose", "--image", "x"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(si.Dir, "compose.yaml"))
	if !strings.Contains(string(data), `- "127.0.0.1:8080:8080"`) {
		t.Fatalf("compose.yaml:\n%s", data)
	}
}

func TestServerInstall_KeyFileChecked(t *testing.T) {
	si, _ := installSetup(t)
	if err := os.Remove(si.KeyFile); err != nil {
		t.Fatal(err)
	}
	out, r := serverInstall(t, "--systemd")
	if r.code != 1 || !strings.Contains(out.Message, "does not exist") || !strings.Contains(out.Message, "serve --init") {
		t.Fatalf("missing: %+v", r)
	}
	if err := os.Mkdir(si.KeyFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, r := serverInstall(t, "--systemd"); r.code != 1 || !strings.Contains(out.Message, "is not a regular file") {
		t.Fatalf("directory: %+v", r)
	}
}

func TestServerInstall_LaunchdRefusedLeavesNoLogDir(t *testing.T) {
	installSetup(t)
	home := os.Getenv("HOME")
	plist := filepath.Join(home, "Library", "LaunchAgents", "dev.agentfeedback.serve.plist")
	writeFile(t, plist, "existing", 0o600)
	if _, r := serverInstall(t, "--launchd"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Logs")); !os.IsNotExist(err) {
		t.Fatalf("a refused run created the log directory: %v", err)
	}
}

func TestServerInstall_ComposeNeedsNoBinary(t *testing.T) {
	installSetup(t)
	executable = func() (string, error) { return "/a b/agentfeedback", nil }
	if _, r := serverInstall(t, "--compose", "--image", "x"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestServerInstall_Windows(t *testing.T) {
	installSetup(t)
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })
	out, r := serverInstall(t, "--compose", "--image", "x")
	if r.code != 2 || !strings.Contains(out.Message, "not supported on Windows") {
		t.Fatalf("%+v", r)
	}
}
