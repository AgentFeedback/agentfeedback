package collect

import (
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"testing"
)

// allowedEnv is the documented allow-list, written out independently of the
// harnesses table.
var allowedEnv = map[string]bool{
	"PI_CODING_AGENT_DIR": true, "OMP_PROFILE": true,
	"PI_CODING_AGENT": true, "PI_PROFILE": true, "PI_MODEL": true,
	"OPENCODE": true, "OPENCODE_MODEL": true,
	"CODEX_SANDBOX": true,
	"CLAUDECODE":    true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_EFFORT": true,
	"AI_AGENT": true, "AGENT_FEEDBACK_SESSION_ID": true,
}

func TestHarnessAllowList(t *testing.T) {
	tests := []struct {
		harness string
		markers []string
		vars    map[string]string // variable -> context key
		model   string
	}{
		{"omp", []string{"PI_CODING_AGENT_DIR", "OMP_PROFILE"}, map[string]string{"OMP_PROFILE": "profile"}, ""},
		{"pi", []string{"PI_CODING_AGENT"}, map[string]string{"PI_PROFILE": "profile"}, "PI_MODEL"},
		{"opencode", []string{"OPENCODE"}, nil, "OPENCODE_MODEL"},
		{"codex", []string{"CODEX_SANDBOX"}, nil, ""},
		{"claude-code", []string{"CLAUDECODE"}, map[string]string{"CLAUDE_CODE_SESSION_ID": "session_id", "CLAUDE_EFFORT": "effort"}, ""},
		{"pi", []string{"PI_MODEL"}, map[string]string{"PI_PROFILE": "profile"}, "PI_MODEL"},
		{"opencode", []string{"OPENCODE_MODEL"}, nil, "OPENCODE_MODEL"},
	}
	for _, tt := range tests {
		for _, marker := range tt.markers {
			t.Run(tt.harness+"/"+marker, func(t *testing.T) {
				env := map[string]string{marker: "1", "AI_AGENT": "agent-x"}
				want := map[string]string{"agent": "agent-x"}
				for v, key := range tt.vars {
					if v == marker {
						continue
					}
					env[v] = "val-" + v
					want[key] = "val-" + v
				}
				if v, ok := tt.vars[marker]; ok {
					want[v] = "1"
				}
				if tt.model != "" && tt.model != marker {
					env[tt.model] = "model-x"
				}
				// Another harness's variable must not leak into this report.
				if tt.harness != "claude-code" {
					env["CLAUDE_CODE_SESSION_ID"] = "leak"
					env["CLAUDE_EFFORT"] = "leak"
				}
				got := map[string]string{}
				name, model := collectHarness(envMap(env), func(k, v string) {
					if v != "" {
						got[k] = v
					}
				})
				wantModel := ""
				switch {
				case tt.model == marker:
					wantModel = "1"
				case tt.model != "":
					wantModel = "model-x"
				}
				if name != tt.harness || model != wantModel || !maps.Equal(got, want) {
					t.Fatalf("got %q %q %v, want %q %q %v", name, model, got, tt.harness, wantModel, want)
				}
			})
		}
	}
}

func TestHarnessNestedOrder(t *testing.T) {
	tests := []struct {
		markers []string
		want    string
	}{
		{[]string{"CLAUDECODE", "PI_CODING_AGENT"}, "pi"},
		{[]string{"CLAUDECODE", "PI_CODING_AGENT", "OMP_PROFILE"}, "omp"},
		{[]string{"PI_CODING_AGENT", "PI_CODING_AGENT_DIR"}, "omp"},
		{[]string{"OPENCODE", "CODEX_SANDBOX", "CLAUDECODE"}, "opencode"},
		{[]string{"PI_CODING_AGENT", "OPENCODE"}, "pi"},
		{[]string{"CODEX_SANDBOX", "CLAUDECODE"}, "codex"},
		{[]string{"CLAUDECODE", "PI_MODEL", "OPENCODE_MODEL"}, "claude-code"},
		{[]string{"PI_MODEL", "OPENCODE_MODEL"}, "pi"},
		{[]string{"OPENCODE_MODEL"}, "opencode"},
		{nil, ""},
	}
	for _, tt := range tests {
		env := map[string]string{}
		for _, m := range tt.markers {
			env[m] = "1"
		}
		if got, _ := collectHarness(envMap(env), func(string, string) {}); got != tt.want {
			t.Errorf("%v: got %q, want %q", tt.markers, got, tt.want)
		}
	}
}

func TestSessionOverride(t *testing.T) {
	env := map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "cc", "AGENT_FEEDBACK_SESSION_ID": "af"}
	r := Collect(Options{Dir: t.TempDir(), Getenv: envMap(env), LookPath: noGit, Hostname: fixedHost})
	if r.Context["session_id"] != "af" || r.Harness != "claude-code" {
		t.Fatalf("%+v", r)
	}
	delete(env, "CLAUDECODE")
	r = Collect(Options{Dir: t.TempDir(), Getenv: envMap(env), LookPath: noGit, Hostname: fixedHost})
	if r.Context["session_id"] != "af" || r.Harness != "" {
		t.Fatalf("no harness, common vars still read: %+v", r)
	}
}

// hostileEnv carries secrets and look-alike variables with unique canary
// values beside the allow-listed ones.
func hostileEnv() map[string]string {
	env := map[string]string{}
	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_API_KEY", "CODEX_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"AGENT_FEEDBACK_API_KEY", "AGENT_FEEDBACK_URL", "AGENT_FEEDBACK_HARNESS", "AGENT_FEEDBACK_MODEL",
		"AGENT_FEEDBACK_MACHINE", "SSH_AUTH_SOCK", "HOME", "USER", "LOGNAME", "PATH", "PWD", "SHELL", "TERM",
		"HOSTNAME", "OPENCODE_FOO", "CODEX_BAR", "CLAUDE_CODE_FOO", "PI_SECRET", "OMP_TOKEN", "REVIEW_CALLER_MODEL",
		"NPM_TOKEN", "DOCKER_AUTH_CONFIG", "KUBECONFIG", "DATABASE_URL", "API_KEY", "SECRET", "PASSWORD",
		"HISTFILE", "EDITOR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GIT_ASKPASS", "SLACK_TOKEN",
	} {
		env[name] = "canary-" + strings.ToLower(name) + "-7c1e"
	}
	for name := range allowedEnv {
		env[name] = "allowed-" + strings.ToLower(name)
	}

	return env
}

func TestHostileEnvironment(t *testing.T) {
	hermeticGit(t)
	env := hostileEnv()
	var mu sync.Mutex
	getenv := func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		if !allowedEnv[name] {
			t.Errorf("looked up %q, which is not on the allow-list", name)
		}

		return env[name]
	}
	base := t.TempDir()
	copyFixture(t, base+"/repo")
	for _, dir := range []string{base, base + "/repo"} {
		for _, lp := range []func(string) (string, error){nil, noGit} {
			r := Collect(Options{Dir: dir, Home: base, Getenv: getenv, LookPath: lp, Hostname: fixedHost, WithCwd: true})
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for name, v := range env {
				if !allowedEnv[name] && strings.Contains(string(b), v) {
					t.Errorf("%s leaked: %s", name, b)
				}
			}
			if r.Harness != "omp" || r.Context["profile"] != "allowed-omp_profile" {
				t.Errorf("harness %+v", r)
			}
		}
	}
}
