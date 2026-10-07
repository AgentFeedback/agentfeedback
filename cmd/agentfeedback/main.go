// Command agentfeedback is the AgentFeedback server and client: serve runs the
// API (serve --init and server install set it up), import and backup maintain
// its database, and doctor, submit (friction,
// review or any kind), flush, version, schema, skill and the read and
// processing commands (list, get, stats, done, undo, redact, rekind, export,
// digest) and migrate are client commands; install and uninstall wire the
// client into the coding-agent harnesses. A bare invocation prints help.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// command is one entry of the router: the name typed after agentfeedback, the
// line help shows, and the handler that receives the remaining arguments.
type command struct {
	name    string
	summary string
	run     func(args []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// commands is the command table in help order. Nothing listens unless serve
// is named: a bare invocation prints help.
var commands = []command{
	{"serve", "serve the HTTP API | " + serveInitSynopsis + ": write the server's key file and serve.env, start nothing", serveCommand},
	{"server", serverSynopsis + ": write a service definition from serve.env, start nothing", runServer},
	{"import", "import [--dry-run] <export.ndjson>: restore an export (format 2) into the database, keeping ids", runImport},
	{"backup", "backup <dest.db>: write a consistent copy of the database", runBackup},
	{"doctor", "check the client setup: the mode (local database or server), the connection and the versions; doctor --e2e [--json]: submit, list and mark one install-check row", runDoctor},
	{"submit", "submit friction --summary S [...] | submit <kind> --stdin [...] | submit review <run_dir> [--include-outputs] | submit review --sweep [<base>...]; --dry-run sends nothing, --scrub redacts known secret formats in the body's string values: file a submission, the outcome is the last line", runSubmit},
	{"flush", "flush [--hook]: send the spooled submissions that are due and print the counts; --hook prints nothing, stops after 5 s and always exits 0", runFlush},
	{"list", "list [filters] [--limit N] [--before-id N | --after-id N] [--include payload] [--all] [--json | --tsv]: list submissions, newest first", runList},
	{"get", "get <id> [--json]: print one submission", runGet},
	{"stats", "stats [filters] [--by a,b] [--top N] [--bucket day|week] [--json]: print aggregates", runStats},
	{"done", "done <id>... --verdict V [--resolution R] [--ref REF] [--processed-by P]: mark submissions processed", runDone},
	{"undo", "undo <id>...: clear the processing mark", runUndo},
	{"redact", "redact <id>: replace a submission with its tombstone (not reversible)", runRedact},
	{"rekind", "rekind <id> <kind>: file a submission again under another kind and mark the original duplicate", runRekind},
	{"export", "export [--after-id N] [--kind K] [--since T] [--limit N]: stream an export (format 2) and verify its trailer", runExport},
	{"migrate", "migrate --to cloud|URL --to-key-from-stdin [--kind K] [--include-kind K]... [--since T] [--limit N] [--dry-run]: copy the records to another server through its import route", runMigrate},
	{"digest", "digest [--out DIR] [--kind K] [--include-kind K]: pull the open queue into a triage directory", runDigest},
	{"version", "print the client version", runVersion},
	{"schema", "schema [<kind> [<version>]]: list the schemas or print one", runSchema},
	{"skill", "skill render <form> [--server URL]: print the submission guidance in one form | skill render docs --out DIR: write the agentfeedback-docs skill | skill render agent-plugin|marketplace --out DIR: write the plugin bundle or a marketplace root | skill reminder: print the one-line session-start reminder", runSkill},
	{"install", installSynopsis + ": wire the agentfeedback skill and Stop hook (or with --mcp an MCP entry) into the coding-agent harnesses; no harness named lists them", runInstall},
	{"uninstall", uninstallSynopsis + ": remove exactly what install added, restoring untouched files from their backups", runUninstall},
	{"help", "print this help", nil},
}

const helpFooter = `
Add --json to help, doctor, version, schema, list, get or stats for JSON output.
First-time setup: printf '%%s' "$KEY" | agentfeedback doctor --init --url URL --key-from-stdin

Local mode: with no server URL configured, every client command works against the data directory
  ${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db with no server; --local forces it, --server URL targets a server once.
client environment (flag > environment > config file):
  AGENT_FEEDBACK_URL       server base URL (doctor --url overrides it)
  AGENT_FEEDBACK_API_KEY   API key (environment or config file only, never a flag)
  AGENT_FEEDBACK_MACHINE   machine name
  AGENT_FEEDBACK_MODEL     model id
  AGENT_FEEDBACK_HARNESS   harness name
client config file: %s
  flat keys: url, api_key, machine, model, harness
  tables: [collect] deny_paths, opt_in_only, opt_in_paths, disabled; [context] cwd, drop, app, workspace, url, channel, task_id, workflow
  a repository .agentfeedback.toml may only narrow (collect.disabled, collect.deny_paths, context.drop)

server environment: API_KEY or API_KEY_FILE (serve only, not both), DATABASE_PATH,
HTTP_LISTEN_ADDR, GRACEFUL_SHUTDOWN_TIMEOUT, SERVICE_VERSION, LOG_LEVEL,
INGEST_SCRUB (on|off, default off) (defaults: the data-directory database, 127.0.0.1:8090)
First server setup: agentfeedback serve --init, then agentfeedback server install --systemd|--launchd|--compose
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run routes one invocation and returns the exit status: 0 on success, 2 for
// a wrong command line, 1 for any other failure.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)

		return 0
	}
	name := args[0]
	if name == "--json" && len(args) == 1 {
		name = "help"
	}
	switch name {
	case "help", "-h", "--help":
		if slices.Contains(args, "--json") {
			if err := writeJSON(stdout, helpJSON()); err != nil {
				return 1
			}

			return 0
		}
		printHelp(stdout)

		return 0
	}

	if strings.HasPrefix(name, "-") {
		fmt.Fprintf(stderr, "agentfeedback: %v\n", errLeadingFlag(name))

		return 2
	}

	var cmd *command
	for i := range commands {
		if commands[i].name == name && commands[i].run != nil {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Fprintf(stderr, "agentfeedback: %v\n\n", errUnknownCommand(name))
		printHelp(stderr)

		return 2
	}

	skipStartupPass = false
	err := cmd.run(args[1:], stdin, stdout, stderr)
	closeLocal()
	if err != nil {
		if cache, cerr := cacheDir(os.Getenv); cerr == nil {
			logSchemaTooNew(cache, err)
		}
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errReported):
		return exitCode(err)
	}
	fmt.Fprintf(stderr, "agentfeedback %s: %v\n", name, err)

	return exitCode(err)
}

// errReported marks a failure the command already printed in its own format
// (doctor's report, doctor --init's JSON outcome); run adds nothing.
var errReported = errors.New("reported")

// reportedError wraps a failure already printed so run keeps its exit status.
type reportedError struct{ err error }

func (e *reportedError) Error() string   { return e.err.Error() }
func (e *reportedError) Unwrap() []error { return []error{e.err, errReported} }

func printHelp(w io.Writer) {
	fmt.Fprint(w, "usage: agentfeedback <command> [arguments]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	path, err := configPath(os.Getenv)
	if err != nil {
		path = "~/.config/agentfeedback/config.toml"
	}
	fmt.Fprintf(w, helpFooter, path)
}

type helpCommand struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

func helpJSON() map[string][]helpCommand {
	out := make([]helpCommand, 0, len(commands))
	for _, c := range commands {
		out = append(out, helpCommand{Name: c.name, Summary: c.summary})
	}

	return map[string][]helpCommand{"commands": out}
}

// newFlagSet returns the flag set every command parses with: errors are
// returned, not fatal, and the flag package prints nothing itself, so run
// reports each error once.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("agentfeedback "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	return fs
}

// parseFlags parses args and, on -h, prints the command's usage to stderr.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(stderr)
		fs.Usage()
		fs.SetOutput(io.Discard)
	}

	return err
}

// serveCommand runs the server, configured by the environment alone, or with
// --init writes its files. Checking the command line first means a mistyped
// one never starts a listener.
func serveCommand(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("serve")
	doInit := fs.Bool("init", false, "write the server's API key file and serve.env, then exit; serve itself takes no flags")
	dir := fs.String("dir", "", "with --init: directory for api-key and serve.env (default ${XDG_CONFIG_HOME:-~/.config}/agentfeedback/server)")
	db := fs.String("db", "", "with --init: database path (default ${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db)")
	port := fs.Int("port", defaultServePort, "with --init: port to listen on, on 127.0.0.1")
	force := fs.Bool("force", false, "with --init: replace existing files, which rotates the API key")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback serve (listens on HTTP_LISTEN_ADDR, default 127.0.0.1:8090; database DATABASE_PATH, default ${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db) | %s\n", serveInitSynopsis)
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args, stderr); err != nil {
		err = errFlags("serve", err)
		if slices.ContainsFunc(args, isInitArg) && !errors.Is(err, flag.ErrHelp) {
			return printServeInitOutcome(stdout, serveInitOutcome{}, err)
		}

		return err
	}
	if *doInit {
		if fs.NArg() != 0 {
			return printServeInitOutcome(stdout, serveInitOutcome{}, errArgs("serve", serveInitSynopsis))
		}
		out, err := runServeInit(os.Getenv, serveInitOptions{dir: *dir, db: *db, port: *port, force: *force}, stderr)

		return printServeInitOutcome(stdout, out, err)
	}
	if fs.NFlag() != 0 {
		return errServeInitOnlyFlags()
	}
	if fs.NArg() != 0 {
		return errArgs("serve", "serve (configured by the server environment variables)")
	}

	return runServe()
}

func setupLogger(level string) {
	lvl := slog.LevelInfo
	if level == "debug" {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}

func runServe() error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	svc := core.New(db, core.Config{Version: cfg.ServiceVersion, Features: api.Features, ScrubIngest: cfg.ScrubIngest})

	var shuttingDown atomic.Bool
	srv := api.New(api.Config{
		Service:         svc,
		DB:              db,
		APIKey:          cfg.APIKey,
		ShuttingDown:    &shuttingDown,
		PublicURL:       cfg.PublicURL,
		MCPInstructions: cfg.MCPInstructions,
	})

	httpSrv := &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	slog.Info("service is up",
		"version", cfg.ServiceVersion, "addr", cfg.HTTPListenAddr, "database", cfg.DatabasePath)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server failed: %w", err)
		}

		return nil
	case sig := <-stop:
		slog.Info("received signal, shutting down", "signal", sig.String())
	}

	// /ready starts failing immediately: a load balancer stops sending new
	// requests while the in-flight ones drain.
	shuttingDown.Store(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server did not shut down gracefully", "error", err, "timeout", cfg.ShutdownTimeout)
	}
	slog.Info("service shut down")

	return nil
}

func runBackup(args []string, _ io.Reader, stdout, _ io.Writer) error {
	if len(args) != 1 {
		return errArgs("backup", "backup <dest.db>")
	}
	dest := args[0]

	cfg, err := loadConfig(false)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	// Refuse rather than overwrite: a backup command that clobbers an existing
	// backup is a data-loss command.
	if _, err := os.Stat(dest); err == nil {
		return errBackupExists(dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return errBackupStat(dest, err)
	}

	// store.Open would create an empty database and back that up.
	if _, err := os.Stat(cfg.DatabasePath); errors.Is(err, os.ErrNotExist) {
		return errBackupNoSource(cfg.DatabasePath)
	} else if err != nil {
		return errPathUnreadable(cfg.DatabasePath, err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.VacuumInto(ctx, dest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backed up %s to %s\n", cfg.DatabasePath, dest)

	return nil
}
