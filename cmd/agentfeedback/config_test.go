package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateServerEnv clears every variable loadConfig reads.
func isolateServerEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"API_KEY", "API_KEY_FILE", "DATABASE_PATH", "HTTP_LISTEN_ADDR",
		"SERVICE_VERSION", "LOG_LEVEL", "PUBLIC_URL", "MCP_INSTRUCTIONS",
		"GRACEFUL_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	isolateServerEnv(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)

	cfg, err := loadConfig(false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := filepath.Join(data, "agentfeedback", "agentfeedback.db")
	if cfg.DatabasePath != want {
		t.Errorf("DatabasePath = %q, want %q", cfg.DatabasePath, want)
	}
	if cfg.HTTPListenAddr != "127.0.0.1:8090" {
		t.Errorf("HTTPListenAddr = %q, want 127.0.0.1:8090", cfg.HTTPListenAddr)
	}
	info, err := os.Stat(filepath.Dir(want))
	if err != nil {
		t.Fatalf("data directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Dir(want))
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("data directory mode = %o, want 700", perm)
	}
}

func TestLoadConfig_ExplicitDatabaseDirMustExist(t *testing.T) {
	isolateServerEnv(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("DATABASE_PATH", filepath.Join(missing, "agentfeedback.db"))

	_, err := loadConfig(false)
	if err == nil {
		t.Fatal("loadConfig succeeded with DATABASE_PATH in a missing directory")
	}
	if want := "DATABASE_PATH directory " + missing + " is not usable"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want the errDatabaseDir error for %s", err, missing)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("loadConfig created the explicit directory %s", missing)
	}
}
