package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func configSettingsTestEnv(t *testing.T, body string) (func(string) string, string) {
	t.Helper()
	root := t.TempDir()
	env := map[string]string{"XDG_CONFIG_HOME": root, "XDG_DATA_HOME": root}
	getenv := func(key string) string { return env[key] }
	path, err := configPath(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return getenv, path
}

func configSettingsTestMap(t *testing.T, settings []configSetting) map[string]configSetting {
	t.Helper()
	out := make(map[string]configSetting)
	for _, setting := range settings {
		if _, exists := out[setting.Key]; exists {
			t.Fatalf("duplicate setting %s", setting.Key)
		}
		out[setting.Key] = setting
	}
	return out
}

func configSettingsTestWant(key string, value any, source string, set bool) configSetting {
	return configSetting{Key: key, Value: value, Source: source, Editable: slices.Contains(configEditableKeys(), key), Set: set}
}

func TestEffectiveConfigSettingsDefaultsAndUnset(t *testing.T) {
	getenv, path := configSettingsTestEnv(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gotPath, _, settings, err := effectiveConfigSettings(modeFlags{}, getenv)
	if err != nil || gotPath != path {
		t.Fatalf("path=%q err=%v", gotPath, err)
	}
	values := map[string]any{
		"mode": "local", "url": "", "api_key": "", "machine": "", "model": "", "harness": "",
		"collect.deny_paths": []string{}, "collect.opt_in_paths": []string{}, "collect.opt_in_only": false, "collect.disabled": false,
		"context.cwd": false, "context.drop": []string{}, "context.app": "", "context.workspace": "", "context.url": "",
		"context.channel": "", "context.task_id": "", "context.workflow": "",
		"detect.nudge": true, "detect.same_tool": 3, "detect.same_command": 2, "detect.session_failures": 5,
		"detect.interval_minutes": 20, "detect.max_per_session": 3,
		"sessions.quiet": "5m0s", "sessions.policy_ttl": "5m0s", "sessions.push_batch": 5, "sessions.push_budget": "1m30s",
	}
	want := make(map[string]configSetting)
	for key, value := range values {
		set := true
		if text, ok := value.(string); ok && text == "" {
			set = false
		}
		want[key] = configSettingsTestWant(key, value, "default", set)
	}
	if got := configSettingsTestMap(t, settings); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %#v; want %#v", got, want)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created config: %v", err)
	}
}

func TestEffectiveConfigSettingsEveryValueAndSource(t *testing.T) {
	getenv, _ := configSettingsTestEnv(t, `url = "https://file.example"
api_key = "fixture-file-secret"
machine = "file-machine"
model = "file-model"
harness = "file-harness"
[collect]
deny_paths = ["/private"]
opt_in_paths = ["~/work"]
opt_in_only = true
disabled = false
[context]
cwd = true
drop = ["remote"]
app = "editor"
workspace = "workspace"
url = "https://work.example"
channel = "channel"
task_id = "task"
workflow = "workflow"
[detect]
nudge = false
same_tool = 7
same_command = 8
session_failures = 9
interval_minutes = 10
max_per_session = 11
[sessions]
quiet = "2m"
policy_ttl = "3m"
push_batch = 12
push_budget = "45s"
`)
	baseEnv := getenv
	getenv = func(key string) string {
		switch key {
		case envURL:
			return "https://environment.example"
		case envAPIKey:
			return "fixture-environment-secret"
		case envMachine:
			return "environment-machine"
		case envModel:
			return "environment-model"
		default:
			return baseEnv(key)
		}
	}
	_, _, settings, err := effectiveConfigSettings(modeFlags{server: "https://flag.example"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"mode": "remote", "url": "https://flag.example", "api_key": "[REDACTED]",
		"machine": "environment-machine", "model": "environment-model", "harness": "file-harness",
		"collect.deny_paths": []string{"/private"}, "collect.opt_in_paths": []string{"~/work"},
		"collect.opt_in_only": true, "collect.disabled": false,
		"context.cwd": true, "context.drop": []string{"remote"}, "context.app": "editor", "context.workspace": "workspace",
		"context.url": "https://work.example", "context.channel": "channel", "context.task_id": "task", "context.workflow": "workflow",
		"detect.nudge": false, "detect.same_tool": 7, "detect.same_command": 8, "detect.session_failures": 9,
		"detect.interval_minutes": 10, "detect.max_per_session": 11,
		"sessions.quiet": "2m0s", "sessions.policy_ttl": "3m0s", "sessions.push_batch": 12, "sessions.push_budget": "45s",
	}
	want := make(map[string]configSetting)
	for key, value := range values {
		source := "file"
		switch key {
		case "mode", "url":
			source = "flag"
		case "api_key", "machine", "model":
			source = "environment"
		}
		want[key] = configSettingsTestWant(key, value, source, true)
	}
	if got := configSettingsTestMap(t, settings); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %#v; want %#v", got, want)
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-file-secret", "fixture-environment-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("effective settings exposed a secret")
		}
	}
}

func TestEffectiveConfigSettingsPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name       string
		body       string
		envURL     string
		flags      modeFlags
		mode       string
		modeSource string
		urlSource  string
	}{
		{"file", "url = 'https://file.example'", "", modeFlags{}, "remote", "file", "file"},
		{"environment", "url = 'https://file.example'", "https://env.example", modeFlags{}, "remote", "environment", "environment"},
		{"local override", "url = 'https://file.example'", "https://env.example", modeFlags{local: true}, "local", "flag", "environment"},
		{"server override", "", "https://env.example", modeFlags{server: "https://flag.example"}, "remote", "flag", "flag"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := configSettingsTestEnv(t, tt.body)
			getenv := func(key string) string {
				if key == envURL {
					return tt.envURL
				}
				return base(key)
			}
			_, _, settings, err := effectiveConfigSettings(tt.flags, getenv)
			if err != nil {
				t.Fatal(err)
			}
			got := configSettingsTestMap(t, settings)
			if got["mode"].Value != tt.mode || got["mode"].Source != tt.modeSource || got["url"].Source != tt.urlSource {
				t.Fatalf("mode=%+v url=%+v", got["mode"], got["url"])
			}
		})
	}
	getenv, _ := configSettingsTestEnv(t, "api_key = 'fixture-key'\n")
	_, _, settings, err := effectiveConfigSettings(modeFlags{}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got := configSettingsTestMap(t, settings)["api_key"]; got.Value != "[REDACTED]" || got.Source != "file" || !got.Set {
		t.Fatalf("masked file key = %+v", got)
	}
	if _, _, _, err := effectiveConfigSettings(modeFlags{local: true, server: "https://flag.example"}, getenv); err == nil {
		t.Fatal("accepted conflicting mode flags")
	}
}

func TestEffectiveConfigSettingsIgnoredNonpositiveValues(t *testing.T) {
	getenv, _ := configSettingsTestEnv(t, `[DETECT]
NUDGE = false
SAME_TOOL = 0
SAME_COMMAND = -1
SESSION_FAILURES = 0
INTERVAL_MINUTES = -1
MAX_PER_SESSION = 0
[SESSIONS]
QUIET = '0s'
POLICY_TTL = '-1m'
PUSH_BATCH = -1
PUSH_BUDGET = '0s'
[COLLECT]
DISABLED = false
DENY_PATHS = []
`)
	_, _, settings, err := effectiveConfigSettings(modeFlags{}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	got := configSettingsTestMap(t, settings)
	for _, key := range []string{"detect.same_tool", "detect.same_command", "detect.session_failures", "detect.interval_minutes", "detect.max_per_session", "sessions.quiet", "sessions.policy_ttl", "sessions.push_batch", "sessions.push_budget"} {
		if got[key].Source != "default" || !got[key].Set {
			t.Errorf("%s = %+v", key, got[key])
		}
	}
	for _, key := range []string{"detect.nudge", "collect.disabled", "collect.deny_paths"} {
		if got[key].Source != "file" || !got[key].Set {
			t.Errorf("%s = %+v", key, got[key])
		}
	}
}

func TestSetConfigSettingRoundTripUnknownRetentionAndPermissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body := `api_key = 'fixture-retained-key'
future = ['one', 'two']
[CoLlEcT]
DeNy_PaThS = ['/old']
future = {enabled = true, names = ['x', 'y']}
[future_table]
answer = 42
[[future_table.items]]
name = 'first'
`
	getenv, path := configSettingsTestEnv(t, body)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := toml.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "directory with spaces")
	comma := filepath.Join(t.TempDir(), "directory,with,commas")
	encoded, err := json.Marshal([]string{absolute, comma, "~/work"})
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		key, raw string
	}{
		{"collect.deny_paths", string(encoded)},
		{"collect.opt_in_paths", absolute + ", ~/work"},
		{"collect.opt_in_only", "true"},
		{"collect.disabled", "false"},
		{"sessions.quiet", "12m"},
		{"context.cwd", "true"},
		{"detect.nudge", "false"},
		{"detect.same_tool", "6"},
		{"detect.same_command", "7"},
		{"detect.session_failures", "8"},
		{"detect.interval_minutes", "9"},
		{"detect.max_per_session", "10"},
	} {
		if err := setConfigSetting(getenv, edit.key, edit.raw); err != nil {
			t.Fatalf("set %s: %v", edit.key, err)
		}
	}
	file, exists, err := loadFileConfig(path)
	if err != nil || !exists {
		t.Fatalf("load exists=%v err=%v", exists, err)
	}
	if !slices.Equal(file.Collect.DenyPaths, []string{absolute, comma, "~/work"}) || !slices.Equal(file.Collect.OptInPaths, []string{absolute, "~/work"}) || !file.Collect.OptInOnly || file.Collect.Disabled || !file.Context.Cwd || file.Detect.Nudge == nil || *file.Detect.Nudge || file.Sessions.Quiet == nil || *file.Sessions.Quiet != "12m" {
		t.Fatalf("round trip = %+v", file)
	}
	if file.Detect.SameTool != 6 || file.Detect.SameCommand != 7 || file.Detect.SessionFailures != 8 || file.Detect.IntervalMinutes != 9 || file.Detect.MaxPerSession != 10 {
		t.Fatalf("thresholds = %+v", file.Detect)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("config was not atomically replaced")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", after.Mode().Perm())
	}
	doc, err := configSettingsDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"api_key", "future", "future_table"} {
		if !reflect.DeepEqual(doc[key], want[key]) {
			t.Errorf("unknown/readonly key %s changed: %#v != %#v", key, doc[key], want[key])
		}
	}
	table := doc["CoLlEcT"].(map[string]any)
	if !reflect.DeepEqual(table["future"], want["CoLlEcT"].(map[string]any)["future"]) {
		t.Fatal("unknown collect field lost")
	}
	if _, duplicate := doc["collect"]; duplicate {
		t.Fatal("created competing lowercase collect table")
	}
	if _, duplicate := table["deny_paths"]; duplicate {
		t.Fatal("created competing lowercase deny_paths key")
	}
	for _, raw := range []string{"", "[]"} {
		if err := setConfigSetting(getenv, "collect.deny_paths", raw); err != nil {
			t.Fatal(err)
		}
		file, _, err = loadFileConfig(path)
		if err != nil || len(file.Collect.DenyPaths) != 0 {
			t.Fatalf("clear %q = %v err=%v", raw, file.Collect.DenyPaths, err)
		}
	}
}

func TestSetConfigSettingRefusedAndInvalidUnchanged(t *testing.T) {
	for _, tt := range []struct {
		key, raw string
	}{
		{"api_key", "fixture-refused-secret"},
		{"API_KEY", "fixture-refused-secret"},
		{"url", "https://refused.example"},
		{"mode", "remote"},
		{"machine", "machine"},
		{"model", "model"},
		{"harness", "harness"},
		{"context.drop", "remote"},
		{"sessions.policy_ttl", "1m"},
		{"sessions.push_batch", "1"},
		{"sessions.push_budget", "1s"},
		{"unknown", "fixture-refused-value"},
		{"collect.disabled", "TRUE"},
		{"context.cwd", "1"},
		{"detect.nudge", "yes"},
		{"collect.opt_in_only", " true "},
		{"detect.same_tool", "0"},
		{"detect.same_command", "-1"},
		{"detect.session_failures", "1.5"},
		{"detect.interval_minutes", "many"},
		{"detect.interval_minutes", "153722868"},
		{"detect.max_per_session", "99999999999999999999999999"},
		{"sessions.quiet", "0s"},
		{"sessions.quiet", "-1m"},
		{"sessions.quiet", "not-duration"},
		{"collect.deny_paths", "relative"},
		{"collect.opt_in_paths", "~someone/work"},
		{"collect.deny_paths", `["/safe", "relative"]`},
		{"collect.opt_in_paths", `[null]`},
		{"collect.deny_paths", `[1]`},
		{"collect.deny_paths", `["/safe", true]`},
		{"collect.deny_paths", `["/safe"`},
		{"collect.deny_paths", "/safe,"},
	} {
		t.Run(tt.key+"/"+tt.raw, func(t *testing.T) {
			const body = "api_key = 'fixture-existing-key'\n[collect]\ndisabled = false\n"
			getenv, path := configSettingsTestEnv(t, body)
			err := setConfigSetting(getenv, tt.key, tt.raw)
			var ue *userError
			if !errors.As(err, &ue) || !ue.usage {
				t.Fatalf("want usage error, got %v", err)
			}
			if strings.Contains(err.Error(), tt.raw) && strings.Contains(tt.raw, "fixture-refused") {
				t.Fatal("error echoed refused value")
			}
			if strings.EqualFold(tt.key, "api_key") || tt.key == "url" {
				if !strings.Contains(err.Error(), "doctor --init") || !strings.Contains(err.Error(), "agentfeedback init") {
					t.Fatalf("missing init guidance: %v", err)
				}
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || string(data) != body {
				t.Fatalf("refused edit changed config: %q err=%v", data, readErr)
			}
		})
	}
}

func TestSetConfigSettingRejectsInvalidResultAndAmbiguousCase(t *testing.T) {
	for _, body := range []string{
		"[sessions]\npolicy_ttl = 'invalid'\n",
		"[context]\ncwd = 'wrong type'\n",
		"[collect]\ndisabled = false\n[COLLECT]\nfuture = true\n",
		"[collect]\ndisabled = false\nDISABLED = true\n",
		"url = 'https://one.test'\nURL = 'https://two.test'\n",
		"api_key = 'one'\nAPI_KEY = 'two'\n",
		"[context]\ncwd = true\nCWD = false\n",
		"[sessions]\nquiet = '1m'\nQUIET = '2m'\n",
		"collect = 'not a table'\n",
		"not valid TOML",
	} {
		t.Run(body, func(t *testing.T) {
			getenv, path := configSettingsTestEnv(t, body)
			if err := setConfigSetting(getenv, "collect.disabled", "true"); err == nil {
				t.Fatal("accepted invalid config")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != body {
				t.Fatalf("invalid result changed config: %q err=%v", data, err)
			}
		})
	}
}

func TestSetConfigSettingCreatesAndEditsEmptyFile(t *testing.T) {
	for _, missing := range []bool{false, true} {
		getenv, path := configSettingsTestEnv(t, "")
		if missing {
			if err := os.RemoveAll(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
		}
		if err := setConfigSetting(getenv, "context.cwd", "true"); err != nil {
			t.Fatal(err)
		}
		file, exists, err := loadFileConfig(path)
		if err != nil || !exists || !file.Context.Cwd {
			t.Fatalf("create exists=%v file=%+v err=%v", exists, file, err)
		}
	}
}

func TestConfigValueText(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  string
	}{
		{"plain", "plain"},
		{true, "true"},
		{false, "false"},
		{12, "12"},
		{[]string{}, "[]"},
		{[]string{"/a,b", "~/work"}, `["/a,b","~/work"]`},
	} {
		if got := configValueText(tt.value); got != tt.want {
			t.Errorf("text(%#v)=%q want %q", tt.value, got, tt.want)
		}
	}
}
