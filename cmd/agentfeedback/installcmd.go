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
	installSynopsis   = "install [all|<harness>...] [--server local|cloud|URL] [--mcp] [--with-reminder=false] [--no-hooks] [--docs] [--dry-run] [--list] [--json] | install --check [all|<harness>...] [--project] [--json] | install --project [--yes] [--uninstall] [--dry-run]"
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
	Warnings  []string                `json:"warnings,omitempty"`
	// Manual is the wiring by hand of each harness, where install cannot
	// run (Windows).
	Manual  []manualStep `json:"manual,omitempty"`
	Message string       `json:"message,omitempty"`
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
		Version:  binaryVersion,
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
	server := fs.String("server", "", "local, cloud or a base URL (default: url in config.toml, then the one recorded at the last install, then a prompt, then local); a URL MCP entry points at it")
	mcp := fs.Bool("mcp", false, "add an MCP entry instead of the skill and the hooks: for local, a stdio entry that runs this binary's mcp command; for a server, a URL entry")
	docs := fs.Bool("docs", false, "also install the agentfeedback-docs skill (reference docs for integrators and operators)")
	reminder := fs.Bool("with-reminder", true, "add a session-start hook that runs agentfeedback prime, where the harness supports one; --with-reminder=false leaves it out")
	noHooks := fs.Bool("no-hooks", false, "wire no hooks (neither the failure note and end-of-turn flush nor the session-start reminder); the skill and the rule only")
	dryRun := fs.Bool("dry-run", false, "print what would change and change nothing")
	list := fs.Bool("list", false, "list the harnesses and how they are wired, and change nothing")
	asJSON := fs.Bool("json", false, "with the list or --check: print one JSON object instead of the table")
	check := fs.Bool("check", false, "report whether each harness's global instruction file holds the current rule section (no names: the detected and recorded harnesses); changes nothing, exits 3 when one is missing or stale")
	project := fs.Bool("project", false, "write the AgentFeedback pointer section into the repository's AGENTS.md (or CLAUDE.md when only that exists) instead of wiring harnesses; with --check, also report it")
	yes := fs.Bool("yes", false, "with --project: confirm without a prompt")
	uninstallProject := fs.Bool("uninstall", false, "with --project: remove the pointer section")
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
	if err := checkHarnessNames(pos); err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	reminderAsked := false
	fs.Visit(func(f *flag.Flag) { reminderAsked = reminderAsked || f.Name == "with-reminder" })
	if *project && (*mcp || (len(pos) > 0 && !*check)) {
		return printInstallOutcome(stdout, installOutcome{}, errProjectArgs())
	}
	if (*yes || *uninstallProject) && (!*project || *check) {
		return printInstallOutcome(stdout, installOutcome{}, usageErr("--yes and --uninstall apply only to install --project", "run agentfeedback install --project --yes or agentfeedback install --project --uninstall"))
	}
	if *project && !*check {
		return runInstallProject(*uninstallProject, *yes, *dryRun, stdin, stdout, stderr)
	}
	if err := checkInstallSupported("install"); err != nil {
		steps, warnings := manualSteps("install", pos, *server, stderr)

		return printInstallOutcome(stdout, installOutcome{Manual: steps, Warnings: warnings}, err)
	}
	env, err := harnessEnv()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if *check {
		return runInstallCheck(env, pos, *project, *asJSON, stdout)
	}
	if len(pos) == 0 || *list {
		return listHarnesses(env, *asJSON, stdout)
	}

	out, err := wireHarnesses(env, pos, wireOptions{mcp: *mcp, docs: *docs, reminder: *reminder, reminderAsked: reminderAsked, noHooks: *noHooks, dryRun: *dryRun},
		func() (string, bool, error) { return resolveInstallServer(os.Getenv, env, *server, stdin, stderr) }, "install", stderr)

	return printInstallOutcome(stdout, out, err)
}

// wireOptions are the choices of one install run.
type wireOptions struct {
	mcp, docs, reminder, reminderAsked, noHooks, dryRun bool
}

// wireHarnesses wires the harnesses pos names (all: every detected one, plus
// the ones named beside it) and reports on stderr under command. resolve
// picks the server; it runs under the lock, after the harnesses are known.
// The outcome holds what the run did even when err is set.
func wireHarnesses(env harness.Env, pos []string, o wireOptions, resolve func() (string, bool, error), command string, stderr io.Writer) (installOutcome, error) {
	// The lock is held from before the manifest is first read to the end
	// of the run.
	unlock, err := harness.Lock(env, stderr, command)
	if err != nil {
		return installOutcome{}, installErr(err)
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
			return installOutcome{}, errInstallNoneDetected()
		}
	}
	names = dedupe(names)
	mode := harness.ModeCLI
	if o.mcp {
		mode = harness.ModeMCP
	}
	// Under all, a detected harness that cannot be wired in this mode is
	// left out with a note; one named beside all is refused by the run.
	var skipped []harness.HarnessStatus
	if slices.Contains(pos, "all") {
		kept := names[:0:0]
		for _, n := range names {
			if ok, reason := harness.Supports(n, mode); !ok && !slices.Contains(pos, n) {
				skipped = append(skipped, harness.HarnessStatus{Name: n, Mode: "-", Skill: "-", MCP: "-", Hook: "-", Reminder: "-", Docs: "-", Rule: "-",
					Notes: []string{reason + "; it was left out of " + command + " all"}})

				continue
			}
			kept = append(kept, n)
		}
		names = kept
		if len(names) == 0 {
			return installOutcome{}, errInstallNoneSupported(skipped, mode)
		}
	}
	// Detection is read before the run, which may create a harness's
	// directory.
	undetected := map[string]bool{}
	for _, n := range names {
		undetected[n] = env.Detect(n) == "no"
	}

	bin, err := binaryPath()
	if err != nil {
		return installOutcome{}, err
	}
	warnings := pathWarnings(bin)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "agentfeedback %s: warning: %s\n", command, w)
	}
	srv, configExists, err := resolve()
	if err != nil {
		return installOutcome{}, err
	}
	if o.mcp && srv != serverLocal {
		if os.Getenv(envAPIKey) == "" {
			fmt.Fprintf(stderr, "agentfeedback %s: warning: %s is not set here; the harnesses read the key from it, so set it in the environment they start from\n", command, envAPIKey)
		}
	}

	if afterManifestRead != nil {
		afterManifestRead()
	}
	res, err := env.Run(harness.Request{
		Harnesses: names,
		Options:   harness.Options{Mode: mode, Reminder: o.reminder, ReminderAsked: o.reminderAsked, NoHooks: o.noHooks, Docs: o.docs, Server: srv, Binary: bin},
		DryRun:    o.dryRun,
	})
	out := installOutcome{Status: res.Status, Harnesses: res.Harnesses, Changed: res.Changed, Backups: res.Backups, Warnings: warnings}
	if err != nil {
		return out, installErr(err)
	}
	for i := range out.Harnesses {
		if undetected[out.Harnesses[i].Name] {
			out.Harnesses[i].Notes = append(out.Harnesses[i].Notes, out.Harnesses[i].Name+" was not detected on this machine; it was wired anyway")
		}
		if o.dryRun {
			for _, c := range res.Commands {
				if out.Harnesses[i].Name == "claude-code" {
					out.Harnesses[i].Notes = append(out.Harnesses[i].Notes, "would run: "+c)
				}
			}
		}
	}
	out.Harnesses = append(out.Harnesses, skipped...)
	if !configExists && srv != serverLocal {
		out.Next = append(out.Next, `printf '%s' "$KEY" | agentfeedback doctor --init --url `+srv+` --key-from-stdin`)
	}
	report(stderr, command, out, res.Commands, o.dryRun)

	return out, nil
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
	dash := func(s string) string {
		if s == "" {
			return "-"
		}

		return s
	}
	fmt.Fprintln(tw, "HARNESS\tDETECTED\tMODE\tSKILL\tMCP\tHOOK\tREMINDER\tDOCS\tRULE\tVERIFIED\tBINARY\tVERSION")
	for _, h := range st {
		verified := "-"
		if v := h.Verification; v != nil {
			verified = v.Level + " " + v.Date
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, h.Detected, h.Mode, h.Skill, h.MCP, h.Hook, h.Reminder, h.Docs, h.Rule,
			verified, dash(h.Binary), dash(h.BinaryVersion))
	}

	return tw.Flush()
}

// resolveInstallServer picks the server: --server, then config.toml's url,
// then the one the manifest records, then a prompt on a terminal, then
// local mode ("local"; also an empty answer at the prompt). A
// --server that differs from config.toml's url is refused: install never
// changes the configured server.
func resolveInstallServer(getenv func(string) string, env harness.Env, flagValue string, stdin io.Reader, stderr io.Writer) (srv string, configExists bool, err error) {
	srv, configExists, err = installServerNoPrompt(getenv, env, flagValue)
	if err != nil || srv != "" {
		return srv, configExists, err
	}
	if !isTerminal() {
		return serverLocal, configExists, nil
	}
	fmt.Fprint(stderr, `AgentFeedback server ("local", "cloud" or a URL): `)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", configExists, errInstallNoServer()
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return serverLocal, configExists, nil
	}
	srv, err = normaliseServer("the server", line)

	return srv, configExists, err
}

// installServerNoPrompt is resolveInstallServer up to the prompt: --server,
// then config.toml's url, then the one the manifest records; "" when none
// is set. It never reads stdin.
func installServerNoPrompt(getenv func(string) string, env harness.Env, flagValue string) (srv string, configExists bool, err error) {
	path, err := configPath(getenv)
	if err != nil {
		return "", false, err
	}
	file, configExists, err := loadFileConfig(path)
	if err != nil {
		return "", configExists, err
	}
	// The configured server is what every command resolves: the environment
	// over the config file. Install never changes it, and never records
	// local beside it.
	var configured, source string
	if u := resolveClient(flagConfig{}, getenv, file).URL; u.Value != "" {
		source = "the url in " + path
		if u.Source == sourceEnv {
			source = envURL
		}
		if configured, err = normaliseServer(source, u.Value); err != nil {
			return "", configExists, err
		}
	}
	if flagValue != "" {
		if srv, err = normaliseServer("--server", flagValue); err != nil {
			return "", configExists, err
		}
		if configured != "" && configured != srv {
			return "", configExists, errInstallServerDiffers(srv, configured, source)
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

	return "", configExists, nil
}

// normaliseServer maps cloud to its URL and puts every other server through
// skillgen.NormalizeServer, so the URL is safe to show in a shell command;
// source names where the value came from.
func normaliseServer(source, raw string) (string, error) {
	if raw == serverLocal {
		return serverLocal, nil
	}
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

// serverLocal is the server value of local mode: no URL, the data-directory
// database. The manifest records it as is.
const serverLocal = harness.ServerLocal

func errProjectArgs() error {
	return usageErr("install --project takes no harness name and no --mcp", "run agentfeedback install --project from inside the repository")
}

// errInstallNoneSupported is install all when every detected harness was
// left out because it cannot be wired in mode.
func errInstallNoneSupported(skipped []harness.HarnessStatus, mode string) error {
	with, other, flag := "without --mcp", harness.ModeMCP, "--mcp"
	if mode == harness.ModeMCP {
		with, other, flag = "with --mcp", harness.ModeCLI, "no --mcp"
	}
	var parts []string
	next := "name a harness that can be, or run agentfeedback install all with " + flag
	for _, h := range skipped {
		_, reason := harness.Supports(h.Name, mode)
		parts = append(parts, h.Name+": "+reason)
		if ok, _ := harness.Supports(h.Name, other); ok && mode == harness.ModeCLI {
			next = "run agentfeedback install " + h.Name + " --mcp"
		} else if ok && mode == harness.ModeMCP {
			next = "run agentfeedback install " + h.Name + " without --mcp"
		}
	}

	return usageErr("no detected harness can be wired "+with+": "+strings.Join(parts, "; "), next)
}
