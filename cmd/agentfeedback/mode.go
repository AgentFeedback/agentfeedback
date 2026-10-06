package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// Mode names as doctor reports them.
const (
	modeLocal  = "local"
	modeRemote = "remote"
)

// modeFlags are the per-invocation overrides every client command takes:
// --local forces local mode, --server URL targets that server. Neither is
// written anywhere.
type modeFlags struct {
	local  bool
	server string
}

// addModeFlags registers --local and --server on fs.
func addModeFlags(fs *flag.FlagSet) *modeFlags {
	mf := &modeFlags{}
	fs.BoolVar(&mf.local, "local", false, "use the local database for this invocation even when a server URL is configured")
	fs.StringVar(&mf.server, "server", "", "server base URL for this invocation (overrides AGENT_FEEDBACK_URL and the config file)")

	return mf
}

// clientMode is where a command's requests go: the local database or a
// server. Settings carry the resolved values either way; in local mode
// Settings.URL is empty when nothing is configured and otherwise names the
// server --local set aside.
type clientMode struct {
	Mode     string
	Database string
	Settings clientSettings
}

// resolveMode decides the mode from the config file and getenv; see
// resolveModeFrom.
func resolveMode(mf modeFlags, getenv func(string) string) (clientMode, error) {
	path, err := configPath(getenv)
	if err != nil {
		return clientMode{}, err
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		return clientMode{}, err
	}

	return resolveModeFrom(mf, getenv, file)
}

// resolveModeFrom decides the mode over an already loaded config file.
// --local and --server together is an error. --local means local. Otherwise
// a URL from --server, the environment or the config file means remote; no
// URL at all means local. A configured URL is therefore never ignored
// without --local, and nothing configured never reaches a server.
func resolveModeFrom(mf modeFlags, getenv func(string) string, file fileConfig) (clientMode, error) {
	if mf.local && mf.server != "" {
		return clientMode{}, errModeBoth()
	}
	m := clientMode{Mode: modeRemote, Settings: resolveClient(flagConfig{URL: mf.server}, getenv, file)}
	if mf.local || m.Settings.URL.Value == "" {
		m.Mode = modeLocal
		var err error
		if m.Database, err = localDBPath(getenv); err != nil {
			return clientMode{}, err
		}
	}

	return m, nil
}

// localTarget is the open local database of this invocation; run closes it.
var localTarget *localmode.Target

// skipStartupPass is set by the commands that must not run the start-up
// pass: flush, which is the pass, and doctor, which changes nothing. run
// resets it.
var skipStartupPass bool

// closeLocal closes the local target opened by this invocation, if any.
func closeLocal() {
	if localTarget != nil {
		_ = localTarget.Close()
		localTarget = nil
	}
}

// openLocalClient opens the local database, runs the start-up pass over it
// and builds the client over it. The in-process handler logs through the
// default logger, so that logger is pointed at stderr for errors only: an
// access log line per command would be noise, an internal error is not.
func openLocalClient(m clientMode, getenv func(string) string, stderr io.Writer) (*client.Client, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	cache, err := cacheDir(getenv)
	if err != nil {
		return nil, err
	}
	data, err := dataDir(getenv)
	if err != nil {
		return nil, err
	}
	opened := false
	if localTarget == nil {
		t, err := localmode.Open(context.Background(), m.Database, clientVersion().Version, stderr)
		if err != nil {
			return nil, errLocalOpen(m.Database, err)
		}
		localTarget = t
		opened = true
	}
	cfg := client.Config{
		Transport: localTarget.Transport(),
		APIKey:    localmode.Key,
		DataDir:   data,
		CacheDir:  cache,
		Now:       nowFunc,
		Stderr:    stderr,
		Version:   clientVersion().Version,
	}
	if opened && !skipStartupPass {
		startupPass(cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, errClientSetup(err)
	}

	return c, nil
}

// startupPass is what every local-mode command does once, right after its
// database opens and before its own requests: deliver the spool entries
// bound to the local database that are due (a write that found the
// database busy or the disk full spooled them). Nothing is printed: a
// delivery logs a flushed line to client.jsonl, a quarantine or an expiry
// logs an error line, a failure leaves the entry for the next command, and
// entries bound to a server are left in place without a word (flush names
// them). Ordering is best effort: when the pass fails and the database
// frees up a moment later, the command's own write lands before the older
// entry, which the next pass delivers; refusing the write to keep the order
// would spool a report the database can take. This is the hook point for
// the file inbox: its ingestion joins here when it ships.
func startupPass(cfg client.Config) {
	cfg.Stderr = io.Discard
	c, err := client.New(cfg)
	if err != nil {
		return
	}
	if client.HasDue(cfg.DataDir, nowFunc()) {
		c.Flush(context.Background())
	}
}

// newAPIClient resolves the mode and builds the client for it: the local
// database, or the configured server with its key.
func newAPIClient(getenv func(string) string, mf modeFlags, stderr io.Writer) (*client.Client, clientMode, error) {
	m, err := resolveMode(mf, getenv)
	if err != nil {
		return nil, m, err
	}
	if m.Mode == modeLocal {
		c, err := openLocalClient(m, getenv, stderr)

		return c, m, err
	}
	if m.Settings.APIKey.Value == "" {
		return nil, m, errKeyUnset()
	}
	cache, err := cacheDir(getenv)
	if err != nil {
		return nil, m, err
	}
	data, err := dataDir(getenv)
	if err != nil {
		return nil, m, err
	}
	c, err := client.New(client.Config{
		URL:      m.Settings.URL.Value,
		APIKey:   m.Settings.APIKey.Value,
		DataDir:  data,
		CacheDir: cache,
		Now:      nowFunc,
		Stderr:   stderr,
		Version:  clientVersion().Version,
	})
	if err != nil {
		return nil, m, errClientSetup(err)
	}

	return c, m, nil
}

// apiClient is newAPIClient without the mode.
func apiClient(getenv func(string) string, mf modeFlags, stderr io.Writer) (*client.Client, error) {
	c, _, err := newAPIClient(getenv, mf, stderr)

	return c, err
}

func errModeBoth() error {
	return usageErr("--local and --server are both set", "pass one of them")
}

func errLocalOpen(path string, err error) error {
	return failErr(fmt.Sprintf("the local database %s cannot be opened: %s", path, oneLine(err.Error())),
		"check the data directory is writable, or set AGENT_FEEDBACK_URL to use a server")
}
