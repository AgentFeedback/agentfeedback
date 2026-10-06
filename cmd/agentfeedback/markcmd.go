package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

const (
	processedPath = "/api/v1/submissions/processed"
	// contextMax is the envelope's context entry limit.
	contextMax = 32
)

// batchMark is the body of the batch mark route; a nil field is omitted, and
// a given empty one is sent, which clears verdict, ref and processed_by.
type batchMark struct {
	IDs         []int64 `json:"ids"`
	Processed   bool    `json:"processed"`
	Verdict     *string `json:"verdict,omitempty"`
	Resolution  *string `json:"resolution,omitempty"`
	Ref         *string `json:"ref,omitempty"`
	ProcessedBy *string `json:"processed_by,omitempty"`
}

// batchMax is the most ids one batch mark takes.
const batchMax = 500

// uniqueIDs collapses duplicates, keeping the first order, and refuses more
// than one batch.
func uniqueIDs(ids []int64) ([]int64, error) {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > batchMax {
		return nil, errTooManyIDs(len(out))
	}

	return out, nil
}

// batchResult is the classification the batch mark route returns.
type batchResult struct {
	Updated   []int64 `json:"updated"`
	Unchanged []int64 `json:"unchanged"`
	NotFound  []int64 `json:"not_found"`
}

// postMark sends one batch mark and returns the raw body and its result.
func postMark(ctx context.Context, c *client.Client, m batchMark) ([]byte, batchResult, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, batchResult{}, err
	}
	res, err := c.Do(ctx, http.MethodPost, processedPath, nil, body)
	if err != nil {
		return nil, batchResult{}, err
	}
	var out batchResult
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return nil, batchResult{}, errBadResponse("the batch mark", err)
	}
	if err := checkBatch(m.IDs, out); err != nil {
		return nil, batchResult{}, err
	}

	return res.Body, out, nil
}

// checkBatch requires every requested id in exactly one of the three lists
// and nothing else in them.
func checkBatch(ids []int64, out batchResult) error {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	seen := map[int64]bool{}
	for _, list := range [][]int64{out.Updated, out.Unchanged, out.NotFound} {
		for _, id := range list {
			if !want[id] || seen[id] {
				return errBadResponse("the batch mark", fmt.Errorf("id %d is unexpected or classified twice", id))
			}
			seen[id] = true
		}
	}
	if len(seen) != len(want) {
		return errBadResponse("the batch mark", fmt.Errorf("%d of %d ids are classified", len(seen), len(want)))
	}

	return nil
}

// runMark sends the batch mark for done and undo: the compact API body is
// the last stdout line, the counts go to stderr, and an id not found exits 1.
func runMark(name string, m batchMark, stdout, stderr io.Writer) error {
	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	body, out, err := postMark(context.Background(), c, m)
	if err != nil {
		return apiErr(err, stderr)
	}
	fmt.Fprintf(stderr, "agentfeedback %s: marked %d, unchanged %d, not found %d\n", name, len(out.Updated), len(out.Unchanged), len(out.NotFound))
	if err := writeCompact(stdout, body); err != nil {
		return err
	}
	if len(out.NotFound) > 0 {
		return &reportedError{fmt.Errorf("%d id(s) not found", len(out.NotFound))}
	}

	return nil
}

const doneSynopsis = "done <id>... --verdict V [--resolution R] [--ref REF] [--processed-by P]"

// runDone marks submissions processed.
func runDone(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("done")
	verdict := fs.String("verdict", "", "fixed, invalid, duplicate, wont_fix, deferred, upstream or unverifiable (required)")
	resolution := fs.String("resolution", "", "what was done")
	ref := fs.String("ref", "", "a commit, URL, or the uid of another submission")
	processedBy := fs.String("processed-by", "", "who processed it")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("done", err)
	}
	if len(pos) == 0 {
		return errArgs("done", doneSynopsis)
	}
	ids, err := parseIDs(pos)
	if err != nil {
		return err
	}
	if ids, err = uniqueIDs(ids); err != nil {
		return err
	}
	if *verdict == "" {
		return errVerdictRequired()
	}
	set := visited(fs)
	given := func(name string, v *string) *string {
		if set[name] {
			return v
		}

		return nil
	}

	return runMark("done", batchMark{
		IDs: ids, Processed: true, Verdict: verdict, Resolution: given("resolution", resolution),
		Ref: given("ref", ref), ProcessedBy: given("processed-by", processedBy),
	}, stdout, stderr)
}

// runUndo clears the processing mark of submissions.
func runUndo(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("undo")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("undo", err)
	}
	if len(pos) == 0 {
		return errArgs("undo", "undo <id>...")
	}
	ids, err := parseIDs(pos)
	if err != nil {
		return err
	}
	if ids, err = uniqueIDs(ids); err != nil {
		return err
	}

	return runMark("undo", batchMark{IDs: ids, Processed: false}, stdout, stderr)
}

// runRedact replaces a submission with its tombstone.
func runRedact(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("redact")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("redact", err)
	}
	if len(pos) != 1 {
		return errArgs("redact", "redact <id>")
	}
	ids, err := parseIDs(pos)
	if err != nil {
		return err
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	res, err := c.Do(context.Background(), http.MethodDelete, submissionPath(ids[0]), nil, nil)
	if err != nil {
		return idErr(err, ids[0], stderr)
	}
	fmt.Fprintf(stderr, "agentfeedback redact: #%d is a tombstone\n", ids[0])

	return writeCompact(stdout, res.Body)
}

// rekindBody is the new submission rekind sends: the original's envelope
// under the new kind and a key fixed by the original's uid, so a rerun is a
// duplicate. schema_version is left for the server to infer.
type rekindBody struct {
	Kind       string                     `json:"kind"`
	Key        string                     `json:"key"`
	Summary    string                     `json:"summary,omitempty"`
	Machine    string                     `json:"machine,omitempty"`
	Model      string                     `json:"model,omitempty"`
	Harness    string                     `json:"harness,omitempty"`
	Project    string                     `json:"project,omitempty"`
	OccurredAt string                     `json:"occurred_at,omitempty"`
	Context    map[string]json.RawMessage `json:"context"`
	Payload    json.RawMessage            `json:"payload,omitempty"`
}

// runRekind files a submission again under another kind and marks the
// original a duplicate of the new row. The last stdout line is the outcome.
func runRekind(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("rekind")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("rekind", err)
	}
	if len(pos) != 2 {
		return errArgs("rekind", "rekind <id> <kind>")
	}
	ids, err := parseIDs(pos[:1])
	if err != nil {
		return err
	}
	id := ids[0]
	kind := schema.Token(pos[1])
	if kind == "" {
		return errRekindKind(pos[1])
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	res, err := c.Do(ctx, http.MethodGet, submissionPath(id), nil, nil)
	if err != nil {
		return idErr(err, id, stderr)
	}
	var orig row
	if err := json.Unmarshal(res.Body, &orig); err != nil {
		return errBadResponse("get", err)
	}
	if orig.RedactedAt != "" {
		return errRekindRedacted(id)
	}
	if schema.Token(orig.Kind) == kind {
		return errRekindSameKind(id, kind)
	}
	// Marked duplicate is a rerun of rekind itself; any other mark is a
	// decision rekind must not overwrite.
	if orig.ProcessedAt != "" && orig.Verdict != "duplicate" {
		verdict := orig.Verdict
		if verdict == "" {
			verdict = "processed"
		}

		return errRekindProcessed(id, clean(verdict))
	}

	nb := rekindBody{
		Kind: kind, Key: "rekind-" + orig.UID, Summary: orig.Summary, Machine: orig.Machine, Model: orig.Model,
		Harness: orig.Harness, Project: orig.Project, OccurredAt: orig.OccurredAt,
		Context: map[string]json.RawMessage{}, Payload: orig.Payload,
	}
	if len(orig.Context) > 0 {
		if err := json.Unmarshal(orig.Context, &nb.Context); err != nil {
			return errBadResponse("get", err)
		}
	}
	if _, ok := nb.Context["rekinded_from"]; !ok && len(nb.Context) >= contextMax {
		return errRekindContextFull(id, len(nb.Context))
	}
	nb.Context["rekinded_from"], _ = json.Marshal(orig.UID)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(nb); err != nil {
		return err
	}

	o := c.Submit(ctx, bytes.TrimRight(buf.Bytes(), "\n"))
	switch o.Outcome {
	case client.OutcomeSubmitted, client.OutcomeDuplicate:
		if err := markRekinded(ctx, c, id, kind, o.ID); err != nil {
			o = client.Outcome{
				Outcome: client.OutcomeError, Kind: o.Kind, Key: o.Key, ID: o.ID, RequestID: o.RequestID, Reason: "mark_failed",
				Message: fmt.Sprintf("#%d was filed as #%d but marking #%d a duplicate failed: %v; rerun agentfeedback rekind %d %s",
					id, o.ID, id, describeErr(err), id, kind),
			}
			c.Log(o)
		} else {
			fmt.Fprintf(stderr, "agentfeedback rekind: #%d re-kinded as %s #%d and marked duplicate\n", id, kind, o.ID)
		}
	case client.OutcomeSpooled:
		fmt.Fprintf(stderr, "agentfeedback rekind: the new row is spooled and #%d is not marked yet; rerun agentfeedback rekind %d %s after the spool is flushed\n", id, id, kind)
	}
	if err := o.Write(stdout); err != nil {
		return err
	}
	if o.ExitCode() != 0 {
		return &reportedError{errors.New("rekind " + o.Outcome)}
	}

	return nil
}

// markRekinded marks the original a duplicate whose ref is the new row's uid.
func markRekinded(ctx context.Context, c *client.Client, id int64, kind string, newID int64) error {
	res, err := c.Do(ctx, http.MethodGet, submissionPath(newID), nil, nil)
	if err != nil {
		return err
	}
	var nr row
	if err := json.Unmarshal(res.Body, &nr); err != nil || nr.UID == "" {
		return errBadResponse("get", fmt.Errorf("no uid for #%d", newID))
	}
	verdict, resolution := "duplicate", fmt.Sprintf("re-kinded as %s, #%d", kind, newID)
	_, out, err := postMark(ctx, c, batchMark{
		IDs: []int64{id}, Processed: true, Verdict: &verdict, Ref: &nr.UID, Resolution: &resolution,
	})
	if err != nil {
		return err
	}
	if len(out.NotFound) > 0 {
		return errNotFound(id)
	}

	return nil
}
