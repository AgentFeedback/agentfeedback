package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/pkg/client"
	"github.com/agentfeedback/agentfeedback/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

const (
	// runTSLayout is meta.json's run_ts, in the runner's local time.
	runTSLayout = "20060102-150405"
	// sweepMinAge: younger runs are the runner's scoring hook's to submit.
	sweepMinAge = 2 * time.Hour
	// submittedMarker holds the id of the row a run dir was filed as.
	submittedMarker = ".submitted"
	envReviewLogDir = "REVIEW_LOG_DIR"
	envReviewDirs   = "AGENT_FEEDBACK_REVIEW_DIRS"
)

// reviewLocation is the zone run_ts is read in; a variable so tests can fix
// it.
var reviewLocation = time.Local

// jsonNumber is a JSON number literal.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// reviewSkip is why a run dir is not submitted.
type reviewSkip struct {
	reason, message string
}

// reviewMeta is the runner's meta.json.
type reviewMeta struct {
	Machine string                     `json:"machine"`
	Skill   string                     `json:"skill"`
	RunTS   string                     `json:"run_ts"`
	Caller  string                     `json:"caller"`
	Slots   map[string]json.RawMessage `json:"slots"`
}

// label is the scorecard label of slot, "" when there is none.
func (m reviewMeta) label(slot string) string {
	var s struct {
		Label string `json:"label"`
	}
	if raw, ok := m.Slots[slot]; ok && json.Unmarshal(raw, &s) == nil {
		return s.Label
	}

	return ""
}

// scoreRow is one scorecards.tsv row of the run; a nil field is absent.
type scoreRow struct {
	label                 string
	score, valid, invalid json.RawMessage
	note                  string
}

// reviewer is one payload.reviewers row, as the bash client builds it.
type reviewer struct {
	Slot      string          `json:"slot"`
	Model     string          `json:"model"`
	Status    string          `json:"status"`
	DurationS json.RawMessage `json:"duration_s,omitempty"`
	Bytes     json.RawMessage `json:"bytes,omitempty"`
	Output    string          `json:"output,omitempty"`
	Score     json.RawMessage `json:"score,omitempty"`
	Valid     json.RawMessage `json:"valid,omitempty"`
	Invalid   json.RawMessage `json:"invalid,omitempty"`
	Note      string          `json:"note,omitempty"`
}

type reviewGrader struct {
	Model string `json:"model"`
}

type reviewPayload struct {
	Skill     string        `json:"skill"`
	Grader    *reviewGrader `json:"grader,omitempty"`
	Reviewers []reviewer    `json:"reviewers"`
	Prompt    string        `json:"prompt,omitempty"`
}

type reviewBody struct {
	Kind       string            `json:"kind"`
	Key        string            `json:"key"`
	Summary    string            `json:"summary"`
	Machine    string            `json:"machine"`
	Model      string            `json:"model,omitempty"`
	OccurredAt string            `json:"occurred_at"`
	Context    map[string]string `json:"context"`
	Payload    reviewPayload     `json:"payload"`
}

// numberText is a numeric field as a JSON number: the literal when it is
// one, else the parsed value; ok is false for anything else.
func numberText(s string) (json.RawMessage, bool) {
	if jsonNumber.MatchString(s) {
		return json.RawMessage(s), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || strings.ContainsAny(s, "xXpP_iInN") {
		return nil, false
	}

	return json.RawMessage(strconv.FormatFloat(f, 'f', -1, 64)), true
}

// optionalNumber is a scorecard number: blank, PENDING and anything that
// does not parse are absent.
func optionalNumber(s string) json.RawMessage {
	if s == "" || s == "PENDING" {
		return nil
	}
	n, _ := numberText(s)

	return n
}

// readMeta reads a run dir's meta.json; ok false when it is missing.
func readMeta(dir string) (reviewMeta, bool, *reviewSkip) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return reviewMeta{}, false, &reviewSkip{"no_meta", "no meta.json (predates the integration)"}
	}
	var m reviewMeta
	if err != nil || json.Unmarshal(data, &m) != nil {
		return reviewMeta{}, true, &reviewSkip{"malformed_meta", "unreadable meta.json"}
	}

	return m, true, nil
}

// timestampAmbiguous reports whether another run dir beside dir has the
// same run_ts: scorecards.tsv rows are keyed by run_ts alone.
func timestampAmbiguous(dir, runTS string) bool {
	base, self := filepath.Dir(dir), filepath.Base(dir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == self || !e.IsDir() {
			continue
		}
		peer, _, skip := readMeta(filepath.Join(base, e.Name()))
		if skip == nil && schema.Trim(peer.RunTS) == runTS {
			return true
		}
	}

	return false
}

// readScorecards returns the scorecards.tsv rows of run_ts.
func readScorecards(path, runTS string) ([]scoreRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []scoreRow
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 7)
		if len(f) < 4 || f[0] != runTS {
			continue
		}
		r := scoreRow{label: f[2], score: optionalNumber(f[3])}
		if len(f) > 4 {
			r.valid = optionalNumber(f[4])
		}
		if len(f) > 5 {
			r.invalid = optionalNumber(f[5])
		}
		if len(f) > 6 {
			r.note = f[6]
		}
		rows = append(rows, r)
	}

	return rows, nil
}

// readOptional reads one of the files --include-outputs sends: a missing
// file is absent (ok false); any other read error, or a running total over
// the body limit, is a skip. Sizes are checked before anything is read.
func readOptional(path string, total *int64) (string, bool, *reviewSkip) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, &reviewSkip{"unreadable_output", fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	if *total += st.Size(); *total > envelope.BodyLimit {
		return "", false, &reviewSkip{"body_too_large", fmt.Sprintf("the outputs and prompt are over the %d-byte body limit at %s; drop --include-outputs", envelope.BodyLimit, path)}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, &reviewSkip{"unreadable_output", fmt.Sprintf("cannot read %s: %v", path, err)}
	}

	return string(data), true, nil
}

// buildReview reads a run dir into the v1 review body and its key. Garbled
// timing columns are dropped with a warning; everything that would make the
// write-once key hold an untrustworthy run is a skip. drop lists context
// keys never sent.
func buildReview(dir string, includeOutputs bool, drop []string, warn func(string)) ([]byte, string, *reviewSkip) {
	// Absolute, so "." and relative paths name the same basename and ledger
	// as a sweep does.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	m, _, skip := readMeta(dir)
	if skip != nil {
		return nil, "", skip
	}
	if st, err := os.Stat(filepath.Join(dir, "summary.tsv")); err != nil || st.Size() == 0 {
		return nil, "", &reviewSkip{"no_summary", "no summary.tsv"}
	}
	machine, skill, runTS := schema.Trim(m.Machine), schema.Trim(m.Skill), schema.Trim(m.RunTS)
	caller := schema.Trim(m.Caller)
	if machine == "" || skill == "" || runTS == "" {
		return nil, "", &reviewSkip{"malformed_meta", "malformed meta.json (machine, skill or run_ts missing)"}
	}
	runID := machine + "-" + filepath.Base(dir)
	key := skill + "-" + runID
	at, err := time.ParseInLocation(runTSLayout, runTS, reviewLocation)
	if err != nil {
		return nil, key, &reviewSkip{"malformed_meta", fmt.Sprintf("malformed meta.json (run_ts %q is not YYYYMMDD-HHMMSS)", runTS)}
	}

	var rows []scoreRow
	ledger := filepath.Join(filepath.Dir(dir), "scorecards.tsv")
	if _, err := os.Stat(ledger); err == nil {
		if timestampAmbiguous(dir, runTS) {
			return nil, key, &reviewSkip{"ambiguous_timestamp", "ambiguous timestamp: multiple run directories share scorecard timestamp " + runTS}
		}
		if rows, err = readScorecards(ledger, runTS); err != nil {
			return nil, key, &reviewSkip{"unreadable_scorecards", fmt.Sprintf("cannot read %s: %v", ledger, err)}
		}
	}

	summary, err := os.ReadFile(filepath.Join(dir, "summary.tsv"))
	if err != nil {
		return nil, key, &reviewSkip{"no_summary", fmt.Sprintf("cannot read summary.tsv: %v", err)}
	}
	var reviewers []reviewer
	var incomplete []string
	var scoreSum float64
	var outputBytes int64
	scored, completed := 0, 0
	lines := strings.Split(string(summary), "\n")
	for _, line := range lines[1:] {
		f := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 5)
		for len(f) < 5 {
			f = append(f, "")
		}
		slot, model, status, dur, size := f[0], f[1], f[2], f[3], f[4]
		if slot == "" {
			continue
		}
		r := reviewer{Slot: slot, Model: model, Status: status}
		if dur != "" {
			n, ok := numberText(dur)
			if strings.Trim(dur, "0123456789.") != "" || !ok {
				warn(fmt.Sprintf("%s: non-numeric duration_s %q for slot %s; omitting it", runID, dur, slot))
			} else {
				r.DurationS = n
			}
		}
		if size != "" {
			v, err := strconv.ParseUint(size, 10, 64)
			if strings.Trim(size, "0123456789") != "" || err != nil {
				warn(fmt.Sprintf("%s: non-numeric bytes %q for slot %s; omitting it", runID, size, slot))
			} else {
				r.Bytes = json.RawMessage(strconv.FormatUint(v, 10))
			}
		}
		if includeOutputs && filepath.Base(slot) == slot {
			out, ok, skip := readOptional(filepath.Join(dir, slot+".md"), &outputBytes)
			if skip != nil {
				return nil, key, skip
			}
			if ok {
				r.Output = out
			}
		}
		label := m.label(slot)
		var graded []scoreRow
		for _, row := range rows {
			if row.label == label {
				graded = append(graded, row)
			}
		}
		if label != "" {
			for _, row := range rows {
				if row.label == label && row.score != nil {
					r.Score, r.Valid, r.Invalid, r.Note = row.score, row.valid, row.invalid, row.note
					if v, err := strconv.ParseFloat(string(row.score), 64); err == nil {
						scoreSum += v
						scored++
					}

					break
				}
			}
		}
		if status == "completed" {
			completed++
			if label == "" {
				incomplete = append(incomplete, slot+" (no scorecard label)")
			} else if len(graded) != 1 || graded[0].score == nil {
				incomplete = append(incomplete, slot+" ("+label+")")
			}
		}
		reviewers = append(reviewers, r)
	}
	if len(reviewers) == 0 {
		return nil, key, &reviewSkip{"no_reviewers", "no reviewer rows in summary.tsv"}
	}
	if len(incomplete) > 0 {
		return nil, key, &reviewSkip{"incomplete_scorecard", "incomplete scorecard for completed reviewer(s): " + strings.Join(incomplete, ", ")}
	}

	title := fmt.Sprintf("%s: %d reviewers, %d completed", skill, len(reviewers), completed)
	if scored > 0 {
		title += fmt.Sprintf(", mean %.1f/5", scoreSum/float64(scored))
	}
	body := reviewBody{
		Kind: "review", Key: key, Summary: title, Machine: machine,
		OccurredAt: at.UTC().Format(occurredLayout),
		Context:    map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "client": "agentfeedback/" + clientVersion().Version},
		Payload:    reviewPayload{Skill: skill, Reviewers: reviewers},
	}
	for _, k := range drop {
		delete(body.Context, k)
	}
	if caller != "" && caller != "unknown" {
		body.Model = caller
		body.Payload.Grader = &reviewGrader{Model: caller}
	}
	if includeOutputs {
		p, ok, skip := readOptional(filepath.Join(dir, "prompt.md"), &outputBytes)
		if skip != nil {
			return nil, key, skip
		}
		if ok {
			body.Payload.Prompt = p
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, key, &reviewSkip{"malformed_meta", err.Error()}
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), key, nil
}

const reviewSynopsis = "submit review <run_dir> [--include-outputs] [--dry-run] | submit review --sweep [<base>...]"

// runReview is submit review <run_dir> and submit review --sweep.
func runReview(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("submit review")
	includeOutputs := fs.Bool("include-outputs", false, "send the reviewer outputs and the prompt")
	sweep := fs.Bool("sweep", false, "submit every old unsubmitted run under the bases")
	dryRun := fs.Bool("dry-run", false, "print the body and check it locally; send nothing")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("submit", err)
	}
	if *sweep && *includeOutputs {
		return errExclusive("--sweep", "--include-outputs")
	}
	if !*sweep && len(pos) != 1 {
		return errArgs("submit review", reviewSynopsis)
	}
	s, err := newSubmitter("submit", *dryRun, stdout, stderr)
	if err != nil {
		return err
	}
	if o, off := s.disabled(); off {
		return finish("submit", o, stdout)
	}
	if *sweep {
		return s.sweep(pos)
	}

	o, err := s.submitRun(pos[0], *includeOutputs)
	if err != nil {
		return err
	}

	return finish("submit", o, stdout)
}

// submitRun builds and sends one run dir and writes its marker after the
// server stored or already had it.
func (s *submitter) submitRun(dir string, includeOutputs bool) (client.Outcome, error) {
	body, key, skip := buildReview(dir, includeOutputs, s.contextDrop(), s.warn)
	if skip != nil {
		o := client.Outcome{Outcome: client.OutcomeRejected, Kind: "review", Key: key, Reason: skip.reason,
			Message: skip.message + "; nothing was sent"}
		s.logLocal(o)

		return o, nil
	}

	return s.sendRun(dir, body)
}

// contextDrop is the config file's context.drop with the repository's.
func (s *submitter) contextDrop() []string {
	return append(slices.Clone(s.file.Context.Drop), s.decision.Drop...)
}

// sendRun sends a run's body and writes the marker after submitted,
// duplicate or mismatch: in each the server holds a submission under this
// key, so a sweep must not send the run again. A dry run writes none.
func (s *submitter) sendRun(dir string, body []byte) (client.Outcome, error) {
	o, err := s.send(body)
	if err != nil {
		return o, err
	}
	if s.dryRun {
		return o, nil
	}
	switch o.Outcome {
	case client.OutcomeSubmitted, client.OutcomeDuplicate, client.OutcomeMismatch:
		content := strconv.FormatInt(o.ID, 10) + "\n"
		if o.Outcome == client.OutcomeMismatch && o.ID <= 0 {
			content = "mismatch\n"
		}
		marker := filepath.Join(dir, submittedMarker)
		if err := writeMarker(dir, content); err != nil {
			s.warn(fmt.Sprintf("cannot write %s (%v); the next sweep submits this run again", marker, err))
		}
	}

	return o, nil
}

// writeMarker writes the marker through a temporary file and a rename, so a
// symlink named .submitted is replaced, never followed.
func writeMarker(dir, content string) error {
	f, err := os.CreateTemp(dir, submittedMarker+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, submittedMarker))
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}

	return werr
}

// reviewBases are the positional bases, else REVIEW_LOG_DIR and the
// colon-separated AGENT_FEEDBACK_REVIEW_DIRS.
func reviewBases(pos []string) []string {
	if len(pos) > 0 {
		return pos
	}
	var out []string
	if d := os.Getenv(envReviewLogDir); d != "" {
		out = append(out, d)
	}
	for d := range strings.SplitSeq(os.Getenv(envReviewDirs), ":") {
		if d != "" {
			out = append(out, d)
		}
	}

	return out
}

// sweep flushes the spool, then submits every run dir under the bases that
// has meta.json, no marker (submitted, duplicate or mismatch), and a run_ts
// at least two hours old. Skips are warnings, as is a flush that stopped or
// rejected; each submitted run prints its outcome line; it exits 0.
func (s *submitter) sweep(pos []string) error {
	if !s.dryRun {
		c, err := s.client()
		if err != nil {
			return err
		}
		rep := c.Flush(context.Background())
		if rep.Stopped != "" || rep.Rejected+rep.Mismatched > 0 {
			msg := fmt.Sprintf("spool flush: flushed %d, pending %d, rejected %d, mismatched %d", rep.Flushed, rep.Pending, rep.Rejected, rep.Mismatched)
			if rep.Stopped != "" {
				msg += ", stopped: " + rep.Stopped
			}
			s.warn(msg)
		}
	}
	bases := reviewBases(pos)
	if len(bases) == 0 {
		s.warn("no review run directories configured; set " + envReviewLogDir + " or " + envReviewDirs + ", or pass the bases")

		return nil
	}
	cutoff := nowFunc().Add(-sweepMinAge)
	for _, base := range bases {
		entries, err := os.ReadDir(base)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.warn(fmt.Sprintf("cannot read %s: %v", base, err))
			}

			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(base, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, submittedMarker)); err == nil {
				continue
			}
			if m, _, skip := readMeta(dir); skip == nil {
				if at, err := time.ParseInLocation(runTSLayout, schema.Trim(m.RunTS), reviewLocation); err == nil && at.After(cutoff) {
					continue
				}
			}
			body, _, skip := buildReview(dir, false, s.contextDrop(), s.warn)
			if skip != nil {
				s.warn(fmt.Sprintf("%s: %s; not submitting", dir, skip.message))

				continue
			}
			o, err := s.sendRun(dir, body)
			if err != nil {
				return err
			}
			if err := o.Write(s.stdout); err != nil {
				return err
			}
		}
	}

	return nil
}
