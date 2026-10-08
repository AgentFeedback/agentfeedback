package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

const (
	submitSynopsis = "submit friction --summary S [flags] | submit <kind> [--stdin] [--scrub] [flags] | submit review <run_dir> [--include-outputs] | submit review --sweep [<base>...] [--local | --server URL]"
	flushSynopsis  = "flush [--hook] [--local | --server URL]"
	// occurredLayout is occurred_at: UTC with microseconds.
	occurredLayout = "2006-01-02T15:04:05.000000Z"
	// stdinMax bounds what --stdin reads: one byte over the body limit is
	// enough to know the body is too large.
	stdinMax = envelope.BodyLimit + 1
)

// frictionPayload maps the friction flags to their payload members.
var frictionPayload = []struct{ flag, member string }{
	{"category", "category"},
	{"details", "details"},
	{"suggested-fix", "suggested_fix"},
	{"fix-status", "fix_status"},
	{"fix-ref", "fix_ref"},
	{"severity", "severity"},
}

// rawMember is one top-level member of a JSON object, its value kept as
// the bytes it was given.
type rawMember struct {
	name string
	raw  json.RawMessage
}

// rawObject is a JSON object as an ordered member list: numbers, unknown
// members and their spellings pass through byte for byte.
type rawObject []rawMember

// parseObject reads exactly one JSON object at the token level.
func parseObject(data []byte) (rawObject, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the input is empty")
	}
	if !json.Valid(data) {
		dec := json.NewDecoder(bytes.NewReader(data))
		var first json.RawMessage
		if err := dec.Decode(&first); err == nil {
			return nil, errors.New("more than one JSON value")
		}

		return nil, errors.New("not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("the value is not an object")
	}
	var out rawObject
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, rawMember{name, raw})
	}

	return out, nil
}

// isObject reports whether raw is a JSON object.
func isObject(raw json.RawMessage) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")

	return len(t) > 0 && t[0] == '{'
}

func (o rawObject) has(name string) bool {
	return slices.ContainsFunc(o, func(m rawMember) bool { return m.name == name })
}

// get returns the last value of name, as the server reads it.
func (o rawObject) get(name string) (json.RawMessage, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].name == name {
			return o[i].raw, true
		}
	}

	return nil, false
}

// set replaces the first member called name and drops the later ones, or
// appends it.
func (o rawObject) set(name string, raw json.RawMessage) rawObject {
	out := make(rawObject, 0, len(o)+1)
	done := false
	for _, m := range o {
		if m.name != name {
			out = append(out, m)
		} else if !done {
			out = append(out, rawMember{name, raw})
			done = true
		}
	}
	if !done {
		out = append(out, rawMember{name, raw})
	}

	return out
}

func (o rawObject) remove(name string) rawObject {
	return slices.DeleteFunc(slices.Clone(o), func(m rawMember) bool { return m.name == name })
}

func (o rawObject) setString(name, v string) rawObject { return o.set(name, jsonString(v)) }

// encode writes the object compactly around the member values as given.
func (o rawObject) encode() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(m.name))
		buf.WriteByte(':')
		buf.Write(m.raw)
	}
	buf.WriteByte('}')

	return buf.Bytes()
}

// jsonString encodes s as a JSON string without HTML escaping.
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return bytes.TrimRight(buf.Bytes(), "\n")
}

// stringMap encodes m as an object with sorted keys.
func stringMap(m map[string]string) rawObject {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(rawObject, 0, len(keys))
	for _, k := range keys {
		out = append(out, rawMember{k, jsonString(m[k])})
	}

	return out
}

// hasBoolFlag reports whether args name the boolean flag before a "--".
func hasBoolFlag(args []string, name string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-" + name, "--" + name, "-" + name + "=true", "--" + name + "=true":
			return true
		}
	}

	return false
}

// submitter carries one submit or flush run: where it prints, whether it
// only validates, and the client once one is needed.
type submitter struct {
	name           string
	stdout, stderr io.Writer
	dryRun         bool
	mode           modeFlags
	file           fileConfig
	decision       collect.Decision
	dir            string // the working directory the narrowing rules saw
	c              *client.Client
}

// newSubmitter loads the config file and applies the narrowing rules to the
// working directory; their warnings go to stderr.
func newSubmitter(name string, dryRun bool, mf modeFlags, stdout, stderr io.Writer) (*submitter, error) {
	s := &submitter{name: name, stdout: stdout, stderr: stderr, dryRun: dryRun, mode: mf}
	path, err := configPath(os.Getenv)
	if err != nil {
		return nil, err
	}
	if s.file, _, err = loadFileConfig(path); err != nil {
		return nil, err
	}
	if s.dir, err = os.Getwd(); err != nil {
		return nil, errWorkdir(err)
	}
	s.decision = collect.Check(s.dir, "", s.file.Collect)
	for _, w := range s.decision.Warnings {
		s.warn(w)
	}

	return s, nil
}

func (s *submitter) warn(msg string) {
	fmt.Fprintf(s.stderr, "agentfeedback %s: warning: %s\n", s.name, msg)
}

// client builds the API client on first use.
func (s *submitter) client() (*client.Client, error) {
	if s.c == nil {
		c, err := apiClient(os.Getenv, s.mode, s.stderr)
		if err != nil {
			return nil, err
		}
		s.c = c
	}

	return s.c, nil
}

// logLocal appends an outcome decided without a request to the client log;
// a dry run logs nothing.
func (s *submitter) logLocal(o client.Outcome) {
	if s.dryRun {
		return
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		s.warn(err.Error())

		return
	}
	client.LogTo(cache, o, nowFunc(), s.stderr)
}

// logSetupFailure logs a client setup failure to the client log: a database
// written by a newer binary as a schema_too_new line, anything else as o.
func (s *submitter) logSetupFailure(o client.Outcome, err error) {
	if !errors.Is(err, store.ErrSchemaTooNew) {
		s.logLocal(o)

		return
	}
	if s.dryRun {
		return
	}
	cache, cerr := cacheDir(os.Getenv)
	if cerr != nil {
		s.warn(cerr.Error())

		return
	}
	logSchemaTooNew(cache, err)
}

// disabled reports the narrowing outcome when the working directory is
// switched off, and whether it was.
func (s *submitter) disabled() (client.Outcome, bool) {
	if !s.decision.Disabled {
		return client.Outcome{}, false
	}
	o := client.Disabled(s.decision.Reason)
	s.logLocal(o)

	return o, true
}

// send checks body locally and sends it, or with --dry-run prints it and its
// local warnings. It never prints the outcome.
func (s *submitter) send(body []byte) (client.Outcome, error) {
	prepared, kind, key, err := client.PrepareBody(body)
	if err != nil {
		o := client.Outcome{Outcome: client.OutcomeRejected, Reason: "invalid_body", Message: err.Error()}
		s.logLocal(o)

		return o, nil
	}
	if len(prepared) > envelope.BodyLimit {
		o := client.Outcome{
			Outcome: client.OutcomeRejected, Kind: kind, Key: key, Reason: "body_too_large",
			Message: fmt.Sprintf("the body is %d bytes, over the %d-byte limit; it was not sent", len(prepared), envelope.BodyLimit),
		}
		s.logLocal(o)

		return o, nil
	}
	if s.dryRun {
		if _, err := s.stdout.Write(append(slices.Clone(prepared), '\n')); err != nil {
			return client.Outcome{}, err
		}
		_, warnings, err := envelope.Decode(prepared)
		var rej *envelope.Rejection
		if errors.As(err, &rej) {
			return client.Outcome{Outcome: client.OutcomeRejected, Kind: kind, Key: key, Reason: rej.Code, Message: rej.Message}, nil
		}
		for _, w := range warnings {
			fmt.Fprintf(s.stderr, "agentfeedback %s: warning: %s %s: %s\n", s.name, w.Code, w.Pointer, w.Message)
		}

		return client.Valid(kind, key), nil
	}
	c, err := s.client()
	if err != nil {
		// The body may have come from stdin and is gone with this process:
		// echo it for recovery, and keep the one-outcome-line contract.
		o := client.Outcome{Outcome: client.OutcomeError, Kind: kind, Key: key, Reason: "client_setup", Message: err.Error()}
		fmt.Fprintf(s.stderr, "agentfeedback %s: %s; the submission was NOT persisted and is echoed below for recovery\n", s.name, err)
		_, _ = s.stderr.Write(prepared)
		fmt.Fprintln(s.stderr)
		s.logSetupFailure(o, err)

		return o, nil
	}

	return c.Submit(context.Background(), prepared), nil
}

// finish prints the outcome as the last stdout line and maps it to the exit
// status.
func finish(name string, o client.Outcome, stdout io.Writer) error {
	if err := o.Write(stdout); err != nil {
		return err
	}
	if o.ExitCode() != 0 {
		return &reportedError{errors.New(name + " " + o.Outcome)}
	}

	return nil
}

// runSubmit dispatches submit friction, submit review and submit <kind>.
func runSubmit(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errArgs("submit", submitSynopsis)
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
		fmt.Fprintf(stderr, "usage: agentfeedback %s\n", submitSynopsis)

		return flag.ErrHelp
	}
	if strings.HasPrefix(args[0], "-") {
		return errArgs("submit", submitSynopsis)
	}
	kind, rest := args[0], args[1:]
	if schema.Token(kind) == "" {
		return errSubmitKind(kind)
	}
	if schema.Token(kind) == "review" && !hasBoolFlag(rest, "stdin") {
		return runReview(rest, stdout, stderr)
	}

	return runEnvelope(kind, rest, stdin, stdout, stderr)
}

// runEnvelope is submit friction and submit <kind>: flags over stdin over
// the environment and the config file over collected context.
func runEnvelope(kind string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	friction := schema.Token(kind) == "friction"
	fs := newFlagSet("submit " + kind)
	mf := addModeFlags(fs)
	str := map[string]*string{}
	for _, name := range []string{"summary", "project", "harness", "model", "machine", "key", "schema-version"} {
		str[name] = fs.String(name, "", name)
	}
	if friction {
		for _, p := range frictionPayload {
			str[p.flag] = fs.String(p.flag, "", "payload."+p.member)
		}
	}
	var ctxFlags contextFlags
	fs.Var(&ctxFlags, "context", "key=value: set context.<key> to the string value over stdin's and the collected context; repeatable")
	useStdin := fs.Bool("stdin", false, "read one JSON object from stdin and merge the flags into it")
	dryRun := fs.Bool("dry-run", false, "print the body and check it locally; send nothing")
	scrubFlag := fs.Bool("scrub", false, "replace known secret formats in every string value of the body with [REDACTED:<class>] before checking or sending")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("submit", err)
	}
	if len(pos) != 0 {
		return errArgs("submit", submitSynopsis)
	}
	// The narrowing gate comes before anything reads or checks the input: a
	// switched-off directory is disabled whatever stdin holds.
	s, err := newSubmitter("submit", *dryRun, *mf, stdout, stderr)
	if err != nil {
		return err
	}
	if o, off := s.disabled(); off {
		return finish("submit", o, stdout)
	}
	given := visited(fs)

	var version json.RawMessage
	if given["schema-version"] {
		n, err := strconv.ParseUint(*str["schema-version"], 10, 63)
		if err != nil || n == 0 {
			return errSchemaVersionFlag(*str["schema-version"])
		}
		version = json.RawMessage(strconv.FormatUint(n, 10))
	}

	var obj rawObject
	tooBig := false
	if *useStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, stdinMax))
		if err != nil {
			return errStdinRead(err)
		}
		if tooBig = len(data) > envelope.BodyLimit; !tooBig {
			if obj, err = parseObject(data); err != nil {
				return errStdinObject(err.Error())
			}
		}
	}
	if friction && !tooBig && !given["summary"] && !obj.has("summary") {
		return errSummaryRequired()
	}

	// Payload flags go into payload and drop a flat top-level spelling.
	var payloadSet []rawMember
	if friction {
		for _, p := range frictionPayload {
			if given[p.flag] {
				payloadSet = append(payloadSet, rawMember{p.member, jsonString(*str[p.flag])})
			}
		}
	}
	if len(payloadSet) > 0 {
		var payload rawObject
		if raw, ok := obj.get("payload"); ok {
			if !isObject(raw) {
				return errPayloadNotObject()
			}
			if payload, err = parseObject(raw); err != nil {
				return errPayloadNotObject()
			}
		}
		for _, m := range payloadSet {
			payload = payload.set(m.name, m.raw)
			obj = obj.remove(m.name)
		}
		obj = obj.set("payload", payload.encode())
	}

	if tooBig {
		o := client.Outcome{Outcome: client.OutcomeRejected, Reason: "body_too_large",
			Message: fmt.Sprintf("stdin is over the %d-byte limit; it was not sent", envelope.BodyLimit)}
		s.logLocal(o)

		return finish("submit", o, stdout)
	}

	nonCode := map[string]string{
		"app": s.file.Context.App, "workspace": s.file.Context.Workspace, "url": s.file.Context.URL,
		"channel": s.file.Context.Channel, "task_id": s.file.Context.TaskID, "workflow": s.file.Context.Workflow,
	}
	collected := collect.Collect(collect.Options{
		Dir: s.dir, Getenv: os.Getenv, ClientVersion: clientVersion().Version, WithCwd: s.file.Context.Cwd,
		Context: nonCode, Drop: append(slices.Clone(s.file.Context.Drop), s.decision.Drop...),
	})
	settings := resolveClient(flagConfig{}, os.Getenv, s.file)

	obj = obj.setString("kind", kind)
	flagOr := func(member, flagName string, fallbacks ...string) {
		if given[flagName] {
			obj = obj.setString(member, *str[flagName])

			return
		}
		if obj.has(member) {
			return
		}
		for _, v := range fallbacks {
			if v != "" {
				obj = obj.setString(member, v)

				return
			}
		}
	}
	flagOr("key", "key")
	if version != nil {
		obj = obj.set("schema_version", version)
	}
	flagOr("summary", "summary")
	flagOr("project", "project", collected.Project)
	flagOr("machine", "machine", settings.Machine.Value, collected.Machine)
	flagOr("harness", "harness", settings.Harness.Value, collected.Harness)
	flagOr("model", "model", settings.Model.Value, collected.Model)
	if !obj.has("occurred_at") {
		obj = obj.setString("occurred_at", nowFunc().UTC().Format(occurredLayout))
	}
	// Collected context goes under stdin's: stdin keys win.
	if raw, ok := obj.get("context"); ok {
		if isObject(raw) {
			if stdinCtx, err := parseObject(raw); err == nil {
				for _, m := range stringMap(collected.Context) {
					if !stdinCtx.has(m.name) {
						stdinCtx = append(stdinCtx, m)
					}
				}
				obj = obj.set("context", stdinCtx.encode())
			}
		}
	} else if len(collected.Context) > 0 {
		obj = obj.set("context", stringMap(collected.Context).encode())
	}
	// --context goes over both.
	if len(ctxFlags) > 0 {
		var ctx rawObject
		if raw, ok := obj.get("context"); ok {
			if !isObject(raw) {
				return errContextNotObject()
			}
			if ctx, err = parseObject(raw); err != nil {
				return errContextNotObject()
			}
		}
		for _, kv := range ctxFlags {
			ctx = ctx.set(kv[0], jsonString(kv[1]))
		}
		obj = obj.set("context", ctx.encode())
	}
	// A body with no payload and nothing for the server to move into one
	// gets an empty payload rather than a payload_inferred warning.
	members := envelope.Members()
	if !obj.has("payload") && !slices.ContainsFunc(obj, func(m rawMember) bool { return !slices.Contains(members, m.name) }) {
		obj = obj.set("payload", json.RawMessage("{}"))
	}

	var warnings []client.Warning
	if *scrubFlag || originSessionScan(obj) {
		obj, warnings = scrubBody(obj)
		for _, w := range warnings {
			s.warn(w.Code + " " + w.Pointer + ": " + w.Message)
		}
	}

	o, err := s.send(obj.encode())
	if err != nil {
		return err
	}
	if len(warnings) > 0 {
		o.Warnings = append(o.Warnings, warnings...)
	}

	return finish("submit", o, stdout)
}

// contextFlags are the --context key=value pairs in the order given.
type contextFlags [][2]string

func (c *contextFlags) String() string { return "" }

// Set takes one key=value; the key must not be empty.
func (c *contextFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("--context wants key=value with a non-empty key, got %q", v)
	}
	*c = append(*c, [2]string{k, val})

	return nil
}

// originSessionScan reports whether the body's context.origin is
// session-scan, which turns scrubbing on without --scrub.
func originSessionScan(obj rawObject) bool {
	raw, ok := obj.get("context")
	if !ok || !isObject(raw) {
		return false
	}
	ctx, err := parseObject(raw)
	if err != nil {
		return false
	}
	rawOrigin, ok := ctx.get("origin")
	if !ok {
		return false
	}
	var origin string
	if json.Unmarshal(rawOrigin, &origin) != nil {
		return false
	}

	return strings.EqualFold(strings.TrimSpace(origin), "session-scan")
}

// scrubBody replaces known secret formats in the string values of every
// member, duplicates and context included, in place, and returns one
// warning per member it changed. Member names are never changed.
func scrubBody(obj rawObject) (rawObject, []client.Warning) {
	var warnings []client.Warning
	out := make(rawObject, len(obj))
	for i, m := range obj {
		out[i] = m
		raw, c, err := scrub.JSON(m.raw)
		if err != nil || c.Total() == 0 {
			continue
		}
		out[i].raw = raw
		warnings = append(warnings, client.Warning{Code: "scrubbed", Pointer: "/" + schema.EscapeToken(m.name),
			Message: scrubMessage(c)})
	}

	return out, warnings
}

// scrubMessage is the warning message for c.
func scrubMessage(c scrub.Counts) string {
	noun := "values"
	if c.Total() == 1 {
		noun = "value"
	}

	return fmt.Sprintf("redacted %d %s matching known secret formats: %s", c.Total(), noun, c)
}

// flushReport is the one line flush prints.
type flushReport struct {
	Flushed    int `json:"flushed"`
	Duplicates int `json:"duplicates"`
	Pending    int `json:"pending"`
	Rejected   int `json:"rejected"`
	Mismatched int `json:"mismatched"`
	Expired    int `json:"expired"`
	Deferred   int `json:"deferred"`
	// OtherDestination counts the entries bound to another destination
	// than this invocation's (a server, or the local database), left in
	// place and named on stderr.
	OtherDestination int    `json:"other_destination"`
	Stopped          string `json:"stopped,omitempty"`
}

// runFlush sends the spool's due entries once and prints the counts. The
// flush is the start-up pass, so it skips the one every other command runs.
func runFlush(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	skipStartupPass = true
	fs := newFlagSet("flush")
	mf := addModeFlags(fs)
	hook := fs.Bool("hook", false, "run as a harness hook, as installs before agentfeedback hook wired it: print nothing, stop within 4.5 seconds, log failures to the client log and exit 0")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("flush", err)
	}
	if len(pos) != 0 {
		return errArgs("flush", flushSynopsis)
	}
	if *hook {
		runFlushHook(*mf)

		return nil
	}
	s, err := newSubmitter("flush", false, *mf, stdout, stderr)
	if err != nil {
		return err
	}
	if o, off := s.disabled(); off {
		return finish("flush", o, stdout)
	}
	if empty, err := flushNothingLocal(*mf); err != nil {
		return err
	} else if empty {
		// Nothing is due: no reason to create the local database.
		return writeJSON(stdout, flushReport{})
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	rep := c.Flush(context.Background())
	if local, err := localMode(*mf); err == nil && local {
		if data, err := dataDir(os.Getenv); err == nil {
			inboxPass(context.Background(), c, data)
		}
	}

	return writeJSON(stdout, flushReport{
		Flushed: rep.Flushed, Duplicates: rep.Duplicates, Pending: rep.Pending, Rejected: rep.Rejected,
		Mismatched: rep.Mismatched, Expired: rep.Expired, Deferred: rep.Deferred,
		OtherDestination: rep.OtherDestination, Stopped: rep.Stopped,
	})
}

// flushNothingLocal reports whether this flush is in local mode with an
// empty spool and no inbox file, in which case opening (and on a fresh
// machine creating) the local database would be a side effect of a no-op.
// Any entry, due or not, bound here or elsewhere, is reported by the flush
// itself. In local mode a flush also ingests the inbox, silently, as the
// start-up pass does.
func flushNothingLocal(mf modeFlags) (bool, error) {
	local, err := localMode(mf)
	if err != nil || !local {
		return false, err
	}
	data, err := dataDir(os.Getenv)
	if err != nil {
		return false, err
	}

	return !client.HasEntries(data) && !inboxHasCandidates(data), nil
}

// localMode reports whether mf and the configuration select local mode.
func localMode(mf modeFlags) (bool, error) {
	m, err := resolveMode(mf, os.Getenv)

	return m.Mode == modeLocal, err
}

// The flush --hook deadlines, both from the start of the command and under
// the 5 s the harness allows the hook: the send stops at flushHookSend, and
// the hook waits until flushHookDeadline for the flush to finish its
// bookkeeping (attempts, backoff, releasing the claim) before it logs that
// the flush ran past its deadline and returns. Variables so tests can
// shorten them.
var (
	flushHookSend     = 3500 * time.Millisecond
	flushHookDeadline = 4500 * time.Millisecond
)

// flushHookHasDue is client.HasDue; a variable so tests can slow it down.
var flushHookHasDue = client.HasDue

// flushHookFlush is the hook's Flush; a variable so tests can stretch the
// bookkeeping a flush does after its send is cancelled.
var flushHookFlush = func(ctx context.Context, c *client.Client) client.FlushReport { return c.Flush(ctx) }

// runFlushHook is flush as a harness hook: nothing on stdout or stderr,
// never a failing exit. An empty spool returns before the config is read;
// any failure, the deadline included, is one error line in the client log.
func runFlushHook(mf modeFlags) {
	boundedHook(time.Now(), "flush --hook", mf)
}

// boundedHook runs flushHookBody under the flush --hook deadlines counted
// from start; a failure is one client-log error line whose reason starts
// with label.
func boundedHook(start time.Time, label string, mf modeFlags) {
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(flushHookSend))
	defer cancel()
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		return
	}
	data, err := dataDir(os.Getenv)
	if err != nil {
		return
	}
	fail := func(msg string) {
		client.LogTo(cache, client.Outcome{Outcome: client.OutcomeError, Reason: label + ": " + oneLine(msg)}, nowFunc(), io.Discard)
	}
	done := make(chan error, 1)
	go func() { done <- flushHookBody(ctx, data, mf) }()
	timer := time.NewTimer(time.Until(start.Add(flushHookDeadline)))
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil && !logSchemaTooNew(cache, err) {
			fail(err.Error())
		}
	case <-timer.C:
		fail("the flush ran past its deadline")
	}
}

// flushHookBody is the work boundedHook bounds; it returns the failure to
// log, or nil. An empty spool and an empty inbox return before the config
// is read; data is the data directory the spool and the inbox live in. In
// local mode the inbox is ingested after the flush, within the same
// deadline.
func flushHookBody(ctx context.Context, data string, mf modeFlags) error {
	if !flushHookHasDue(data, nowFunc()) && !inboxHasCandidates(data) {
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("the flush ran past its deadline")
	}
	skipStartupPass = true
	s, err := newSubmitter("flush", false, mf, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if _, off := s.disabled(); off {
		return nil
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	rep := flushHookFlush(ctx, c)
	switch {
	case rep.Stopped != "":
		return errors.New("the flush stopped: " + rep.Stopped)
	case ctx.Err() != nil:
		return errors.New("the flush ran past its deadline")
	}
	// The inbox goes last and only with a second to spare: a file it cannot
	// finish is left for the next command.
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < time.Second {
		return nil
	}
	if local, err := localMode(mf); err == nil && local {
		inboxPass(ctx, c, data)
	}

	return nil
}
