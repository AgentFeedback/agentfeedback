package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

type config struct {
	APIKey          string
	DatabasePath    string
	HTTPListenAddr  string
	ShutdownTimeout time.Duration
	ServiceVersion  string
	LogLevel        string
	PublicURL       string
	MCPInstructions string
}

// defaultServiceVersion is the build version without a leading "v", the form
// /api/v1/meta reports; SERVICE_VERSION overrides it.
func defaultServiceVersion() string {
	return strings.TrimPrefix(clientVersion().Version, "v")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func envDurationOr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, errDuration(key, v, err)
	}
	if d <= 0 {
		return 0, errDuration(key, v, nil)
	}

	return d, nil
}

// loadConfig reads the environment and fails fast on anything that would only
// surface later as a failed write.
func loadConfig(requireAPIKey bool) (config, error) {
	cfg := config{
		APIKey:          os.Getenv("API_KEY"),
		DatabasePath:    envOr("DATABASE_PATH", "/data/agentfeedback.db"),
		HTTPListenAddr:  envOr("HTTP_LISTEN_ADDR", "0.0.0.0:8080"),
		ServiceVersion:  envOr("SERVICE_VERSION", defaultServiceVersion()),
		LogLevel:        envOr("LOG_LEVEL", "info"),
		MCPInstructions: os.Getenv("MCP_INSTRUCTIONS"),
	}
	if raw := os.Getenv("PUBLIC_URL"); raw != "" {
		u, err := skillgen.NormalizeServer(raw)
		if err != nil {
			return config{}, errPublicURL(err)
		}
		cfg.PublicURL = u
	}

	timeout, err := envDurationOr("GRACEFUL_SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return config{}, err
	}
	cfg.ShutdownTimeout = timeout

	if requireAPIKey {
		if path := os.Getenv("API_KEY_FILE"); path != "" {
			if cfg.APIKey != "" {
				return config{}, errAPIKeyBoth()
			}
			key, err := readKeyFile(path)
			if err != nil {
				return config{}, err
			}
			cfg.APIKey = key
		}
		if cfg.APIKey == "" {
			return config{}, errAPIKeyUnset()
		}
	}
	switch cfg.LogLevel {
	case "debug", "info":
	default:
		return config{}, errLogLevel(cfg.LogLevel)
	}
	if err := checkWritableDir(filepath.Dir(cfg.DatabasePath)); err != nil {
		return config{}, err
	}

	return cfg, nil
}

// readKeyFile reads the key API_KEY_FILE names, with the rules of a key read
// from stdin. Errors name the path, never the content.
func readKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errAPIKeyFile(path, "cannot be read: "+pathCause(err))
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, keyReadLimit+1))
	if err != nil {
		return "", errAPIKeyFile(path, "cannot be read: "+pathCause(err))
	}
	if len(data) > keyReadLimit {
		return "", errAPIKeyFile(path, "is longer than 64 KiB")
	}
	key, ok := parseKey(data)
	switch {
	case key == "":
		return "", errAPIKeyFile(path, "is empty")
	case !ok:
		return "", errAPIKeyFile(path, "holds whitespace or control characters inside the key")
	}

	return key, nil
}

// pathCause is err without the path a *fs.PathError repeats.
func pathCause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}

	return err.Error()
}

// checkWritableDir verifies the database's directory exists and accepts writes,
// so a misconfigured volume fails at boot rather than on the first submission.
func checkWritableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return errDatabaseDir(dir, fmt.Sprintf("is not usable: %v", err))
	}
	if !info.IsDir() {
		return errDatabaseDir(dir, "is not a directory")
	}

	probe, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return errDatabaseDir(dir, fmt.Sprintf("is not writable: %v", err))
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)

	return nil
}
