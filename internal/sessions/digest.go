package sessions

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

// Notice heads every digest.
const Notice = "This digest is shown to the model that reads it and so reaches that model's provider; agentfeedback itself stores and sends none of it."

// timeLayout is the digest's timestamp form: UTC, milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z"

// flagPhrases are the prompt phrases a digest flags, matched without case
// on word boundaries.
var flagPhrases = []string{"doesn't work", "does not work", "still failing", "still broken", "wrong", "why did you",
	"that's not", "not what i", "broken", "again"}

var flagRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(flagPhrases))
	for i, p := range flagPhrases {
		out[i] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(p) + `\b`)
	}

	return out
}()

var exitLine = regexp.MustCompile(`^Exit code (-?\d+)\s*$`)

// DigestRequest selects the sessions to digest: Refs, or with Unprocessed
// the new and changed sessions of Harness ("" for all) up to Limit (0 for
// no limit). AllowUnknownProject admits sessions that record no cwd.
type DigestRequest struct {
	Refs                []string
	Unprocessed         bool
	Harness             string
	Limit               int
	AllowUnknownProject bool
}

// Event is a prompt or a tool call that did not succeed.
type Event struct {
	Span       string `json:"span"`
	Ref        string `json:"ref"`
	At         string `json:"at,omitempty"`
	Type       string `json:"type"`
	Summary    string `json:"summary,omitempty"`
	Tool       string `json:"tool,omitempty"`
	ArgsDigest string `json:"args_digest,omitempty"`
	Status     string `json:"status,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
	// Self marks a tool call that ran agentfeedback itself, by the rule
	// detect applies to hook events: its command runs agentfeedback, or
	// the tool's name contains it.
	Self bool `json:"self,omitempty"`
}

// Retry is a tool call repeating an earlier failed one (same tool and
// arguments digest).
type Retry struct {
	Span       string `json:"span"`
	OfSpan     string `json:"of_span"`
	Tool       string `json:"tool"`
	ArgsDigest string `json:"args_digest"`
}

// Flag is a prompt that contains one of the flagged phrases.
type Flag struct {
	Span   string `json:"span"`
	Phrase string `json:"phrase"`
}

// SessionDigest is the digest of one session from FromOffset on.
type SessionDigest struct {
	Ref          string   `json:"ref"`
	Harness      string   `json:"harness"`
	SessionID    string   `json:"session_id"`
	Project      string   `json:"project"`
	Cwd          string   `json:"cwd"`
	Start        string   `json:"start,omitempty"`
	End          string   `json:"end,omitempty"`
	Model        string   `json:"model,omitempty"`
	FromOffset   int64    `json:"from_offset"`
	Truncated    bool     `json:"truncated"`
	Counts       Counts   `json:"counts"`
	Events       []Event  `json:"events"`
	Retries      []Retry  `json:"retries"`
	Denials      []string `json:"denials"`
	Flagged      []Flag   `json:"flagged"`
	FinalExcerpt string   `json:"final_excerpt,omitempty"`
}

// DigestError is a session the digest refused or could not find.
type DigestError struct {
	Ref     string `json:"ref"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// DigestOutput is Digest's result.
type DigestOutput struct {
	Notice          string          `json:"notice"`
	DetectorVersion string          `json:"detector_version"`
	Sessions        []SessionDigest `json:"sessions"`
	Errors          []DigestError   `json:"errors"`
	// Deferred are refs left out to stay under MaxRunBytes; they keep
	// their watermark. Error refs and deferred refs are scrubbed.
	Deferred []string `json:"deferred"`
}

// RefusedError is a session whose state forbids reading it.
type RefusedError struct {
	Ref    string
	State  string
	Reason string
}

func (e *RefusedError) Error() string {
	msg := e.Ref + ": session is " + e.State
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}

	return msg
}

// ErrSessionNotFound: no session file carries the ref's session id.
var ErrSessionNotFound = errors.New("session not found")

// ErrSpanNotFound: the session has no entry with the span.
var ErrSpanNotFound = errors.New("span not found in session")

// find locates the candidate of one session id among cands; the policy is
// not applied here.
func find(cands []candidate, sessionID string) (candidate, bool) {
	for _, c := range cands {
		if c.sessionID == sessionID {
			return c, true
		}
	}

	return candidate{}, false
}

// listing is one reader's candidates, problems and error, listed once per
// run.
type listing struct {
	cands    []candidate
	problems []problem
	err      error
}

// refusedStore is the state of every session of a store whose candidates
// could not be listed: unsupported-format or unreadable, "" when the store
// is absent or was listed.
func refusedStore(err error) string {
	switch {
	case err == nil || errors.Is(err, fs.ErrNotExist):
		return ""
	case errors.Is(err, errUnsupported):
		return StateUnsupportedFormat
	default:
		return StateUnreadable
	}
}

// Digest builds the digest of the sessions req selects and records the
// digest watermark of every session it includes. Refused sessions
// (disabled, denied, absent, unreadable, unsupported-format, and
// unknown-project unless AllowUnknownProject) are reported in Errors and
// do not fail the call. Sessions are selected on their summary and read
// again in full when digested.
func Digest(ctx context.Context, db *store.DB, env Env, req DigestRequest) (DigestOutput, error) {
	out := DigestOutput{Notice: Notice, DetectorVersion: DetectorVersion, Sessions: []SessionDigest{}, Errors: []DigestError{}, Deferred: []string{}}

	var evs []*evaluated
	if req.Unprocessed {
		if len(req.Refs) > 0 {
			return DigestOutput{}, errors.New("digest: give refs or unprocessed, not both")
		}
		rs, err := selectReaders(req.Harness)
		if err != nil {
			return DigestOutput{}, err
		}
		for _, r := range rs {
			_, all, _, err := evaluateAll(ctx, db, env, r, time.Time{})
			if err != nil {
				return DigestOutput{}, err
			}
			for _, ev := range all {
				if ev.state == StateNew || ev.state == StateChanged || (ev.state == StateUnknownProject && req.AllowUnknownProject) {
					evs = append(evs, ev)
				}
			}
		}
		sortEvaluated(evs)
		if req.Limit > 0 && len(evs) > req.Limit {
			evs = evs[:req.Limit]
		}
		for i, ev := range evs {
			if err := ctx.Err(); err != nil {
				return DigestOutput{}, err
			}
			evs[i] = read(env, ev.r, ev.c, ev.row, true)
		}
	} else {
		listed := map[string]listing{}
		for _, ref := range req.Refs {
			if err := ctx.Err(); err != nil {
				return DigestOutput{}, err
			}
			h, id, err := ParseRef(ref)
			if err != nil {
				return DigestOutput{}, err
			}
			r, _ := readerFor(h)
			var row *store.SessionSeen
			if err := db.Read(ctx, func(q store.Querier) error {
				s, ok, err := store.GetSession(ctx, q, h, id)
				if ok {
					row = &s
				}

				return err
			}); err != nil {
				return DigestOutput{}, err
			}
			l, ok := listed[h]
			if !ok {
				l.cands, l.problems, l.err = r.candidates(env)
				listed[h] = l
			}
			if state := refusedStore(l.err); state != "" {
				out.Errors = append(out.Errors, DigestError{Ref: ref, State: state, Message: (&RefusedError{Ref: ref, State: state, Reason: l.err.Error()}).Error()})

				continue
			}
			c, ok := find(l.cands, id)
			if !ok {
				state, msg := StateAbsent, "no session file carries this session id"
				if row != nil {
					msg = "the session file is gone"
					if st, reason := unlisted(l.problems, row.Path); st != StateAbsent {
						state, msg = st, reason
					}
				}
				out.Errors = append(out.Errors, DigestError{Ref: ref, State: state, Message: msg})

				continue
			}
			ev := read(env, r, c, row, true)
			if ev.state == StateProcessed {
				// Asked for by name: digest the whole file again.
				ev.from = 0
			}
			evs = append(evs, ev)
		}
	}

	var marks []store.SessionDigest
	total := 0
	now := env.now().UnixMicro()
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			return DigestOutput{}, err
		}
		ref := Ref(ev.r.harness(), ev.c.sessionID)
		switch ev.state {
		case StateNew, StateChanged, StateProcessed:
		case StateUnknownProject:
			if req.AllowUnknownProject {
				break
			}
			out.Errors = append(out.Errors, DigestError{Ref: ref, State: ev.state, Message: "the session records no working directory, so no project policy can be checked; allow unknown projects to digest it"})

			continue
		default:
			out.Errors = append(out.Errors, DigestError{Ref: ref, State: ev.state, Message: (&RefusedError{Ref: ref, State: ev.state, Reason: ev.reason}).Error()})

			continue
		}
		sd, size, covered, err := buildDigest(ev)
		if err != nil {
			return DigestOutput{}, err
		}
		if total+size > MaxRunBytes {
			out.Deferred = append(out.Deferred, scrubString(ref))

			continue
		}
		total += size
		out.Sessions = append(out.Sessions, sd)
		marks = append(marks, store.SessionDigest{
			Harness: ev.r.harness(), SessionID: ev.c.sessionID, Path: ev.c.path,
			Mtime: ev.c.mtime.UnixNano(), Size: covered, Head: ev.tr.head, DigestedAt: now,
		})
	}
	for i := range out.Errors {
		out.Errors[i].Ref = scrubString(out.Errors[i].Ref)
		out.Errors[i].Message = scrubString(out.Errors[i].Message)
	}
	if len(marks) > 0 {
		if err := db.Write(ctx, func(q store.Querier) error {
			for _, m := range marks {
				if err := store.UpsertSessionDigest(ctx, q, m); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			return DigestOutput{}, err
		}
	}

	return out, nil
}

func sortEvaluated(evs []*evaluated) {
	slices.SortStableFunc(evs, func(a, b *evaluated) int {
		if c := a.lastSeen().Compare(b.lastSeen()); c != 0 {
			return c
		}

		return strings.Compare(Ref(a.r.harness(), a.c.sessionID), Ref(b.r.harness(), b.c.sessionID))
	})
}

// spanned is one item of a digest list with the offset of its entry and
// the offset of the line that completes it (an event's prompt line, or the
// line carrying a tool call's result). owner is, for the other lists, the
// index of the event the item goes with: a denial's or a flag's own event,
// the failed event a retry repeats.
type spanned[T any] struct {
	offset int64
	done   int64
	owner  int
	v      T
	size   int // JSON bytes and a separator, once scrubbed
}

// buildDigest assembles, scrubs and caps one session's digest; size is the
// length of its JSON and covered the watermark size it records: the
// complete-line size, or when events were left out to stay under the cap,
// the completion offset of the first one left out. Under the cap events are
// kept in order of completion, whole groups completed by one line at a
// time, so covered lies past every kept event and the next incremental
// digest reports the rest. A retry is kept with the failed event it
// repeats, so a pair split by the cap may go unreported.
func buildDigest(ev *evaluated) (SessionDigest, int, int64, error) {
	tr := ev.tr
	ref := Ref(ev.r.harness(), ev.c.sessionID)
	sd := SessionDigest{
		Ref: ref, Harness: ev.r.harness(), SessionID: ev.c.sessionID, Model: tr.lastModel,
		FromOffset: ev.from, Counts: tr.counts(ev.from),
	}
	if len(tr.cwds) > 0 {
		sd.Project, sd.Cwd = tr.cwds[0], tr.lastCwd
	}
	if !tr.start.IsZero() {
		sd.Start, sd.End = tr.start.Format(timeLayout), tr.end.Format(timeLayout)
	}

	var events []spanned[Event]
	var retries []spanned[Retry]
	var denials []spanned[string]
	var flagged []spanned[Flag]
	failed := map[string]int{} // tool + "\x00" + args digest -> event index
	for _, e := range tr.entries {
		// An entry before from still yields the tool calls whose result
		// came at or after it.
		in := e.offset >= ev.from
		at := ""
		if !e.at.IsZero() {
			at = e.at.Format(timeLayout)
		}
		switch {
		case !in:
		case e.kind == kindPrompt:
			events = append(events, spanned[Event]{offset: e.offset, done: e.offset, v: Event{
				Span: e.span, Ref: ref + "#" + e.span, At: at, Type: "prompt", Summary: summary(e.text),
			}})
			for i, re := range flagRes {
				if re.MatchString(e.text) {
					flagged = append(flagged, spanned[Flag]{offset: e.offset, owner: len(events) - 1, v: Flag{Span: e.span, Phrase: flagPhrases[i]}})
				}
			}
		case e.kind == kindAssistantText:
			sd.FinalExcerpt = excerpt(e.text)
		case e.kind == kindToolCall:
			if e.text != "" {
				sd.FinalExcerpt = excerpt(e.text)
			}
		}
		for _, c := range e.calls {
			digest := argsDigest(c)
			key := c.tool + "\x00" + digest
			if of, ok := failed[key]; ok && in {
				retries = append(retries, spanned[Retry]{offset: e.offset, owner: of, v: Retry{Span: e.span, OfSpan: events[of].v.Span, Tool: c.tool, ArgsDigest: digest}})
			}
			if !c.result || c.status == statusOK || (!in && c.resultOffset < ev.from) {
				continue
			}
			failed[key] = len(events)
			ev := Event{
				Span: e.span, Ref: ref + "#" + e.span, At: at, Type: "tool_call", Tool: c.tool, ArgsDigest: digest,
				Status: c.status, Excerpt: excerpt(c.content),
				Self: (c.hasCommand && detect.IsSelf(c.command)) || strings.Contains(strings.ToLower(c.tool), "agentfeedback"),
			}
			switch c.status {
			case statusDenied:
				ev.ErrorClass = "denied"
				denials = append(denials, spanned[string]{offset: e.offset, done: c.resultOffset, owner: len(events), v: e.span})
			case statusInterrupted:
				ev.ErrorClass = "interrupted"
			default:
				// A native exit code goes before the "Exit code N" first
				// line of the result; a native 0 on a failed call is no
				// exit class.
				ev.ErrorClass = "tool_error"
				if c.exitCode != nil {
					if *c.exitCode == 0 {
						break
					}
					n := *c.exitCode
					ev.ExitCode, ev.ErrorClass = &n, "exit"

					break
				}
				first, _, _ := strings.Cut(c.content, "\n")
				if m := exitLine.FindStringSubmatch(first); m != nil {
					if n, err := strconv.Atoi(m[1]); err == nil {
						ev.ExitCode, ev.ErrorClass = &n, "exit"
					}
				}
			}
			events = append(events, spanned[Event]{offset: e.offset, done: c.resultOffset, v: ev})
		}
	}

	// Every item is scrubbed on its own and sized; the cap then keeps the
	// events completed first and the items that go with them.
	if err := scrubItems(events, retries, denials, flagged); err != nil {
		return SessionDigest{}, 0, 0, err
	}
	head := sd
	head.Events, head.Retries, head.Denials, head.Flagged = []Event{}, []Retry{}, []string{}, []Flag{}
	if _, err := scrubbed(&head); err != nil {
		return SessionDigest{}, 0, 0, err
	}
	kept := make([]bool, len(events))
	assemble := func(truncated bool) ([]byte, SessionDigest, error) {
		out := head
		out.Events = take(events, kept, true)
		out.Retries = take(retries, kept, false)
		out.Denials = take(denials, kept, false)
		out.Flagged = take(flagged, kept, false)
		out.Truncated = truncated
		b, err := json.Marshal(out)

		return b, out, err
	}
	for i := range kept {
		kept[i] = true
	}
	b, out, err := assemble(false)
	if err != nil {
		return SessionDigest{}, 0, 0, err
	}
	if len(b) <= MaxSessionBytes {
		return out, len(b), tr.size, nil
	}

	// Events in order of completion, cut into groups completed by one line.
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(events[a].done, events[b].done) })
	var groups [][]int
	for i, idx := range order {
		if i == 0 || events[idx].done != events[order[i-1]].done {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], idx)
	}
	extra := make([]int, len(events)) // size of the items that go with each event
	for _, it := range retries {
		extra[it.owner] += it.size
	}
	for _, it := range denials {
		extra[it.owner] += it.size
	}
	for _, it := range flagged {
		extra[it.owner] += it.size
	}

	// Greedy: add whole groups while the estimated size fits.
	base, err := json.Marshal(func() SessionDigest { h := head; h.Truncated = true; return h }())
	if err != nil {
		return SessionDigest{}, 0, 0, err
	}
	size := len(base)
	k := 0
	for k < len(groups) {
		add := 0
		for _, idx := range groups[k] {
			add += events[idx].size + extra[idx]
		}
		if size+add > MaxSessionBytes {
			break
		}
		size += add
		k++
	}
	for {
		for i := range kept {
			kept[i] = false
		}
		for _, g := range groups[:k] {
			for _, idx := range g {
				kept[idx] = true
			}
		}
		b, out, err = assemble(true)
		if err != nil {
			return SessionDigest{}, 0, 0, err
		}
		if len(b) <= MaxSessionBytes {
			return out, len(b), events[groups[k][0]].done, nil
		}
		if k == 0 {
			return SessionDigest{}, 0, 0, fmt.Errorf("digest of %s: %d bytes without events, over the %d byte cap", ref, len(b), MaxSessionBytes)
		}
		k--
	}
}

// take returns the items kept: the events themselves (self) or the items
// whose owner is kept.
func take[T any](items []spanned[T], kept []bool, self bool) []T {
	out := []T{}
	for i, it := range items {
		if (self && kept[i]) || (!self && kept[it.owner]) {
			out = append(out, it.v)
		}
	}

	return out
}

// scrubItems scrubs every item in place and records its JSON size plus a
// separator.
func scrubItems(events []spanned[Event], retries []spanned[Retry], denials []spanned[string], flagged []spanned[Flag]) error {
	for i := range events {
		if err := scrubOne(&events[i]); err != nil {
			return err
		}
	}
	for i := range retries {
		if err := scrubOne(&retries[i]); err != nil {
			return err
		}
	}
	for i := range denials {
		if err := scrubOne(&denials[i]); err != nil {
			return err
		}
	}
	for i := range flagged {
		if err := scrubOne(&flagged[i]); err != nil {
			return err
		}
	}

	return nil
}

func scrubOne[T any](it *spanned[T]) error {
	b, err := scrubJSON(&it.v)
	it.size = len(b) + 1

	return err
}

// scrubJSON scrubs every string of *v in place and returns its JSON.
func scrubJSON[T any](v *T) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if s, ok := tree.(string); ok {
		s, _ = scrub.String(s)
		tree = s
	} else {
		scrub.Tree(tree)
	}
	b, err = json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	*v = out

	return json.Marshal(v)
}

// scrubbed scrubs every string of sd in place and returns its JSON.
func scrubbed(sd *SessionDigest) ([]byte, error) { return scrubJSON(sd) }

// argsDigest is detect.CommandDigest of the input's command when it has
// one, else the first 12 hex digits of the SHA-256 of the compact input.
func argsDigest(c *call) string {
	if c.hasCommand {
		return detect.CommandDigest(c.command)
	}
	var buf bytes.Buffer
	in := []byte(c.input)
	if json.Compact(&buf, in) == nil {
		in = buf.Bytes()
	}
	sum := sha256.Sum256(in)

	return hex.EncodeToString(sum[:])[:12]
}

// summary is the first non-empty line of a prompt, scrubbed, at most
// SummaryBytes.
func summary(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			s, _ := scrub.String(l)

			return cut(s, SummaryBytes)
		}
	}

	return ""
}

// excerpt is text trimmed and scrubbed, at most ExcerptBytes. Scrubbing
// comes first so a cut never leaves part of a secret unrecognised.
func excerpt(text string) string {
	s, _ := scrub.String(strings.TrimSpace(text))

	return cut(s, ExcerptBytes)
}

// cut shortens s to at most n bytes without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}

	return s[:i]
}

// SpanFacts are the facts of one session entry.
type SpanFacts struct {
	Harness   string    `json:"harness"`
	SessionID string    `json:"session_id"`
	Span      string    `json:"span"`
	At        time.Time `json:"at,omitzero"`
	Model     string    `json:"model,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	CwdExists bool      `json:"cwd_exists"`
	// Detector is DetectorFor the harness.
	Detector string `json:"detector,omitempty"`
	// State is StateUnknownProject when the session records no cwd, else
	// "".
	State string `json:"state,omitempty"`
}

// Locate re-reads the session ref names, under the same policy as List,
// and returns the facts of its entry span: the entry's time and cwd, and
// the model of the last assistant entry at or before it. A session that is
// disabled, denied, unreadable or unsupported fails with *RefusedError; a
// missing session with ErrSessionNotFound, a missing span with
// ErrSpanNotFound.
func Locate(ctx context.Context, env Env, ref, span string) (SpanFacts, error) {
	if err := ctx.Err(); err != nil {
		return SpanFacts{}, err
	}
	h, id, err := ParseRef(ref)
	if err != nil {
		return SpanFacts{}, err
	}
	if span == "" {
		return SpanFacts{}, fmt.Errorf("%s: %w: empty span", ref, ErrSpanNotFound)
	}
	r, _ := readerFor(h)
	cands, _, err := r.candidates(env)
	if state := refusedStore(err); state != "" {
		return SpanFacts{}, &RefusedError{Ref: ref, State: state, Reason: err.Error()}
	}
	c, ok := find(cands, id)
	if !ok {
		return SpanFacts{}, fmt.Errorf("%s: %w", ref, ErrSessionNotFound)
	}
	ev := read(env, r, c, nil, true)
	switch ev.state {
	case StateNew, StateUnknownProject:
	default:
		return SpanFacts{}, &RefusedError{Ref: ref, State: ev.state, Reason: ev.reason}
	}
	facts := SpanFacts{Harness: h, SessionID: id, Span: span, Detector: DetectorFor(h)}
	if ev.state == StateUnknownProject {
		facts.State = StateUnknownProject
	}
	model := ""
	for _, e := range ev.tr.entries {
		if e.model != "" {
			model = e.model
		}
		if e.span != span {
			continue
		}
		facts.At, facts.Model, facts.Cwd = e.at, model, e.cwd
		if e.cwd != "" {
			if info, err := os.Stat(e.cwd); err == nil && info.IsDir() {
				facts.CwdExists = true
			}
		}

		return facts, nil
	}

	return SpanFacts{}, fmt.Errorf("%s: %w: %s", ref, ErrSpanNotFound, span)
}
