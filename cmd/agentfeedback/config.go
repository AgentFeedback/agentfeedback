package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	APIKey          string
	DatabasePath    string
	HTTPListenAddr  string
	ShutdownTimeout time.Duration
	ServiceVersion  string
	LogLevel        string
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
		APIKey:         os.Getenv("API_KEY"),
		DatabasePath:   envOr("DATABASE_PATH", "/data/agentfeedback.db"),
		HTTPListenAddr: envOr("HTTP_LISTEN_ADDR", "0.0.0.0:8080"),
		ServiceVersion: envOr("SERVICE_VERSION", defaultServiceVersion()),
		LogLevel:       envOr("LOG_LEVEL", "info"),
	}

	timeout, err := envDurationOr("GRACEFUL_SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return config{}, err
	}
	cfg.ShutdownTimeout = timeout

	if requireAPIKey && cfg.APIKey == "" {
		return config{}, errAPIKeyUnset()
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
