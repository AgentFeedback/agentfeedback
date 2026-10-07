package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// binaryVersionTimeout bounds one `agentfeedback version --json` run.
const binaryVersionTimeout = 3 * time.Second

// binaryVersion reports the version of the agentfeedback binary at path:
// "missing" when no file is there, "unknown" when it is not a regular file,
// does not run or does not print a version. The child never sees the API
// key. A variable so tests can stand in for it.
var binaryVersion = func(path string) string {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	if err != nil || !fi.Mode().IsRegular() {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), binaryVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--json")
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	cmd.Env = withEnv(os.Environ(), map[string]string{envAPIKey: ""})
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(out, &v) != nil || v.Version == "" {
		return "unknown"
	}

	return v.Version
}

// pathBinary is the agentfeedback a shell finds on PATH, symlinks resolved;
// ok is false when there is none. A variable so tests can stand in for it.
var pathBinary = func() (string, bool) {
	p, err := exec.LookPath("agentfeedback")
	if err != nil {
		return "", false
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		return "", false
	}

	return p, true
}

// pathWarnings compares the agentfeedback on PATH with bin, the binary the
// hooks are wired to: the skill runs the PATH one, so a missing or a
// different one is a warning.
func pathWarnings(bin string) []string {
	p, ok := pathBinary()
	if !ok {
		return []string{"no agentfeedback on PATH; the skill runs agentfeedback from PATH, so add " + filepath.Dir(bin) + " to PATH"}
	}
	if filepath.Clean(p) == filepath.Clean(bin) {
		return nil
	}

	return []string{"the agentfeedback on PATH (" + p + ", version " + binaryVersion(p) + ") is not the one being wired (" +
		bin + ", version " + clientVersion().Version + "); agents run the PATH one and hooks this one, and the newer of the two " +
		"migrates the local database, after which the older one fails: keep one binary"}
}
