package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

// Session states.
const (
	StateNew               = "new"
	StateProcessed         = "processed"
	StateChanged           = "changed"
	StateAbsent            = "absent"
	StateDisabled          = "disabled"
	StateDenied            = "denied"
	StateUnknownProject    = "unknown-project"
	StateUnreadable        = "unreadable"
	StateUnsupportedFormat = "unsupported-format"
)

// Store states: whether a harness's session store is there. StateAbsent,
// StateUnreadable and StateUnsupportedFormat are shared with sessions.
const StorePresent = "present"

// Reasons of StateChanged.
const (
	ChangeAppended  = "appended"
	ChangeTruncated = "truncated"
	ChangeRotated   = "rotated"
)

// Mark outcomes.
const (
	OutcomeFiled   = "filed"
	OutcomeNothing = "nothing"
	OutcomeSkipped = "skipped"
)

const (
	// DetectorVersion is the version of the digest rules; it enters Key.
	DetectorVersion = "1"
	// Detector names the Claude Code reader and its version, for
	// context.detector of a submission built from a digest.
	Detector = "claude-code-jsonl/1"
	// MaxSessionBytes caps one session's digest JSON: events are kept from
	// the earliest until the cap, then Truncated is set.
	MaxSessionBytes = 64 << 10
	// MaxRunBytes caps the session digests of one run; sessions beyond it
	// are listed in DigestOutput.Deferred and get no watermark.
	MaxRunBytes = 256 << 10
	// ExcerptBytes caps an excerpt; SummaryBytes a prompt summary.
	ExcerptBytes = 240
	SummaryBytes = 160
	// SelectionKey is the triage_state key of the saved selection.
	SelectionKey = "selection"
)

// Env is everything the package takes from its caller's environment; it
// reads no environment variable itself.
type Env struct {
	// Home is the user's home directory.
	Home string
	// ClaudeConfigDir is $CLAUDE_CONFIG_DIR when set, else Home/.claude.
	ClaudeConfigDir string
	// Policy is the user config's [collect] table.
	Policy collect.Policy
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Open opens a session file for reading; nil means os.Open. Every
	// session file is opened through it.
	Open func(path string) (io.ReadCloser, error)
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

func (e Env) open(path string) (io.ReadCloser, error) {
	if e.Open != nil {
		return e.Open(path)
	}

	return os.Open(path)
}

// reader is one session log format.
type reader interface {
	harness() string
	// name is the registry's Sessions.Reader value.
	name() string
	location(env Env) string
	// candidates lists the session files without opening any.
	candidates(env Env) ([]candidate, []problem, error)
	// gate applies the user policy before a file is opened; "" allows it.
	gate(env Env, c candidate) (state, reason string)
	parse(r io.Reader) (*transcript, error)
}

// readers are the implemented readers, in listing order.
var readers = []reader{claudeReader{}}

func readerFor(harness string) (reader, bool) {
	for _, r := range readers {
		if r.harness() == harness {
			return r, true
		}
	}

	return nil, false
}

// Harnesses lists the harnesses with an implemented reader.
func Harnesses() []string {
	out := make([]string, len(readers))
	for i, r := range readers {
		out[i] = r.harness()
	}

	return out
}

// problem is a part of a present store that could not be read.
type problem struct {
	path   string
	dir    bool // a project directory that could not be listed
	reason string
}

func (p problem) String() string { return p.path + ": " + p.reason }

// hides reports whether the problem kept the session file path from being
// listed.
func (p problem) hides(path string) bool {
	if p.dir {
		return filepath.Dir(path) == p.path
	}

	return path == p.path
}

// hiddenBy returns the problem that kept path from being listed.
func hiddenBy(problems []problem, path string) (problem, bool) {
	for _, p := range problems {
		if p.hides(path) {
			return p, true
		}
	}

	return problem{}, false
}

// unlisted is the state and reason of a session file that was not listed.
func unlisted(problems []problem, path string) (state, reason string) {
	if p, ok := hiddenBy(problems, path); ok {
		if p.dir {
			return StateUnreadable, "project directory " + p.path + " could not be listed: " + p.reason
		}

		return StateUnreadable, p.reason
	}

	return StateAbsent, ""
}

type candidate struct {
	sessionID string
	path      string
	project   string // the store's per-project directory name
	mtime     time.Time
}

// Counts are a session's tallies.
type Counts struct {
	Entries    int `json:"entries"`
	Prompts    int `json:"prompts"`
	ToolCalls  int `json:"tool_calls"`
	ToolErrors int `json:"tool_errors"`
	Interrupts int `json:"interrupts"`
	Denials    int `json:"denials"`
	// Unparsed counts the complete lines that are not an entry.
	Unparsed int `json:"unparsed"`
}

// Store is one harness's session store. Problems lists the parts of a
// present store that could not be read ("<path>: <error>"); their sessions
// are not listed.
type Store struct {
	Harness  string   `json:"harness"`
	Location string   `json:"location"`
	State    string   `json:"state"`
	Problems []string `json:"problems,omitempty"`
}

// Session is one listed session. Project, Cwds, Start, End and Counts are
// empty when the file was not read or the session is refused after the
// read. Path, Project and Cwds are scrubbed.
type Session struct {
	Ref       string    `json:"ref"`
	Harness   string    `json:"harness"`
	SessionID string    `json:"session_id"`
	Path      string    `json:"path"`
	Project   string    `json:"project,omitempty"`
	Cwds      []string  `json:"cwds,omitempty"`
	Start     time.Time `json:"start,omitzero"`
	End       time.Time `json:"end,omitzero"`
	Counts    *Counts   `json:"counts,omitempty"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
}

// Listing is List's result.
type Listing struct {
	Stores   []Store   `json:"stores"`
	Sessions []Session `json:"sessions"`
}

// ListOptions select sessions. Harness "" means every harness with a
// reader; Since keeps sessions whose last entry (the file's mtime when it
// was not read) is not before it; Unprocessed keeps new and changed ones.
type ListOptions struct {
	Harness     string
	Since       time.Time
	Unprocessed bool
}

// evaluated is one candidate after the policy, the read and the watermark.
type evaluated struct {
	r      reader
	c      candidate
	state  string
	reason string
	tr     *transcript
	from   int64
	row    *store.SessionSeen
}

func (ev *evaluated) session() Session {
	s := Session{
		Ref: Ref(ev.r.harness(), ev.c.sessionID), Harness: ev.r.harness(), SessionID: ev.c.sessionID,
		Path: scrubString(ev.c.path), State: ev.state, Reason: ev.reason,
	}
	if ev.tr != nil {
		if len(ev.tr.cwds) > 0 {
			s.Project = scrubString(ev.tr.cwds[0])
		}
		for _, cwd := range ev.tr.cwds {
			s.Cwds = append(s.Cwds, scrubString(cwd))
		}
		s.Start, s.End = ev.tr.start, ev.tr.end
		c := ev.tr.total
		s.Counts = &c
	}

	return s
}

// scrubString is s with the known secret formats replaced.
func scrubString(s string) string {
	out, _ := scrub.String(s)

	return out
}

// lastSeen is the session's last entry time, the file's mtime when unread.
func (ev *evaluated) lastSeen() time.Time {
	if ev.tr != nil && !ev.tr.end.IsZero() {
		return ev.tr.end
	}

	return ev.c.mtime
}

// read applies the policy, reads the file and decides the state up to the
// watermark; row is the sessions_seen row, nil when none. Without full only
// the transcript's summary is kept, not its entries. A session refused
// after the read keeps nothing of it.
func read(env Env, r reader, c candidate, row *store.SessionSeen, full bool) *evaluated {
	ev := &evaluated{r: r, c: c, row: row}
	if ev.state, ev.reason = r.gate(env, c); ev.state != "" {
		return ev
	}
	f, err := env.open(c.path)
	if err != nil {
		ev.state, ev.reason = StateUnreadable, ioReason(err)

		return ev
	}
	tr, err := r.parse(f)
	_ = f.Close()
	if err != nil {
		ev.state, ev.reason = StateUnreadable, ioReason(err)

		return ev
	}
	if tr.parsed == 0 {
		ev.state = StateUnsupportedFormat

		return ev
	}
	if state, reason := cwdPolicy(env, tr.cwds); state != "" {
		ev.state, ev.reason = state, reason

		return ev
	}
	tr.summarise(full)
	ev.tr = tr
	if len(tr.cwds) == 0 {
		ev.state = StateUnknownProject

		return ev
	}
	ev.state, ev.reason, ev.from = watermark(tr, row)

	return ev
}

func ioReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}

	return err.Error()
}

// cwdPolicy checks every recorded working directory: one disabled makes the
// session disabled, otherwise one denied makes it denied.
func cwdPolicy(env Env, cwds []string) (state, reason string) {
	for _, cwd := range cwds {
		var d collect.Decision
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			d = collect.Check(cwd, env.Home, env.Policy)
		} else {
			d = collect.CheckUser(cwd, env.Home, env.Policy)
		}
		if !d.Disabled {
			continue
		}
		switch d.Reason {
		case collect.ReasonDisabled, collect.ReasonRepoDisabled:
			return StateDisabled, d.Reason
		default:
			if state == "" {
				state, reason = StateDenied, d.Reason
			}
		}
	}

	return state, reason
}

// watermark compares the read file with the marked watermark by first-line
// hash and complete-line size only. The store is taken as append-only: a
// rewrite that keeps the first line and the size, or grows it, is not
// detected (a larger one is read as an append). processed_mtime is recorded
// but does not enter the state.
func watermark(tr *transcript, row *store.SessionSeen) (state, reason string, from int64) {
	if row == nil || row.ProcessedAt == nil || row.ProcessedSize == nil {
		return StateNew, "", 0
	}
	switch {
	case tr.head != row.ProcessedHead:
		return StateChanged, ChangeRotated, 0
	case tr.size < *row.ProcessedSize:
		return StateChanged, ChangeTruncated, 0
	case tr.size > *row.ProcessedSize:
		return StateChanged, ChangeAppended, *row.ProcessedSize
	}

	return StateProcessed, "", 0
}

// Ref is the ref of a session: "<harness>:<session id>".
func Ref(harness, sessionID string) string { return harness + ":" + sessionID }

// ParseRef splits a session ref; the harness must have a reader and the
// session id must be a plain file stem.
func ParseRef(ref string) (harness, sessionID string, err error) {
	harness, sessionID, ok := strings.Cut(ref, ":")
	if !ok || sessionID == "" {
		return "", "", fmt.Errorf("session ref %q: want <harness>:<session id>", ref)
	}
	if _, ok := readerFor(harness); !ok {
		return "", "", fmt.Errorf("session ref %q: no session reader for harness %q (have %s)", ref, harness, strings.Join(Harnesses(), ", "))
	}
	if strings.ContainsAny(sessionID, `/\#`) || sessionID == "." || sessionID == ".." {
		return "", "", fmt.Errorf("session ref %q: invalid session id", ref)
	}

	return harness, sessionID, nil
}

func selectReaders(harness string) ([]reader, error) {
	if harness == "" {
		return readers, nil
	}
	r, ok := readerFor(harness)
	if !ok {
		return nil, fmt.Errorf("no session reader for harness %q (have %s)", harness, strings.Join(Harnesses(), ", "))
	}

	return []reader{r}, nil
}

func rowsByID(ctx context.Context, db *store.DB, harness string) (map[string]*store.SessionSeen, error) {
	out := map[string]*store.SessionSeen{}
	err := db.Read(ctx, func(q store.Querier) error {
		rows, err := store.ListSessions(ctx, q, harness)
		for i := range rows {
			out[rows[i].SessionID] = &rows[i]
		}

		return err
	})

	return out, err
}

// storeState lists a reader's candidates and words the store's state; its
// problems are scrubbed.
func storeState(env Env, r reader) (Store, []candidate, []problem) {
	st := Store{Harness: r.harness(), Location: r.location(env), State: StorePresent}
	cands, problems, err := r.candidates(env)
	for _, p := range problems {
		st.Problems = append(st.Problems, scrubString(p.String()))
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st.State = StateAbsent
	case err != nil:
		st.State = StateUnreadable
	}

	return st, cands, problems
}

// evaluateAll reads every candidate of r, keeping each transcript's summary
// only, and adds the rows whose file was not listed: unreadable when a
// problem of the store hid it, else absent.
func evaluateAll(ctx context.Context, db *store.DB, env Env, r reader, since time.Time) (Store, []*evaluated, []Session, error) {
	st, cands, problems := storeState(env, r)
	rows, err := rowsByID(ctx, db, r.harness())
	if err != nil {
		return st, nil, nil, err
	}
	var evs []*evaluated
	seen := map[string]bool{}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return st, nil, nil, err
		}
		seen[c.sessionID] = true
		// The file's mtime bounds its last entry: skip it unopened.
		if !since.IsZero() && c.mtime.Before(since) {
			continue
		}
		ev := read(env, r, c, rows[c.sessionID], false)
		if !since.IsZero() && ev.lastSeen().Before(since) {
			continue
		}
		evs = append(evs, ev)
	}
	var absent []Session
	for id, row := range rows {
		if seen[id] {
			continue
		}
		state, reason := unlisted(problems, row.Path)
		absent = append(absent, Session{Ref: Ref(r.harness(), id), Harness: r.harness(), SessionID: id, Path: scrubString(row.Path), State: state, Reason: reason})
	}

	return st, evs, absent, nil
}

// List returns the session stores and sessions selected by opts, oldest
// last entry first.
func List(ctx context.Context, db *store.DB, env Env, opts ListOptions) (Listing, error) {
	rs, err := selectReaders(opts.Harness)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Stores: []Store{}, Sessions: []Session{}}
	type keyed struct {
		s    Session
		when time.Time
	}
	var all []keyed
	for _, r := range rs {
		st, evs, absent, err := evaluateAll(ctx, db, env, r, opts.Since)
		if err != nil {
			return Listing{}, err
		}
		out.Stores = append(out.Stores, st)
		for _, ev := range evs {
			all = append(all, keyed{ev.session(), ev.lastSeen()})
		}
		for _, s := range absent {
			all = append(all, keyed{s, time.Time{}})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].when.Equal(all[j].when) {
			return all[i].when.Before(all[j].when)
		}

		return all[i].s.Ref < all[j].s.Ref
	})
	for _, k := range all {
		if opts.Unprocessed && k.s.State != StateNew && k.s.State != StateChanged {
			continue
		}
		out.Sessions = append(out.Sessions, k.s)
	}

	return out, nil
}

// Mark records outcome (OutcomeFiled, OutcomeNothing or OutcomeSkipped) and
// the uids of the submissions filed for each ref at env's clock, copying each session's
// digest watermark. All refs are marked in one transaction or none is; a
// session not digested yet fails with an error wrapping
// store.ErrNotDigested, a uid not in the submission uid form with one
// wrapping ErrInvalidUID.
func Mark(ctx context.Context, db *store.DB, env Env, refs []string, outcome string, uids []string) error {
	switch outcome {
	case OutcomeFiled, OutcomeNothing, OutcomeSkipped:
	default:
		return fmt.Errorf("outcome %q: want %s, %s or %s", outcome, OutcomeFiled, OutcomeNothing, OutcomeSkipped)
	}
	if len(refs) == 0 {
		return errors.New("mark: no session ref given")
	}
	if err := CheckUIDs(uids); err != nil {
		return err
	}
	if uids == nil {
		uids = []string{}
	}
	refsJSON, err := json.Marshal(uids)
	if err != nil {
		return err
	}
	type target struct{ ref, harness, id string }
	targets := make([]target, 0, len(refs))
	for _, ref := range refs {
		h, id, err := ParseRef(ref)
		if err != nil {
			return err
		}
		targets = append(targets, target{ref, h, id})
	}
	now := env.now().UnixMicro()

	return db.Write(ctx, func(q store.Querier) error {
		for _, t := range targets {
			if err := store.MarkSession(ctx, q, t.harness, t.id, now, outcome, refsJSON); err != nil {
				if errors.Is(err, store.ErrNotDigested) {
					return fmt.Errorf("%s: %w; run sessions digest on it first", t.ref, err)
				}

				return err
			}
		}

		return nil
	})
}

// ErrInvalidUID: a uid given to Mark is not a submission uid.
var ErrInvalidUID = errors.New("not a submission uid")

// CheckUIDs checks that every uid has the form of a submission uid (an RFC
// 9562 UUID in 8-4-4-4-12 hex form).
func CheckUIDs(uids []string) error {
	for _, u := range uids {
		if !core.ValidUID(u) {
			return fmt.Errorf("uid %q: %w; pass the uid submit printed", u, ErrInvalidUID)
		}
	}

	return nil
}

// Selection is the processor's saved choice of sessions.
type Selection struct {
	Harnesses []string `json:"harnesses"`
	Since     string   `json:"since"`
	Limit     int      `json:"limit"`
}

// HarnessStatus counts one harness's sessions per state.
type HarnessStatus struct {
	Harness  string         `json:"harness"`
	Location string         `json:"location"`
	Store    string         `json:"store"`
	Problems []string       `json:"problems,omitempty"`
	States   map[string]int `json:"states"`
}

// StatusOutput is Status's result; Selection is nil when none is saved.
type StatusOutput struct {
	Harnesses []HarnessStatus `json:"harnesses"`
	Selection *Selection      `json:"selection,omitempty"`
}

// Status counts the sessions of every harness with a reader per state and
// returns the saved selection.
func Status(ctx context.Context, db *store.DB, env Env) (StatusOutput, error) {
	out := StatusOutput{Harnesses: []HarnessStatus{}}
	for _, r := range readers {
		st, evs, absent, err := evaluateAll(ctx, db, env, r, time.Time{})
		if err != nil {
			return StatusOutput{}, err
		}
		hs := HarnessStatus{Harness: r.harness(), Location: st.Location, Store: st.State, Problems: st.Problems, States: map[string]int{}}
		for _, ev := range evs {
			hs.States[ev.state]++
		}
		for _, s := range absent {
			hs.States[s.State]++
		}
		out.Harnesses = append(out.Harnesses, hs)
	}
	var raw string
	var ok bool
	if err := db.Read(ctx, func(q store.Querier) error {
		var err error
		raw, ok, err = store.TriageState(ctx, q, SelectionKey)

		return err
	}); err != nil {
		return StatusOutput{}, err
	}
	if ok {
		var sel Selection
		if err := json.Unmarshal([]byte(raw), &sel); err != nil {
			return StatusOutput{}, fmt.Errorf("saved selection: %w", err)
		}
		out.Selection = &sel
	}

	return out, nil
}

// SetSelection saves sel. Every harness must have a reader and Limit must
// not be negative.
func SetSelection(ctx context.Context, db *store.DB, sel Selection) error {
	for _, h := range sel.Harnesses {
		if !slices.Contains(Harnesses(), h) {
			return fmt.Errorf("selection: no session reader for harness %q", h)
		}
	}
	if sel.Limit < 0 {
		return fmt.Errorf("selection: limit must not be negative, got %d", sel.Limit)
	}
	if sel.Harnesses == nil {
		sel.Harnesses = []string{}
	}
	b, err := json.Marshal(sel)
	if err != nil {
		return err
	}

	return db.Write(ctx, func(q store.Querier) error { return store.SetTriageState(ctx, q, SelectionKey, string(b)) })
}

// Key is the idempotency key of a submission built from one finding of a
// session: "session-scan-" and the SHA-256 hex of harness, session id,
// span, ordinal and detector version joined by NUL bytes.
func Key(harness, sessionID, span string, ordinal int, detectorVersion string) string {
	sum := sha256.Sum256([]byte(harness + "\x00" + sessionID + "\x00" + span + "\x00" + strconv.Itoa(ordinal) + "\x00" + detectorVersion))

	return "session-scan-" + hex.EncodeToString(sum[:])
}
