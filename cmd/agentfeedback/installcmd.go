package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

const (
	installSynopsis   = "install [all|<harness>...] [--server cloud|URL] [--mcp] [--with-reminder] [--docs] [--dry-run] [--list] [--json]"
	uninstallSynopsis = "uninstall all|<harness>... [--dry-run] [--json]"
)

// isTerminal reports whether stdin is a terminal; a variable so tests can
// stand in for one.
var isTerminal = func() bool {
	info, err := os.Stdin.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// installOutcome is the one JSON line install and uninstall end with.
type installOutcome struct {
	Status    string                  `json:"status"`
	Harnesses []harness.HarnessStatus `json:"harnesses,omitempty"`
	Changed   []string                `json:"changed,omitempty"`
	Backups   []string                `json:"backups,omitempty"`
	Next      []string                `json:"next,omitempty"`
	Message   string                  `json:"message,omitempty"`
}

func printInstallOutcome(stdout io.Writer, out installOutcome, err error) error {
	if err == nil {
		return writeJSON(stdout, out)
	}
	out.Status, out.Message = "error", err.Error()
	out.Harnesses, out.Next = nil, nil
	if werr := writeJSON(stdout, out); werr != nil {
		return werr
	}

	return &reportedError{err}
}

func harnessEnv() (harness.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return harness.Env{}, errNoHome(err)
	}

	return harness.Env{
		Home:     home,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		Exec: func(ctx context.Context, setenv map[string]string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = withEnv(os.Environ(), setenv)

			return cmd.CombinedOutput()
		},
	}, nil
}

// withEnv is environ with the setenv overrides applied; an empty value
// removes the variable.
func withEnv(environ []string, setenv map[string]string) []string {
	out := make([]string, 0, len(environ)+len(setenv))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := setenv[name]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range setenv {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}

	return out
}

// afterManifestRead, when set, runs once the command has read the manifest
// (server resolution, uninstall all) and before the run; tests use it to
// check the lock is already held.
var afterManifestRead func()

// installErr turns a harness error into the sentence a person reads.
func installErr(err error) error {
	var r *harness.Refusal
	var fe *harness.FileError
	switch {
	case errors.As(err, &r):
		return failErr(r.Problem, r.Next)
	case errors.As(err, &fe):
		return failErr(fmt.Sprintf("cannot update %s: %s", fe.Path, oneLine(fe.Err.Error())),
			"fix or restore that file, then run the command again")
	}

	return failErr(oneLine(err.Error()), "fix the cause, then run the command again")
}

// runInstall wires the harnesses, or with no harness named lists them.
func runInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("install")
	server := fs.String("server", "", "server the MCP entries point at: cloud or a base URL (default: url in config.toml, then the one recorded at the last install)")
	mcp := fs.Bool("mcp", false, "add an MCP entry instead of the skill and the Stop hook")
	docs := fs.Bool("docs", false, "also install the agentfeedback-docs skill (reference docs for integrators and operators)")
	reminder := fs.Bool("with-reminder", false, "also add a session-start hook that prints a one-line reminder, where the harness supports one")
	dryRun := fs.Bool("dry-run", false, "print what would change and change nothing")
	list := fs.Bool("list", false, "list the harnesses and how they are wired, and change nothing")
	asJSON := fs.Bool("json", false, "with the list: print one JSON object instead of the table")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\nharnesses: %s\n", installSynopsis, strings.Join(harness.Names(), ", "))
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		err = errFlags("install", err)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}

		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if err := checkInstallSupported("install"); err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if err := checkHarnessNames(pos); err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	env, err := harnessEnv()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if len(pos) == 0 || *list {
		return listHarnesses(env, *asJSON, stdout)
	}

	// The lock is held from before the manifest is first read to the end
	// of the run.
	unlock, err := harness.Lock(env, stderr, "install")
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, installErr(err))
	}
	defer unlock()
	names := pos
	if slices.Contains(pos, "all") {
		// all is every detected harness plus the ones named beside it.
		names = nil
		for _, n := range harness.Names() {
			if env.Detect(n) != "no" || slices.Contains(pos, n) {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			return printInstallOutcome(stdout, installOutcome{}, errInstallNoneDetected())
		}
	}
	names = dedupe(names)
	// Detection is read before the run, which may create a harness's
	// directory.
	undetected := map[string]bool{}
	for _, n := range names {
		undetected[n] = env.Detect(n) == "no"
	}

	bin, err := binaryPath()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	srv, configExists, err := resolveInstallServer(os.Getenv, env, *server, stdin, stderr)
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	mode := harness.ModeCLI
	if *mcp {
		mode = harness.ModeMCP
		if os.Getenv(envAPIKey) == "" {
			fmt.Fprintf(stderr, "agentfeedback install: warning: %s is not set here; the harnesses read the key from it, so set it in the environment they start from\n", envAPIKey)
		}
	}

	if afterManifestRead != nil {
		afterManifestRead()
	}
	res, err := env.Run(harness.Request{
		Harnesses: names,
		Options:   harness.Options{Mode: mode, Reminder: *reminder, Docs: *docs, Server: srv, Binary: bin},
		DryRun:    *dryRun,
	})
	out := installOutcome{Status: res.Status, Harnesses: res.Harnesses, Changed: res.Changed, Backups: res.Backups}
	if err != nil {
		return printInstallOutcome(stdout, out, installErr(err))
	}
	for i := range out.Harnesses {
		if undetected[out.Harnesses[i].Name] {
			out.Harnesses[i].Notes = append(out.Harnesses[i].Notes, out.Harnesses[i].Name+" was not detected on this machine; it was wired anyway")
		}
		if *dryRun {
			for _, c := range res.Commands {
				if out.Harnesses[i].Name == "claude-code" {
					out.Harnesses[i].Notes = append(out.Harnesses[i].Notes, "would run: "+c)
				}
			}
		}
	}
	if !configExists {
		out.Next = append(out.Next, `printf '%s' "$KEY" | agentfeedback doctor --init --url `+srv+` --key-from-stdin`)
	}
	report(stderr, "install", out, res.Commands, *dryRun)

	return printInstallOutcome(stdout, out, nil)
}

// runUninstall removes what the manifest records for the named harnesses.
func runUninstall(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("uninstall")
	dryRun := fs.Bool("dry-run", false, "print what would change and change nothing")
	_ = fs.Bool("json", false, "accepted for symmetry with install; the outcome is always the last stdout line")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\nharnesses: %s\n", uninstallSynopsis, strings.Join(harness.Names(), ", "))
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		err = errFlags("uninstall", err)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}

		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if err := checkInstallSupported("uninstall"); err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if len(pos) == 0 {
		return printInstallOutcome(stdout, installOutcome{}, errArgs("uninstall", uninstallSynopsis))
	}
	if err := checkHarnessNames(pos); err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	env, err := harnessEnv()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	unlock, err := harness.Lock(env, stderr, "uninstall")
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, installErr(err))
	}
	defer unlock()
	names := pos
	if slices.Contains(pos, "all") {
		if names, err = env.Recorded(); err != nil {
			return printInstallOutcome(stdout, installOutcome{}, installErr(err))
		}
	}
	names = dedupe(names)
	if afterManifestRead != nil {
		afterManifestRead()
	}
	res, err := env.Run(harness.Request{Harnesses: names, Uninstall: true, DryRun: *dryRun})
	out := installOutcome{Status: res.Status, Harnesses: res.Harnesses, Changed: res.Changed, Backups: res.Backups}
	if err != nil {
		return printInstallOutcome(stdout, out, installErr(err))
	}
	if *dryRun {
		for i := range out.Harnesses {
			if out.Harnesses[i].Name == "claude-code" {
				for _, c := range res.Commands {
					out.Harnesses[i].Notes = append(out.Harnesses[i].Notes, "would run: "+c)
				}
			}
		}
	}
	if len(names) == 0 {
		out.Message = "the install manifest records no harness; nothing to remove"
	}
	report(stderr, "uninstall", out, res.Commands, *dryRun)

	return printInstallOutcome(stdout, out, nil)
}

func dedupe(names []string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}

	return out
}

func checkInstallSupported(command string) error {
	if goos == "windows" {
		return errInstallWindows(command)
	}

	return nil
}

func checkHarnessNames(names []string) error {
	for _, n := range names {
		if n != "all" && !harness.Known(n) {
			return errInstallHarness(n, harness.Names())
		}
	}

	return nil
}

// report prints the human-readable progress on stderr.
func report(stderr io.Writer, command string, out installOutcome, commands []string, dryRun bool) {
	verb := "changed"
	if dryRun {
		verb = "would change"
	}
	prefix := "agentfeedback " + command + ": "
	if len(out.Changed) == 0 && len(commands) == 0 {
		fmt.Fprintln(stderr, prefix+"nothing to change")
	}
	for _, c := range out.Changed {
		fmt.Fprintf(stderr, "%s%s %s\n", prefix, verb, c)
	}
	for _, c := range commands {
		if dryRun {
			fmt.Fprintf(stderr, "%swould run %s\n", prefix, c)
		} else {
			fmt.Fprintf(stderr, "%sran %s\n", prefix, c)
		}
	}
	for _, b := range out.Backups {
		fmt.Fprintf(stderr, "%sbackup %s\n", prefix, b)
	}
	for _, h := range out.Harnesses {
		for _, n := range h.Notes {
			fmt.Fprintf(stderr, "%snote: %s: %s\n", prefix, h.Name, n)
		}
	}
	for _, n := range out.Next {
		fmt.Fprintf(stderr, "%snext: %s\n", prefix, n)
	}
}

// listHarnesses prints every known harness and how it is wired.
func listHarnesses(env harness.Env, asJSON bool, stdout io.Writer) error {
	st, err := env.Status()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, installErr(err))
	}
	if asJSON {
		return writeJSON(stdout, map[string][]harness.HarnessStatus{"harnesses": st})
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tDETECTED\tMODE\tSKILL\tMCP\tHOOK\tREMINDER\tDOCS")
	for _, h := range st {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, h.Detected, h.Mode, h.Skill, h.MCP, h.Hook, h.Reminder, h.Docs)
	}

	return tw.Flush()
}

// resolveInstallServer picks the server: --server, then config.toml's url,
// then the one the manifest records, then a prompt on a terminal. A
// --server that differs from config.toml's url is refused: install never
// changes the configured server.
func resolveInstallServer(getenv func(string) string, env harness.Env, flagValue string, stdin io.Reader, stderr io.Writer) (srv string, configExists bool, err error) {
	path, err := configPath(getenv)
	if err != nil {
		return "", false, err
	}
	file, configExists, err := loadFileConfig(path)
	if err != nil {
		return "", configExists, err
	}
	var configured string
	if file.URL != "" {
		if configured, err = normaliseServer("the url in "+path, file.URL); err != nil {
			return "", configExists, err
		}
	}
	if flagValue != "" {
		if srv, err = normaliseServer("--server", flagValue); err != nil {
			return "", configExists, err
		}
		if configured != "" && configured != srv {
			return "", configExists, errInstallServerDiffers(srv, configured, path)
		}

		return srv, configExists, nil
	}
	if configured != "" {
		return configured, configExists, nil
	}
	recorded, err := env.Server()
	if err != nil {
		return "", configExists, installErr(err)
	}
	if recorded != "" {
		srv, err = normaliseServer("the server in the install manifest "+env.ManifestPath(), recorded)

		return srv, configExists, err
	}
	if !isTerminal() {
		return "", configExists, errInstallNoServer()
	}
	fmt.Fprint(stderr, `AgentFeedback server ("cloud" or a URL): `)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", configExists, errInstallNoServer()
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", configExists, errInstallNoServer()
	}
	srv, err = normaliseServer("the server", line)

	return srv, configExists, err
}

// normaliseServer maps cloud to its URL and puts every other server through
// skillgen.NormalizeServer, so the URL is safe to show in a shell command;
// source names where the value came from.
func normaliseServer(source, raw string) (string, error) {
	if raw == "cloud" {
		raw = cloudURL
	}
	out, err := skillgen.NormalizeServer(raw)
	var se *skillgen.ServerError
	if errors.As(err, &se) {
		return "", errInstallBadServer(source, raw, se.Reason)
	}

	return out, err
}
