package main

import (
	"bufio"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/term"
)

const initSynopsis = "init [--local | --server cloud|URL [--key-from-stdin]] [--harnesses all|none|<harness>,...] [--yes] [--no-hooks] [--mcp] [--json]"

// initResult is what init did; with --json it is the one line printed.
type initResult struct {
	Status string `json:"status"`
	// Mode is local or remote, as doctor reports it.
	Mode   string `json:"mode,omitempty"`
	Server string `json:"server,omitempty"`
	// Config is the config file init wrote; empty when it wrote none.
	Config    string                  `json:"config,omitempty"`
	DataDir   string                  `json:"data_dir,omitempty"`
	Database  string                  `json:"database,omitempty"`
	Harnesses []harness.HarnessStatus `json:"harnesses,omitempty"`
	Changed   []string                `json:"changed,omitempty"`
	Backups   []string                `json:"backups,omitempty"`
	// Hooks says what each wired hook does.
	Hooks []string `json:"hooks,omitempty"`
	// Restart names the wired harnesses, which load the wiring at their
	// next start.
	Restart  []string     `json:"restart,omitempty"`
	E2E      []e2eStep    `json:"e2e,omitempty"`
	Next     []initNext   `json:"next,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
	Manual   []manualStep `json:"manual,omitempty"`
	Message  string       `json:"message,omitempty"`
}

// initNext is one command init suggests, with what it is for.
type initNext struct {
	Command string `json:"command"`
	Does    string `json:"does"`
}

// readSecret reads a line from the terminal without echoing it; a variable
// so tests can stand in for the terminal.
var readSecret = func() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))

	return string(b), err
}

// runInit sets up this machine in one pass: the target (local unless a
// server is named), the harnesses, the end-to-end check, and the next steps.
func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("init")
	local := fs.Bool("local", false, "report to the local database (the default when no server is configured)")
	server := fs.String("server", "", "report to this server: cloud or a base URL; init writes it to config.toml")
	keyFromStdin := fs.Bool("key-from-stdin", false, "with --server: read the server's API key from stdin and write it to config.toml")
	names := fs.String("harnesses", "", "harnesses to wire, comma-separated; all is every detected one, none wires nothing (default: ask on a terminal, else all detected)")
	yes := fs.Bool("yes", false, "ask nothing; take the defaults for what no flag sets")
	noHooks := fs.Bool("no-hooks", false, "wire no hooks: the skill and the rule only")
	mcp := fs.Bool("mcp", false, "add an MCP entry instead of the skill, the hooks and the rule, as install --mcp does")
	asJSON := fs.Bool("json", false, "print one JSON object instead of the summary")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\nharnesses: %s\n", initSynopsis, strings.Join(harness.Names(), ", "))
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err == nil && len(pos) > 0 {
		err = errArgs("init", initSynopsis)
	}
	if err != nil {
		err = errFlags("init", err)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}

		return finishInit(stdout, initResult{}, *asJSON, err)
	}
	switch {
	case *local && *server != "":
		err = usageErr("--local and --server are both set", "pass one of them")
	case *keyFromStdin && *server == "":
		err = usageErr("--key-from-stdin applies only with --server", "pass --server URL with --key-from-stdin")
	}
	if err != nil {
		return finishInit(stdout, initResult{}, *asJSON, err)
	}
	var wanted []string
	if *names != "" {
		if wanted, err = parseHarnessList(*names); err != nil {
			return finishInit(stdout, initResult{}, *asJSON, err)
		}
	}
	if err := checkInstallSupported("init"); err != nil {
		steps, warnings := manualSteps("init", slices.DeleteFunc(slices.Clone(wanted), func(n string) bool { return n == "all" || n == "none" }), *server, stderr)

		return finishInit(stdout, initResult{Manual: steps, Warnings: warnings}, *asJSON, err)
	}
	// Prompts read stdin only when it is a terminal and nothing else reads it.
	ask := isTerminal() && !*yes && !*keyFromStdin
	in := bufio.NewReader(stdin)
	env, err := harnessEnv()
	if err != nil {
		return finishInit(stdout, initResult{}, *asJSON, err)
	}

	res := initResult{}
	if res.DataDir, err = dataDir(os.Getenv); err != nil {
		return finishInit(stdout, res, *asJSON, err)
	}
	t, err := initTarget(env, *local, *server, *keyFromStdin, ask, in, stderr)
	if err != nil {
		return finishInit(stdout, res, *asJSON, err)
	}
	res.Mode, res.Server = modeRemote, t.server
	if t.server == serverLocal {
		res.Mode, res.Server = modeLocal, ""
		if res.Database, err = localDBPath(os.Getenv); err != nil {
			return finishInit(stdout, res, *asJSON, err)
		}
	}
	key := ""
	if t.write {
		switch {
		case *keyFromStdin:
			if key, err = readKey(stdin, "init --server URL --key-from-stdin"); err != nil {
				return finishInit(stdout, res, *asJSON, err)
			}
		case ask && os.Getenv(envAPIKey) == "":
			if key, err = askKey(t.server, stderr); err != nil {
				return finishInit(stdout, res, *asJSON, err)
			}
		}
	}

	detected := []string{}
	for _, n := range harness.Names() {
		if env.Detect(n) != "no" {
			detected = append(detected, n)
		}
	}
	if wanted == nil {
		wanted = []string{"all"}
		if ask {
			if wanted, err = askHarnesses(detected, in, stderr); err != nil {
				return finishInit(stdout, res, *asJSON, err)
			}
		}
	}
	wire := !slices.Contains(wanted, "none") && (len(detected) > 0 || !slices.Equal(wanted, []string{"all"}))

	if wire {
		out, err := wireHarnesses(env, wanted, wireOptions{mcp: *mcp, reminder: true, noHooks: *noHooks},
			func() (string, bool, error) { return t.server, true, nil }, "init", stderr)
		res.Harnesses, res.Changed, res.Backups, res.Warnings = out.Harnesses, out.Changed, out.Backups, out.Warnings
		if err != nil {
			return finishInit(stdout, res, *asJSON, err)
		}
	} else {
		bin, err := binaryPath()
		if err != nil {
			return finishInit(stdout, res, *asJSON, err)
		}
		res.Warnings = pathWarnings(bin)
		if !slices.Contains(wanted, "none") {
			res.Warnings = append(res.Warnings, "no harness was detected on this machine, so none was wired; name one with agentfeedback init --harnesses <harness>")
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "agentfeedback init: warning: %s\n", w)
		}
	}
	// The configuration is written once the wiring went through, so a
	// refused run leaves none behind.
	if t.write {
		if res.Config, err = writeInitConfig(t.server, key); err != nil {
			return finishInit(stdout, res, *asJSON, err)
		}
	}
	res.Hooks = hookDescriptions(res.Harnesses, t.server == serverLocal)
	for _, h := range res.Harnesses {
		if h.Mode != "-" {
			res.Restart = append(res.Restart, h.Name)
		}
	}

	mf := modeFlags{local: t.server == serverLocal}
	noKey := false
	if t.server != serverLocal {
		m, err := resolveMode(mf, os.Getenv)
		if err != nil {
			return finishInit(stdout, res, *asJSON, err)
		}
		noKey = m.Settings.APIKey.Value == ""
	}
	if noKey {
		res.E2E = []e2eStep{{Step: "e2e", Outcome: "skipped", Message: "no API key for " + t.server + "; the check runs once the key is set"}}
	} else if err := doctorE2E(os.Getenv, mf, stderr, func(s e2eStep) error {
		res.E2E = append(res.E2E, s)

		return nil
	}); err != nil {
		// The failed step's message is the one doctor --e2e prints.
		if n := len(res.E2E); n > 0 {
			res.Message = res.E2E[n-1].Message
		}

		return finishInit(stdout, res, *asJSON, err)
	}
	res.Next = initNextSteps(res, t, noKey)
	res.Status = "ok"

	return finishInit(stdout, res, *asJSON, nil)
}

// initTargetChoice is the server init wires: serverLocal or a base URL.
// write is set when init writes the URL to config.toml.
type initTargetChoice struct {
	server string
	write  bool
}

// initTarget decides where reports go. A configured URL (the environment or
// config.toml) is kept: --local, a different --server and a key to write
// are refused, since a configured server never silently becomes local and
// init never edits the configuration. Otherwise --server, then the answer on
// a terminal, then local; a server the install manifest records is the
// terminal's default, and without a terminal it needs --server or --local.
func initTarget(env harness.Env, local bool, flagServer string, keyFromStdin, ask bool, in *bufio.Reader, stderr io.Writer) (initTargetChoice, error) {
	path, err := configPath(os.Getenv)
	if err != nil {
		return initTargetChoice{}, err
	}
	file, exists, err := loadFileConfig(path)
	if err != nil {
		return initTargetChoice{}, err
	}
	if u := resolveClient(flagConfig{}, os.Getenv, file).URL; u.Value != "" {
		source := "the url in " + path
		if u.Source == sourceEnv {
			source = envURL
		}
		configured, err := normaliseServer(source, u.Value)
		if err != nil {
			return initTargetChoice{}, err
		}
		if local {
			return initTargetChoice{}, errInitConfiguredServer(configured, source)
		}
		if flagServer != "" {
			srv, err := normaliseServer("--server", flagServer)
			if err != nil {
				return initTargetChoice{}, err
			}
			if srv != configured {
				return initTargetChoice{}, errInstallServerDiffers(srv, configured, source)
			}
		}
		if keyFromStdin {
			return initTargetChoice{}, errInitKeyConfigured(configured, source)
		}

		return initTargetChoice{server: configured}, nil
	}
	recorded, err := env.Server()
	if err != nil {
		return initTargetChoice{}, installErr(err)
	}
	if recorded != "" && recorded != serverLocal {
		if recorded, err = normaliseServer("the server in the install manifest "+env.ManifestPath(), recorded); err != nil {
			return initTargetChoice{}, err
		}
	} else {
		recorded = ""
	}
	srv := serverLocal
	switch {
	case local:
	case flagServer != "":
		if srv, err = normaliseServer("--server", flagServer); err != nil {
			return initTargetChoice{}, err
		}
	case ask:
		prompt := `Where should reports go? Enter for local (a database on this machine, no server), or "cloud" or a server URL: `
		if recorded != "" {
			prompt = `Where should reports go? Enter for ` + recorded + ` (the server the harnesses are wired to), "local" for a database on this machine, or "cloud" or a server URL: `
		}
		fmt.Fprint(stderr, prompt)
		line, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return initTargetChoice{}, errInitAnswer(err)
		}
		switch line = strings.TrimSpace(line); {
		case line != "":
			if srv, err = normaliseServer("the server", line); err != nil {
				return initTargetChoice{}, err
			}
		case recorded != "":
			srv = recorded
		}
	case recorded != "":
		return initTargetChoice{}, errInitRecordedServer(recorded, env.ManifestPath())
	}
	if srv == serverLocal {
		return initTargetChoice{server: serverLocal}, nil
	}
	if exists {
		return initTargetChoice{}, errInitConfigNoURL(path, srv)
	}

	return initTargetChoice{server: srv, write: true}, nil
}

// askKey reads the server's API key from the terminal without echo; an
// empty answer skips it.
func askKey(server string, stderr io.Writer) (string, error) {
	fmt.Fprintf(stderr, "API key for %s (not shown; Enter to skip): ", server)
	line, err := readSecret()
	fmt.Fprintln(stderr)
	if err != nil {
		return "", errInitKeyPrompt(err)
	}
	if strings.TrimSpace(line) == "" {
		return "", nil
	}
	key, ok := parseKey([]byte(line))
	if !ok {
		return "", errInitKeyPromptInvalid()
	}

	return key, nil
}

// askHarnesses asks which harnesses to wire; Enter takes every detected one.
func askHarnesses(detected []string, in *bufio.Reader, stderr io.Writer) ([]string, error) {
	if len(detected) == 0 {
		fmt.Fprint(stderr, "No coding-agent harness was detected. Wire which ones (comma-separated; Enter for none)? ")
	} else {
		fmt.Fprintf(stderr, "Detected: %s. Wire which ones (comma-separated, none; Enter for all detected)? ", strings.Join(detected, ", "))
	}
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errInitAnswer(err)
	}
	line = strings.TrimSpace(line)
	switch {
	case line == "" && len(detected) == 0:
		return []string{"none"}, nil
	case line == "":
		return []string{"all"}, nil
	}

	return parseHarnessList(line)
}

// parseHarnessList splits a comma-separated list of harness names; all and
// none are words of their own, and none stands alone.
func parseHarnessList(s string) ([]string, error) {
	var out []string
	for n := range strings.SplitSeq(s, ",") {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if err := checkHarnessNames(slices.DeleteFunc(slices.Clone(out), func(n string) bool { return n == "none" })); err != nil {
		return nil, err
	}
	if len(out) == 0 || (slices.Contains(out, "none") && len(out) > 1) {
		return nil, usageErr(fmt.Sprintf("--harnesses %q names no harness, or none beside others", s), "pass all, none, or harness names separated by commas")
	}

	return out, nil
}

// writeInitConfig writes config.toml with the server's URL and, when key is
// set, its API key: 0600 from creation, never over an existing file.
func writeInitConfig(server, key string) (string, error) {
	path, err := configPath(os.Getenv)
	if err != nil {
		return "", err
	}
	data, err := toml.Marshal(struct {
		URL    string `toml:"url"`
		APIKey string `toml:"api_key,omitempty"`
	}{server, key})
	if err != nil {
		return "", errInitWrite(path, err)
	}

	return path, writeConfigFile(path, data, false)
}

// hookDescriptions says, per wired harness, what its hooks do.
func hookDescriptions(st []harness.HarnessStatus, local bool) []string {
	adapters := map[string]harness.Adapter{}
	for _, a := range harness.Adapters() {
		adapters[a.Name] = a
	}
	var out []string
	for _, h := range st {
		a := adapters[h.Name]
		if h.Hook == "wired" {
			var does []string
			if a.Hook.Deliver != "none" {
				does = append(does, "counts the session's failed tool calls and, when they pile up, shows the agent a short note suggesting a friction report")
			}
			if local {
				if len(does) == 0 {
					does = append(does, "runs at the end of a turn and has nothing to do in local mode")
				}
			} else {
				does = append(does, "at the end of a turn sends what the client spooled while the server was unreachable")
			}
			out = append(out, h.Name+" hook: "+strings.Join(does, "; "))
		}
		if h.Reminder == "wired" {
			out = append(out, h.Name+" session start: runs agentfeedback prime, which prints the reporting guidance into the session")
		}
	}

	return out
}

// initNextSteps are the commands to go on with.
func initNextSteps(res initResult, t initTargetChoice, noKey bool) []initNext {
	var next []initNext
	if noKey {
		next = append(next, keyStep(res, t))
	}
	next = append(next,
		initNext{Command: "agentfeedback ui", Does: "browse the reports in a read-only page on this machine"},
		initNext{Command: "agentfeedback list --open", Does: "list the reports not yet processed"},
		initNext{Command: "npx skills add AgentFeedback/agentfeedback --skill agentfeedback-triage -g", Does: "install the triage skill (third-party skills CLI), then invoke /agentfeedback-triage in the harness to work through the reports"},
	)
	if len(res.Restart) > 0 {
		next = append(next, initNext{Command: "agentfeedback uninstall " + strings.Join(res.Restart, " "), Does: "remove what init wired, restoring the files it changed"})
	}

	return next
}

// keyStep is how to give the server its API key: the configuration init
// wrote is replaced as a whole, an existing one is the person's to edit, and
// a server from the environment gets a new file.
func keyStep(res initResult, t initTargetChoice) initNext {
	then := "; then run agentfeedback doctor --e2e"
	path, err := configPath(os.Getenv)
	_, statErr := os.Stat(path)
	switch {
	case res.Config != "":
		return initNext{
			Command: `printf '%s' "$KEY" | agentfeedback doctor --init --url ` + t.server + ` --key-from-stdin --force`,
			Does:    "replace the configuration init wrote (the URL alone) with the URL and the API key" + then,
		}
	case err == nil && statErr == nil:
		return initNext{
			Command: "export " + envAPIKey + "=<key>",
			Does:    "give the client the server's API key, or add api_key to " + path + then,
		}
	}

	return initNext{
		Command: `printf '%s' "$KEY" | agentfeedback doctor --init --url ` + t.server + ` --key-from-stdin`,
		Does:    "write the URL and the API key to the configuration" + then,
	}
}

// finishInit prints the result: one JSON line with --json, else the summary
// on stdout. A failure is returned marked as reported once printed as JSON.
func finishInit(stdout io.Writer, res initResult, asJSON bool, err error) error {
	if err != nil {
		res.Status, res.Message = "error", cmp.Or(res.Message, err.Error())
		if !asJSON {
			// What was done before the failure; a failed check shows
			// its step here, the router prints any other error.
			if res.Mode != "" {
				printInitSummary(stdout, res)
			}

			return err
		}
		if werr := writeJSON(stdout, res); werr != nil {
			return werr
		}

		return &reportedError{err}
	}
	if asJSON {
		return writeJSON(stdout, res)
	}
	printInitSummary(stdout, res)

	return nil
}

func printInitSummary(w io.Writer, res initResult) {
	if res.Mode == modeLocal {
		fmt.Fprintf(w, "mode:     local (database %s; no server)\n", res.Database)
	} else {
		fmt.Fprintf(w, "mode:     remote (server %s)\n", res.Server)
	}
	if res.Config != "" {
		fmt.Fprintf(w, "config:   %s\n", res.Config)
	}
	fmt.Fprintf(w, "data:     %s\n", res.DataDir)
	for _, h := range res.Harnesses {
		if h.Mode == "-" {
			fmt.Fprintf(w, "harness:  %s not wired\n", h.Name)

			continue
		}
		var parts []string
		for _, c := range [][2]string{{"skill", h.Skill}, {"mcp", h.MCP}, {"hook", h.Hook}, {"reminder", h.Reminder}, {"rule", h.Rule}} {
			if c[1] != "-" {
				parts = append(parts, c[0]+" "+c[1])
			}
		}
		fmt.Fprintf(w, "harness:  %s (%s)\n", h.Name, strings.Join(parts, ", "))
	}
	for _, d := range res.Hooks {
		fmt.Fprintf(w, "hooks:    %s\n", d)
	}
	for _, s := range res.E2E {
		fmt.Fprint(w, "check:    ")
		printE2EStep(w, s)
	}
	if len(res.Restart) > 0 {
		fmt.Fprintf(w, "restart:  %s (a running harness loads the wiring at its next start)\n", strings.Join(res.Restart, ", "))
	}
	for _, n := range res.Next {
		fmt.Fprintf(w, "next:     %s\n          %s\n", n.Command, n.Does)
	}
}
