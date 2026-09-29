package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Client environment variables. Each overrides the config file key of the
// same meaning; an empty value counts as unset.
const (
	envURL     = "AGENT_FEEDBACK_URL"
	envAPIKey  = "AGENT_FEEDBACK_API_KEY"
	envMachine = "AGENT_FEEDBACK_MACHINE"
	envModel   = "AGENT_FEEDBACK_MODEL"
	envHarness = "AGENT_FEEDBACK_HARNESS"
)

// Sources of a resolved value, in precedence order; "" means unset.
const (
	sourceFlag   = "flag"
	sourceEnv    = "env"
	sourceConfig = "config"
)

// fileConfig is the config file: flat top-level keys. Keys and tables it does
// not name are ignored so newer files stay readable.
type fileConfig struct {
	URL     string `toml:"url"`
	APIKey  string `toml:"api_key"`
	Machine string `toml:"machine"`
	Model   string `toml:"model"`
	Harness string `toml:"harness"`
}

// flagConfig holds the values given on the command line. There is no API key
// field: a key on the command line would land in shell history and ps.
type flagConfig struct {
	URL     string
	Machine string
	Model   string
	Harness string
}

// resolvedValue is one setting and where it came from.
type resolvedValue struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type clientSettings struct {
	URL     resolvedValue
	APIKey  resolvedValue
	Machine resolvedValue
	Model   resolvedValue
	Harness resolvedValue
}

// resolveClient applies flag > env > config to every setting; the API key has
// no flag and resolves env > config.
func resolveClient(flags flagConfig, getenv func(string) string, file fileConfig) clientSettings {
	pick := func(flagValue, envName, fileValue string) resolvedValue {
		switch {
		case flagValue != "":
			return resolvedValue{flagValue, sourceFlag}
		case getenv(envName) != "":
			return resolvedValue{getenv(envName), sourceEnv}
		case fileValue != "":
			return resolvedValue{fileValue, sourceConfig}
		}

		return resolvedValue{}
	}

	return clientSettings{
		URL:     pick(flags.URL, envURL, file.URL),
		APIKey:  pick("", envAPIKey, file.APIKey),
		Machine: pick(flags.Machine, envMachine, file.Machine),
		Model:   pick(flags.Model, envModel, file.Model),
		Harness: pick(flags.Harness, envHarness, file.Harness),
	}
}

// xdgDir returns $<env>/agentfeedback when the variable holds an absolute
// path, else ~/<fallback>/agentfeedback.
func xdgDir(getenv func(string) string, env, fallback string) (string, error) {
	if d := getenv(env); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "agentfeedback"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errNoHome(err)
	}

	return filepath.Join(home, fallback, "agentfeedback"), nil
}

// configPath is the client config file.
func configPath(getenv func(string) string) (string, error) {
	dir, err := xdgDir(getenv, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "config.toml"), nil
}

// cacheDir holds the spool (spool/, rejected/ beside it) and the client log
// (log/client.jsonl).
func cacheDir(getenv func(string) string) (string, error) {
	return xdgDir(getenv, "XDG_CACHE_HOME", ".cache")
}

// loadFileConfig reads the config file. A missing file is not an error: it
// returns exists=false and an empty config.
func loadFileConfig(path string) (cfg fileConfig, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileConfig{}, false, nil
	}
	if err != nil {
		return fileConfig{}, true, errConfigRead(path, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return fileConfig{}, true, errConfigInvalid(path, err)
	}

	return cfg, true, nil
}
