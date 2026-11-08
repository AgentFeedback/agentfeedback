package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// configSetting describes the effective machine configuration, not repository or
// organization policy. Set distinguishes absent optional strings from defaults.
type configSetting struct {
	Key      string `json:"key"`
	Value    any    `json:"value"`
	Source   string `json:"source"`
	Editable bool   `json:"editable"`
	Set      bool   `json:"set"`
}

func configEditableKeys() []string {
	return []string{
		"collect.deny_paths", "collect.opt_in_paths", "collect.opt_in_only", "collect.disabled",
		"sessions.quiet", "context.cwd", "detect.nudge", "detect.same_tool", "detect.same_command",
		"detect.session_failures", "detect.interval_minutes", "detect.max_per_session",
	}
}

func configValueText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func effectiveConfigSettings(mf modeFlags, getenv func(string) string) (path string, file fileConfig, settings []configSetting, err error) {
	path, err = configPath(getenv)
	if err != nil {
		return
	}
	file, _, err = loadFileConfig(path)
	if err != nil {
		return
	}
	doc, err := configSettingsDocument(path)
	if err != nil {
		return path, file, nil, err
	}
	mode, err := resolveModeFrom(mf, getenv, file)
	if err != nil {
		return path, file, nil, err
	}
	editable := make(map[string]bool)
	for _, key := range configEditableKeys() {
		editable[key] = true
	}
	add := func(key string, value any, source string, set bool) {
		settings = append(settings, configSetting{Key: key, Value: value, Source: source, Editable: editable[key], Set: set})
	}
	source := func(s string) string {
		switch s {
		case sourceFlag:
			return "flag"
		case sourceEnv:
			return "environment"
		case sourceConfig:
			return "file"
		default:
			return "default"
		}
	}
	modeSource := source(mode.Settings.URL.Source)
	if mf.local {
		modeSource = "flag"
	}
	add("mode", mode.Mode, modeSource, true)
	for _, setting := range []struct {
		key   string
		value resolvedValue
	}{
		{"url", mode.Settings.URL},
		{"api_key", mode.Settings.APIKey},
		{"machine", mode.Settings.Machine},
		{"model", mode.Settings.Model},
		{"harness", mode.Settings.Harness},
	} {
		value := setting.value.Value
		if setting.key == "api_key" && value != "" {
			value = "[REDACTED]"
		}
		if setting.key == "url" && value != "" {
			value = redactURL(value)
		}
		add(setting.key, value, source(setting.value.Source), setting.value.Value != "")
	}
	fileValue := func(key string, value any) {
		s := "default"
		if configSettingsHas(doc, key) {
			s = "file"
		}
		add(key, value, s, true)
	}
	list := func(key string, value []string) {
		if value == nil {
			value = []string{}
		}
		fileValue(key, value)
	}
	optional := func(key, value string) {
		s := "default"
		if value != "" {
			s = "file"
		}
		add(key, value, s, value != "")
	}
	list("collect.deny_paths", file.Collect.DenyPaths)
	list("collect.opt_in_paths", file.Collect.OptInPaths)
	fileValue("collect.opt_in_only", file.Collect.OptInOnly)
	fileValue("collect.disabled", file.Collect.Disabled)
	fileValue("context.cwd", file.Context.Cwd)
	list("context.drop", file.Context.Drop)
	optional("context.app", file.Context.App)
	optional("context.workspace", file.Context.Workspace)
	optional("context.url", redactURL(file.Context.URL))
	optional("context.channel", file.Context.Channel)
	optional("context.task_id", file.Context.TaskID)
	optional("context.workflow", file.Context.Workflow)
	detect := file.Detect.config()
	fileValue("detect.nudge", detect.Nudge)
	positive := func(key string, configured, effective int) {
		s := "default"
		if configured > 0 {
			s = "file"
		}
		add(key, effective, s, true)
	}
	positive("detect.same_tool", file.Detect.SameTool, detect.SameTool)
	positive("detect.same_command", file.Detect.SameCommand, detect.SameCommand)
	positive("detect.session_failures", file.Detect.SessionFailures, detect.SessionFailures)
	positive("detect.interval_minutes", file.Detect.IntervalMinutes, detect.IntervalMinutes)
	positive("detect.max_per_session", file.Detect.MaxPerSession, detect.MaxPerSession)
	sessions := file.Sessions.config()
	duration := func(key string, configured *string, effective time.Duration) {
		s := "default"
		if configured != nil {
			if parsed, parseErr := time.ParseDuration(*configured); parseErr == nil && parsed > 0 {
				s = "file"
			}
		}
		add(key, effective.String(), s, true)
	}
	duration("sessions.quiet", file.Sessions.Quiet, sessions.Quiet)
	duration("sessions.policy_ttl", file.Sessions.PolicyTTL, sessions.PolicyTTL)
	positive("sessions.push_batch", file.Sessions.PushBatch, sessions.PushBatch)
	duration("sessions.push_budget", file.Sessions.PushBudget, sessions.PushBudget)
	return path, file, settings, nil
}

// TOML's struct decoder matches names without case. Lookups and edits must do
// the same rather than introducing a second, competing spelling of a known key.
func configSettingsHas(doc map[string]any, key string) bool {
	parts := strings.SplitN(key, ".", 2)
	for name, value := range doc {
		if !strings.EqualFold(name, parts[0]) {
			continue
		}
		if len(parts) == 1 {
			return true
		}
		if table, ok := value.(map[string]any); ok && configSettingsHas(table, parts[1]) {
			return true
		}
	}
	return false
}

func configSettingsDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]any), nil
	}
	if err != nil {
		return nil, errConfigRead(path, err)
	}
	doc := make(map[string]any)
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, errConfigInvalid(path, err)
	}
	return doc, nil
}

func setConfigSetting(getenv func(string) string, key, raw string) error {
	switch strings.ToLower(key) {
	case "api_key", "url":
		return usageErr("config set cannot change the API key or server URL", "use agentfeedback init or agentfeedback doctor --init to configure the destination and read the key from stdin")
	}
	allowed := false
	for _, editable := range configEditableKeys() {
		if key == editable {
			allowed = true
			break
		}
	}
	if !allowed {
		return usageErr("config set requires a known editable key", "choose one of: "+strings.Join(configEditableKeys(), ", "))
	}
	value, err := parseConfigSetting(key, raw)
	if err != nil {
		return err
	}
	path, err := configPath(getenv)
	if err != nil {
		return err
	}
	return updateConfigFile(path, func(doc map[string]any) error {
		parts := strings.SplitN(key, ".", 2)
		tableName, err := configSettingsSpelling(doc, parts[0])
		if err != nil {
			return err
		}
		table := make(map[string]any)
		if existing, ok := doc[tableName]; ok {
			var isTable bool
			table, isTable = existing.(map[string]any)
			if !isTable {
				return errConfigInvalid(path, fmt.Errorf("%s must be a table", parts[0]))
			}
		}
		name, err := configSettingsSpelling(table, parts[1])
		if err != nil {
			return err
		}
		table[name] = value
		doc[tableName] = table
		return nil
	})
}

// Preserve an existing spelling, but refuse ambiguous case variants: the typed
// decoder's winner must not depend on the encoder's ordering of map entries.
func configSettingsSpelling(doc map[string]any, key string) (string, error) {
	name := ""
	for candidate := range doc {
		if strings.EqualFold(candidate, key) {
			if name != "" {
				return "", usageErr("config contains competing case variants of "+key, "keep a single spelling of that key in the machine config file")
			}
			name = candidate
		}
	}
	if name == "" {
		name = key
	}
	return name, nil
}

func parseConfigSetting(key, raw string) (any, error) {
	invalid := func(want string) error {
		return usageErr("invalid value for "+key, "provide "+want)
	}
	switch key {
	case "collect.deny_paths", "collect.opt_in_paths":
		paths := []string{}
		text := strings.TrimSpace(raw)
		if strings.HasPrefix(text, "[") {
			// Decode elements separately so JSON null cannot silently become an
			// empty string in a []string. Every element must really be a string.
			var values []json.RawMessage
			if err := json.Unmarshal([]byte(text), &values); err != nil {
				return nil, invalid("a comma-separated list or JSON array of absolute or ~/ paths")
			}
			for _, value := range values {
				var path string
				if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &path) != nil {
					return nil, invalid("a JSON array containing only strings")
				}
				paths = append(paths, path)
			}
		} else if text != "" {
			for _, path := range strings.Split(text, ",") {
				paths = append(paths, strings.TrimSpace(path))
			}
		}
		valid, _ := collect.UserPaths(collect.Policy{DenyPaths: paths}, "")
		if len(valid) != len(paths) {
			return nil, invalid("only absolute or ~/ paths (use an empty list to clear)")
		}
		return paths, nil
	case "collect.opt_in_only", "collect.disabled", "context.cwd", "detect.nudge":
		if raw != "true" && raw != "false" {
			return nil, invalid("true or false")
		}
		return raw == "true", nil
	case "sessions.quiet":
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return nil, invalid("a positive duration such as 5m")
		}
		return raw, nil
	default:
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return nil, invalid("a positive integer")
		}
		if key == "detect.interval_minutes" && int64(value) > int64((1<<63-1)/time.Minute) {
			return nil, invalid("an interval that fits in a Go duration")
		}
		return value, nil
	}
}
