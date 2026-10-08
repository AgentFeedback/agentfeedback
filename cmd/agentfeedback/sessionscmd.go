package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

const (
	sessionsSynopsis       = "sessions list|digest|mark|status [flags]"
	sessionsListSynopsis   = "sessions list [--harness H] [--since T] [--unprocessed] [--json]"
	sessionsDigestSynopsis = "sessions digest <harness:id>... | --unprocessed [--harness H] [--limit N] [--allow-unknown-project] [--json]"
	sessionsMarkSynopsis   = "sessions mark <harness:id>... --outcome filed|nothing|skipped [--ref UID]... [--json]"
	sessionsStatusSynopsis = "sessions status [--json] | sessions status --set-selection [--harness H]... [--since T] [--limit N] [--json]"
)

// sessionsSubcommands are the subcommands of sessions, in help order.
var sessionsSubcommands = []string{"list", "digest", "mark", "status"}

// sessionsEnvFrom is the internal/sessions environment: the home directory,
// the harnesses' store directories (each an absolute environment variable,
// else its default under the home directory; Claude Code's as install
// resolves it), $OPENCODE_DB as set and the config file's [collect] table.
func sessionsEnvFrom(getenv func(string) string, file fileConfig) (sessions.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sessions.Env{}, errNoHome(err)
	}

	return sessionsEnvOf(getenv, home, file), nil
}

// sessionsEnvOf resolves the sessions environment for home; a relative
// directory variable is ignored.
func sessionsEnvOf(getenv func(string) string, home string, file fileConfig) sessions.Env {
	dir := func(name string, fallback ...string) string {
		if d := getenv(name); d != "" && filepath.IsAbs(d) {
			return d
		}

		return filepath.Join(append([]string{home}, fallback...)...)
	}
	gemini := filepath.Join(home, ".gemini")
	if d := getenv("GEMINI_CLI_HOME"); d != "" && filepath.IsAbs(d) {
		gemini = filepath.Join(d, ".gemini")
	}

	return sessions.Env{
		Home: home, ClaudeConfigDir: dir("CLAUDE_CONFIG_DIR", ".claude"),
		CodexHome: dir("CODEX_HOME", ".codex"), CopilotHome: dir("COPILOT_HOME", ".copilot"), GeminiDir: gemini,
		DataHome: dir("XDG_DATA_HOME", ".local", "share"), OpenCodeDB: getenv("OPENCODE_DB"),
		Policy: file.Collect, Now: nowFunc,
	}
}

// sessionsEnv loads the config file and builds the sessions environment.
func sessionsEnv(getenv func(string) string) (sessions.Env, error) {
	path, err := configPath(getenv)
	if err != nil {
		return sessions.Env{}, err
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		return sessions.Env{}, err
	}

	return sessionsEnvFrom(getenv, file)
}

// openSessionsDB opens the data-directory database, whatever the mode:
// session state is local and never sent to a server. No start-up pass runs.
func openSessionsDB(getenv func(string) string, stderr io.Writer) (*store.DB, error) {
	if localTarget == nil {
		path, err := localDBPath(getenv)
		if err != nil {
			return nil, err
		}
		t, err := localmode.Open(context.Background(), path, clientVersion().Version, stderr)
		if err != nil {
			return nil, errLocalOpen(path, err)
		}
		localTarget = t
	}

	return localTarget.DB(), nil
}

// sessionsService answers the sessions commands and the stdio session tools
// over one database and environment.
type sessionsService struct {
	db  *store.DB
	env sessions.Env
}

func newSessionsService(getenv func(string) string, stderr io.Writer) (*sessionsService, error) {
	env, err := sessionsEnv(getenv)
	if err != nil {
		return nil, err
	}
	db, err := openSessionsDB(getenv, stderr)
	if err != nil {
		return nil, err
	}

	return &sessionsService{db: db, env: env}, nil
}

// sinceTime parses a --since value: RFC 3339 or relative.
func sinceTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	s, err := parseTimeFlag("since", v)
	if err != nil {
		return time.Time{}, err
	}

	return time.Parse(time.RFC3339Nano, s)
}

// knownHarness refuses a harness without a session reader.
func knownHarness(h string) error {
	if h == "" {
		return nil
	}
	for _, have := range sessions.Harnesses() {
		if have == h {
			return nil
		}
	}

	return fmt.Errorf("no session reader for harness %q (have %s)", h, strings.Join(sessions.Harnesses(), ", "))
}

func checkRefs(refs []string) error {
	for _, r := range refs {
		if _, _, err := sessions.ParseRef(r); err != nil {
			return err
		}
	}

	return nil
}

func checkOutcome(o string) error {
	switch o {
	case sessions.OutcomeFiled, sessions.OutcomeNothing, sessions.OutcomeSkipped:
		return nil
	}

	return fmt.Errorf("outcome %q: want filed, nothing or skipped", o)
}

// The mcp.Sessions methods: argument errors come back as validation
// problems, anything else as an internal error.

func (s *sessionsService) List(ctx context.Context, a mcp.SessionsListArgs) ([]byte, error) {
	if err := knownHarness(a.Harness); err != nil {
		return nil, mcp.InvalidArgument("harness", err.Error())
	}
	since, err := sinceTime(a.Since)
	if err != nil {
		return nil, mcp.InvalidArgument("since", err.Error())
	}
	l, err := sessions.List(ctx, s.db, s.env, sessions.ListOptions{Harness: a.Harness, Since: since, Unprocessed: a.Unprocessed})
	if err != nil {
		return nil, err
	}

	return json.Marshal(l)
}

func (s *sessionsService) Digest(ctx context.Context, a mcp.SessionsDigestArgs) ([]byte, error) {
	if err := knownHarness(a.Harness); err != nil {
		return nil, mcp.InvalidArgument("harness", err.Error())
	}
	if err := checkRefs(a.Refs); err != nil {
		return nil, mcp.InvalidArgument("refs", err.Error())
	}
	out, err := sessions.Digest(ctx, s.db, s.env, sessions.DigestRequest{Refs: a.Refs, Unprocessed: a.Unprocessed,
		Harness: a.Harness, Limit: a.Limit})
	if err != nil {
		return nil, err
	}

	return json.Marshal(out)
}

// markResult is what sessions mark prints with --json and the tool answers.
type markResult struct {
	Marked  []string `json:"marked"`
	Outcome string   `json:"outcome"`
	UIDs    []string `json:"uids"`
}

func (s *sessionsService) Mark(ctx context.Context, a mcp.SessionsMarkArgs) ([]byte, error) {
	if err := checkRefs(a.Refs); err != nil {
		return nil, mcp.InvalidArgument("refs", err.Error())
	}
	if err := checkOutcome(a.Outcome); err != nil {
		return nil, mcp.InvalidArgument("outcome", err.Error())
	}
	if err := sessions.CheckUIDs(a.UIDs); err != nil {
		return nil, mcp.InvalidArgument("uids", err.Error())
	}
	if err := sessions.Mark(ctx, s.db, s.env, a.Refs, a.Outcome, a.UIDs); err != nil {
		if errors.Is(err, store.ErrNotDigested) {
			return nil, mcp.InvalidArgument("refs", err.Error())
		}

		return nil, err
	}
	uids := a.UIDs
	if uids == nil {
		uids = []string{}
	}

	return json.Marshal(markResult{Marked: a.Refs, Outcome: a.Outcome, UIDs: uids})
}

// runSessions dispatches the sessions subcommands.
func runSessions(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errArgs("sessions", sessionsSynopsis)
	}
	switch args[0] {
	case "-h", "--help", "-help":
		fmt.Fprintf(stderr, "usage: agentfeedback %s\n  %s\n  %s\n  %s\n  %s\n", sessionsSynopsis,
			sessionsListSynopsis, sessionsDigestSynopsis, sessionsMarkSynopsis, sessionsStatusSynopsis)

		return flag.ErrHelp
	case "list":
		return runSessionsList(args[1:], stdout, stderr)
	case "digest":
		return runSessionsDigest(args[1:], stdout, stderr)
	case "mark":
		return runSessionsMark(args[1:], stdout, stderr)
	case "status":
		return runSessionsStatus(args[1:], stdout, stderr)
	}

	return usageErr(fmt.Sprintf("unknown sessions subcommand %q", args[0]), "use agentfeedback "+sessionsSynopsis)
}

func runSessionsList(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sessions list")
	harness := fs.String("harness", "", "only this harness ("+strings.Join(sessions.Harnesses(), ", ")+")")
	since := fs.String("since", "", "sessions whose last entry is on or after this RFC 3339 time or <n>m, <n>h, <n>d, <n>w ago")
	unprocessed := fs.Bool("unprocessed", false, "only new and changed sessions")
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", sessionsListSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("sessions list", err)
	}
	if len(pos) != 0 {
		return errArgs("sessions list", sessionsListSynopsis)
	}
	if err := knownHarness(*harness); err != nil {
		return usageErr(err.Error(), "pass --harness "+strings.Join(sessions.Harnesses(), " or --harness "))
	}
	t, err := sinceTime(*since)
	if err != nil {
		return err
	}
	svc, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		return err
	}
	l, err := sessions.List(context.Background(), svc.db, svc.env, sessions.ListOptions{Harness: *harness, Since: t, Unprocessed: *unprocessed})
	if err != nil {
		return errSessions(err)
	}
	storeNotes(l.Stores, *harness != "", stderr)
	if *asJSON {
		return writeJSON(stdout, l)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tSTATE\tPROJECT\tSTART\tEND\tPROMPTS\tTOOL CALLS\tTOOL ERRORS")
	for _, s := range l.Sessions {
		state := s.State
		if s.Reason != "" {
			state += " (" + s.Reason + ")"
		}
		prompts, calls, errs := "-", "-", "-"
		if s.Counts != nil {
			prompts, calls, errs = strconv.Itoa(s.Counts.Prompts), strconv.Itoa(s.Counts.ToolCalls), strconv.Itoa(s.Counts.ToolErrors)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Ref, state, dash(s.Project), shownTime(s.Start), shownTime(s.End), prompts, calls, errs)
	}

	return tw.Flush()
}

// storeNotes names on stderr a store that is not fully readable, and one
// that is absent only when its harness was asked for by name: most
// machines run few of the harnesses with a reader.
func storeNotes(stores []sessions.Store, named bool, stderr io.Writer) {
	for _, st := range stores {
		if st.State != sessions.StorePresent && (named || st.State != sessions.StateAbsent) {
			fmt.Fprintf(stderr, "agentfeedback sessions: %s session store %s is %s\n", st.Harness, st.Location, st.State)
		}
		for _, p := range st.Problems {
			fmt.Fprintf(stderr, "agentfeedback sessions: warning: %s: not listed: %s\n", st.Harness, p)
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func shownTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	return t.UTC().Format(time.RFC3339)
}

func runSessionsDigest(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sessions digest")
	unprocessed := fs.Bool("unprocessed", false, "digest the new and changed sessions instead of the refs given")
	harness := fs.String("harness", "", "with --unprocessed: only this harness")
	limit := fs.Int("limit", 0, "with --unprocessed: at most this many sessions (0: no limit)")
	allowUnknown := fs.Bool("allow-unknown-project", false, "also digest sessions that record no working directory")
	asJSON := fs.Bool("json", false, "print the digest as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", sessionsDigestSynopsis)
		fs.PrintDefaults()
	}
	refs, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("sessions digest", err)
	}
	given := visited(fs)
	switch {
	case *unprocessed && len(refs) > 0:
		return usageErr("refs and --unprocessed are both given", "pass session refs or --unprocessed")
	case !*unprocessed && len(refs) == 0:
		return errArgs("sessions digest", sessionsDigestSynopsis)
	case !*unprocessed && (given["harness"] || given["limit"]):
		return usageErr("--harness and --limit apply only with --unprocessed", "drop them, or pass --unprocessed instead of refs")
	case *limit < 0:
		return usageErr(fmt.Sprintf("--limit %d is negative", *limit), "pass 0 for no limit or a positive number")
	}
	if err := knownHarness(*harness); err != nil {
		return usageErr(err.Error(), "pass --harness "+strings.Join(sessions.Harnesses(), " or --harness "))
	}
	if err := checkRefs(refs); err != nil {
		return usageErr(err.Error(), "name a session as <harness>:<session id>, as sessions list prints it")
	}
	svc, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		return err
	}
	out, err := sessions.Digest(context.Background(), svc.db, svc.env, sessions.DigestRequest{Refs: refs, Unprocessed: *unprocessed,
		Harness: *harness, Limit: *limit, AllowUnknownProject: *allowUnknown})
	if err != nil {
		return errSessions(err)
	}
	if *asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = printDigest(stdout, out)
	}
	if err != nil {
		return err
	}
	if !*asJSON {
		for _, e := range out.Errors {
			fmt.Fprintf(stderr, "agentfeedback sessions digest: %s: %s: %s\n", e.Ref, e.State, e.Message)
		}
	}
	// Refs were named and none could be digested: that is a failure.
	if len(out.Sessions) == 0 && len(out.Errors) > 0 {
		return exitStatus(1)
	}

	return nil
}

// printDigest is the human form of a digest: the notice first, then each
// session's events.
func printDigest(w io.Writer, out sessions.DigestOutput) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n(detector version %s)\n", out.Notice, out.DetectorVersion)
	for _, sd := range out.Sessions {
		fmt.Fprintf(&b, "\n== %s  project %s  cwd %s\n", sd.Ref, dash(sd.Project), dash(sd.Cwd))
		fmt.Fprintf(&b, "   %s .. %s  model %s  from offset %d", dash(sd.Start), dash(sd.End), dash(sd.Model), sd.FromOffset)
		if sd.Truncated {
			b.WriteString("  (truncated)")
		}
		fmt.Fprintf(&b, "\n   prompts %d, tool calls %d, tool errors %d, interrupts %d, denials %d\n",
			sd.Counts.Prompts, sd.Counts.ToolCalls, sd.Counts.ToolErrors, sd.Counts.Interrupts, sd.Counts.Denials)
		for _, e := range sd.Events {
			fmt.Fprintf(&b, "   %s  %s", dash(e.At), e.Type)
			if e.Tool != "" {
				fmt.Fprintf(&b, "  %s", e.Tool)
			}
			if e.Status != "" {
				fmt.Fprintf(&b, "  %s", e.Status)
			}
			if e.ExitCode != nil {
				fmt.Fprintf(&b, "  exit %d", *e.ExitCode)
			}
			if e.ErrorClass != "" {
				fmt.Fprintf(&b, "  %s", e.ErrorClass)
			}
			if e.Self {
				b.WriteString("  (agentfeedback's own command)")
			}
			fmt.Fprintf(&b, "  #%s\n", e.Span)
			if e.Summary != "" {
				fmt.Fprintf(&b, "      %s\n", e.Summary)
			}
			if e.Excerpt != "" {
				fmt.Fprintf(&b, "      %s\n", oneLine(e.Excerpt))
			}
		}
		for _, r := range sd.Retries {
			fmt.Fprintf(&b, "   retry #%s of #%s  %s\n", r.Span, r.OfSpan, r.Tool)
		}
		for _, f := range sd.Flagged {
			fmt.Fprintf(&b, "   flagged #%s  %q\n", f.Span, f.Phrase)
		}
		if sd.FinalExcerpt != "" {
			fmt.Fprintf(&b, "   final: %s\n", oneLine(sd.FinalExcerpt))
		}
	}
	for _, ref := range out.Deferred {
		fmt.Fprintf(&b, "\ndeferred (over the run cap; digest it in a later run): %s", ref)
	}
	if len(out.Deferred) > 0 {
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())

	return err
}

func runSessionsMark(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sessions mark")
	outcome := fs.String("outcome", "", "filed, nothing or skipped")
	var uids multiFlag
	fs.Var(&uids, "ref", "uid of a submission filed from these sessions; repeatable")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", sessionsMarkSynopsis)
		fs.PrintDefaults()
	}
	refs, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("sessions mark", err)
	}
	if len(refs) == 0 || *outcome == "" {
		return errArgs("sessions mark", sessionsMarkSynopsis)
	}
	if err := checkOutcome(*outcome); err != nil {
		return usageErr(err.Error(), "pass --outcome filed, nothing or skipped")
	}
	if err := checkRefs(refs); err != nil {
		return usageErr(err.Error(), "name a session as <harness>:<session id>, as sessions list prints it")
	}
	if err := sessions.CheckUIDs(uids); err != nil {
		return usageErr(oneLine(err.Error()), "pass --ref with the uid of a filed submission")
	}
	svc, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		return err
	}
	if err := sessions.Mark(context.Background(), svc.db, svc.env, refs, *outcome, uids); err != nil {
		if errors.Is(err, store.ErrNotDigested) {
			return failErr(oneLine(err.Error()), "digest the session with agentfeedback sessions digest, then mark it")
		}

		return errSessions(err)
	}
	res := markResult{Marked: refs, Outcome: *outcome, UIDs: uids}
	if res.UIDs == nil {
		res.UIDs = []string{}
	}
	if *asJSON {
		return writeJSON(stdout, res)
	}
	_, err = fmt.Fprintf(stdout, "marked %d session(s) %s\n", len(refs), *outcome)

	return err
}

func runSessionsStatus(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sessions status")
	set := fs.Bool("set-selection", false, "save the selection given by --harness, --since and --limit, then print the status")
	var harnesses multiFlag
	fs.Var(&harnesses, "harness", "with --set-selection: a harness of the selection; repeatable")
	since := fs.String("since", "", "with --set-selection: the selection's RFC 3339 or relative time (7d), saved as given")
	limit := fs.Int("limit", 0, "with --set-selection: the selection's session limit (0: none)")
	asJSON := fs.Bool("json", false, "print the status as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", sessionsStatusSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("sessions status", err)
	}
	if len(pos) != 0 {
		return errArgs("sessions status", sessionsStatusSynopsis)
	}
	given := visited(fs)
	if !*set && (given["harness"] || given["since"] || given["limit"]) {
		return usageErr("--harness, --since and --limit apply only with --set-selection", "add --set-selection, or drop them")
	}
	if *set {
		for _, h := range harnesses {
			if err := knownHarness(h); err != nil {
				return usageErr(err.Error(), "pass --harness "+strings.Join(sessions.Harnesses(), " or --harness "))
			}
		}
		if _, err := sinceTime(*since); err != nil {
			return err
		}
		if *limit < 0 {
			return usageErr(fmt.Sprintf("--limit %d is negative", *limit), "pass 0 for no limit or a positive number")
		}
	}
	svc, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if *set {
		if err := sessions.SetSelection(ctx, svc.db, sessions.Selection{Harnesses: harnesses, Since: *since, Limit: *limit}); err != nil {
			return errSessions(err)
		}
	}
	st, err := sessions.Status(ctx, svc.db, svc.env)
	if err != nil {
		return errSessions(err)
	}
	if *asJSON {
		return writeJSON(stdout, st)
	}
	var b strings.Builder
	for _, h := range st.Harnesses {
		fmt.Fprintf(&b, "%s: store %s (%s)\n", h.Harness, h.Location, h.Store)
		for _, p := range h.Problems {
			fmt.Fprintf(&b, "  not listed: %s\n", p)
		}
		for _, state := range []string{sessions.StateNew, sessions.StateChanged, sessions.StateProcessed, sessions.StateAbsent,
			sessions.StateDisabled, sessions.StateDenied, sessions.StateUnknownProject, sessions.StateUnreadable, sessions.StateUnsupportedFormat} {
			if n := h.States[state]; n > 0 {
				fmt.Fprintf(&b, "  %-18s %d\n", state, n)
			}
		}
	}
	if st.Selection == nil {
		b.WriteString("selection: none saved\n")
	} else {
		fmt.Fprintf(&b, "selection: harnesses %s, since %s, limit %d\n", dash(strings.Join(st.Selection.Harnesses, ",")), dash(st.Selection.Since), st.Selection.Limit)
	}
	_, err = io.WriteString(stdout, b.String())

	return err
}

// errSessions words a failure of internal/sessions.
func errSessions(err error) error {
	var ue *userError
	if errors.As(err, &ue) {
		return err
	}
	e := failErr("the session state cannot be read or written: "+oneLine(err.Error()), "run agentfeedback doctor")
	e.cause = err

	return e
}
