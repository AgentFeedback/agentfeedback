package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

const (
	defaultServePort  = 8090
	serveInitSynopsis = "serve --init [--dir D] [--db PATH] [--port N] [--force]"
	keyFileName       = "api-key"
	serveEnvName      = "serve.env"
	serveEnvComment   = "# written by agentfeedback serve --init; HTTP_LISTEN_ADDR stays on 127.0.0.1 unless you decide to expose the server"
)

// serveInitOptions are the flags of serve --init; empty paths take defaults.
type serveInitOptions struct {
	dir, db string
	port    int
	force   bool
}

// serveInitOutcome is the one JSON line serve --init always ends with. It is
// the only place the generated key is printed.
type serveInitOutcome struct {
	Status   string `json:"status"`
	Dir      string `json:"dir,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	EnvFile  string `json:"env_file,omitempty"`
	Database string `json:"database,omitempty"`
	Listen   string `json:"listen,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Message  string `json:"message,omitempty"`
}

// printServeInitOutcome prints the outcome and returns err marked as already
// reported, so run adds nothing to stderr.
func printServeInitOutcome(stdout io.Writer, out serveInitOutcome, err error) error {
	if err == nil {
		out.Status = "written"

		return writeJSON(stdout, out)
	}
	if werr := writeJSON(stdout, serveInitOutcome{Status: "error", Message: err.Error()}); werr != nil {
		return werr
	}

	return &reportedError{err}
}

// serverDir is the default directory of api-key, serve.env and compose.yaml.
func serverDir(getenv func(string) string) (string, error) {
	dir, err := xdgDir(getenv, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "server"), nil
}

// checkSafePath refuses a path the server files cannot hold as written:
// they are sourced by sh, read by systemd, written into the plist and the
// compose YAML unquoted, and Docker splits a volume spec on ":". Only
// letters, digits and / . _ - + @ , = are accepted.
func checkSafePath(what, path string) error {
	return checkSafeRunes(what, path, "")
}

// checkSafeRunes is checkSafePath with extra accepted runes.
func checkSafeRunes(what, value, extra string) error {
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("/._-+@,="+extra, r) {
			return errUnsafePath(what, value, fmt.Sprintf("contains %q", r))
		}
	}

	return nil
}

// checkSupported refuses the server setup commands on Windows, where neither
// the files nor the service managers they target apply.
func checkSupported(command string) error {
	if goos == "windows" {
		return errWindowsUnsupported(command)
	}

	return nil
}

// goos is runtime.GOOS, a variable so tests can stand in for Windows.
var goos = runtime.GOOS

// absSafe makes path absolute and checks it with checkSafePath.
func absSafe(what, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errServeInitWrite(path, err)
	}

	return abs, checkSafePath(what, abs)
}

// runServeInit writes <dir>/api-key with a new key and <dir>/serve.env
// naming it, the database and a loopback listen address. Without force
// nothing is written when either file exists, and each is created
// exclusively; with force both are replaced atomically. Nothing is started.
func runServeInit(getenv func(string) string, opts serveInitOptions, stderr io.Writer) (serveInitOutcome, error) {
	if err := checkSupported("serve --init"); err != nil {
		return serveInitOutcome{}, err
	}
	if opts.port < 1 || opts.port > 65535 {
		return serveInitOutcome{}, errServeInitPort(opts.port)
	}
	var err error
	if opts.dir == "" {
		if opts.dir, err = serverDir(getenv); err != nil {
			return serveInitOutcome{}, err
		}
	}
	if opts.db == "" {
		if opts.db, err = localDBPath(getenv); err != nil {
			return serveInitOutcome{}, err
		}
	}
	if opts.dir, err = absSafe("the server directory", opts.dir); err != nil {
		return serveInitOutcome{}, err
	}
	if opts.db, err = absSafe("the database path", opts.db); err != nil {
		return serveInitOutcome{}, err
	}

	out := serveInitOutcome{
		Dir:      opts.dir,
		KeyFile:  filepath.Join(opts.dir, keyFileName),
		EnvFile:  filepath.Join(opts.dir, serveEnvName),
		Database: opts.db,
		Listen:   net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.port)),
	}
	if filepath.Dir(opts.db) == string(filepath.Separator) {
		return serveInitOutcome{}, errServeInitDB(opts.db, "is directly under the filesystem root, which compose would mount as /data")
	}
	for _, name := range []string{keyFileName, serveEnvName, "compose.yaml"} {
		if opts.db == filepath.Join(opts.dir, name) {
			return serveInitOutcome{}, errServeInitDB(opts.db, "is one of the server files")
		}
	}
	if info, err := os.Stat(opts.db); err == nil && info.IsDir() {
		return serveInitOutcome{}, errServeInitDB(opts.db, "is a directory")
	}
	if !opts.force {
		for _, p := range []string{out.KeyFile, out.EnvFile} {
			if _, err := os.Lstat(p); err == nil {
				return serveInitOutcome{}, errServeInitExists(p)
			} else if !errors.Is(err, os.ErrNotExist) {
				return serveInitOutcome{}, errServeInitWrite(p, err)
			}
		}
	}
	for _, d := range []string{opts.dir, filepath.Dir(opts.db)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return serveInitOutcome{}, errServeInitWrite(d, err)
		}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return serveInitOutcome{}, errServeInitKey(err)
	}
	key := hex.EncodeToString(raw)
	env := fmt.Sprintf("%s\nAPI_KEY_FILE=%s\nDATABASE_PATH=%s\nHTTP_LISTEN_ADDR=%s\n",
		serveEnvComment, out.KeyFile, out.Database, out.Listen)

	// serve.env goes first: it does not depend on the key. Without force a
	// failed key write removes the new serve.env, leaving nothing behind;
	// with force the old key stays in place and the replaced serve.env names
	// the same files, so the pair still works.
	if err := placeServerFile(out.EnvFile, []byte(env), opts.force); err != nil {
		return serveInitOutcome{}, err
	}
	if err := placeServerFile(out.KeyFile, []byte(key+"\n"), opts.force); err != nil {
		if !opts.force {
			_ = os.Remove(out.EnvFile)
		}

		return serveInitOutcome{}, err
	}
	out.APIKey = key

	fmt.Fprintf(stderr, "agentfeedback serve --init: wrote %s and %s; nothing was started\n", out.KeyFile, out.EnvFile)
	fmt.Fprintf(stderr, "run the server in the foreground:\n  set -a; . %s; set +a; agentfeedback serve\n", out.EnvFile)
	fmt.Fprintf(stderr, "or install it as a service (writes one file, starts nothing):\n  agentfeedback server install --dir %s --systemd|--launchd|--compose\n", out.Dir)
	fmt.Fprintf(stderr, "point this machine's client at it:\n  agentfeedback doctor --init --url http://%s --key-from-stdin < %s\n", out.Listen, out.KeyFile)
	if opts.force {
		fmt.Fprintf(stderr, "a running server keeps the old key until it restarts; restart it with the command for its form:\n"+
			"  systemctl --user restart agentfeedback.service\n"+
			"  launchctl kickstart -k gui/$(id -u)/%s\n"+
			"  docker compose -f %s up -d --force-recreate\n", launchdLabel, filepath.Join(out.Dir, "compose.yaml"))
	}

	return out, nil
}

// placeFileHook runs before each server file is placed; tests make it fail.
var placeFileHook = func(string) error { return nil }

func placeServerFile(path string, data []byte, force bool) error {
	err := placeFileHook(path)
	if err == nil {
		err = placeFile(path, data, force)
	}
	switch {
	case errors.Is(err, os.ErrExist):
		return errServeInitExists(path)
	case err != nil:
		return errServeInitWrite(path, err)
	}

	return nil
}
