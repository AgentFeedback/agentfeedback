package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"

	"github.com/agentfeedback/agentfeedback/pkg/client"
	"github.com/agentfeedback/agentfeedback/pkg/collect"
)

const installCheckSummary = "agentfeedback doctor --e2e install check"

// e2eStep is one line of doctor --e2e: a step that passed, or the step that
// failed, after which nothing more is printed.
type e2eStep struct {
	Step      string `json:"step"`
	Outcome   string `json:"outcome"`
	ID        int64  `json:"id,omitempty"`
	Key       string `json:"key,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// runDoctorE2E submits one install-check row, finds it by its key and marks
// it processed. The submission goes straight to the server, not through the
// spool, so a failed check leaves nothing behind on this machine.
func runDoctorE2E(getenv func(string) string, urlFlag string, asJSON bool, stdout, stderr io.Writer) error {
	emit := func(s e2eStep) error {
		if asJSON {
			return writeJSON(stdout, s)
		}
		label := s.Step + ":"
		switch {
		case s.Outcome != "ok":
			fmt.Fprintf(stdout, "%-9s error: %s\n", label, s.Message)
		case s.Key != "":
			fmt.Fprintf(stdout, "%-9s ok (id %d, key %s)\n", label, s.ID, s.Key)
		case s.Verdict != "":
			fmt.Fprintf(stdout, "%-9s ok (id %d, verdict %s)\n", label, s.ID, s.Verdict)
		default:
			fmt.Fprintf(stdout, "%-9s ok (id %d)\n", label, s.ID)
		}

		return nil
	}
	fail := func(step string, err error) error {
		s := e2eStep{Step: step, Outcome: "error", Message: describeErr(err)}
		var ae *client.APIError
		if errors.As(err, &ae) {
			s.RequestID = ae.RequestID
		}
		if werr := emit(s); werr != nil {
			return werr
		}

		return &reportedError{err}
	}

	ctx := context.Background()
	c, settings, err := newAPIClient(getenv, urlFlag, errURLUnset, stderr)
	if err != nil {
		return fail("submit", err)
	}
	body, err := installCheckBody(getenv, settings)
	if err != nil {
		return fail("submit", err)
	}
	prepared, _, key, err := client.PrepareBody(body)
	if err != nil {
		return fail("submit", err)
	}

	res, err := c.Do(ctx, http.MethodPost, "/api/v1/submissions", nil, prepared)
	if err != nil {
		return fail("submit", err)
	}
	var created struct {
		Submission struct {
			ID int64 `json:"id"`
		} `json:"submission"`
	}
	if err := json.Unmarshal(res.Body, &created); err != nil {
		return fail("submit", errBadResponse("the submission", err))
	}
	id := created.Submission.ID
	if id < 1 {
		return fail("submit", errE2ECheck("submit", "the answer carries no submission id"))
	}
	if err := emit(e2eStep{Step: "submit", Outcome: "ok", ID: id, Key: key}); err != nil {
		return err
	}

	res, err = c.Do(ctx, http.MethodGet, "/api/v1/submissions", url.Values{"kind": {installCheckKind}, "key": {key}}, nil)
	if err != nil {
		return fail("list", err)
	}
	var listed struct {
		Submissions []struct {
			ID int64 `json:"id"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(res.Body, &listed); err != nil {
		return fail("list", errBadResponse("the list", err))
	}
	if len(listed.Submissions) != 1 || listed.Submissions[0].ID != id {
		return fail("list", errE2ECheck("list", fmt.Sprintf("listing key %s returned %d row(s), not submission %d alone", key, len(listed.Submissions), id)))
	}
	if err := emit(e2eStep{Step: "list", Outcome: "ok", ID: id}); err != nil {
		return err
	}

	verdict := installCheckKind
	_, out, err := postMark(ctx, c, batchMark{IDs: []int64{id}, Processed: true, Verdict: &verdict})
	if err != nil {
		return fail("mark", err)
	}
	if !slices.Contains(out.Updated, id) {
		return fail("mark", errE2ECheck("mark", fmt.Sprintf("submission %d was not updated", id)))
	}

	return emit(e2eStep{Step: "mark", Outcome: "ok", ID: id, Verdict: verdict})
}

// installCheckBody is the install-check envelope: the machine, harness and
// model as submit would fill them, and only the machine group of context.
func installCheckBody(getenv func(string) string, settings clientSettings) ([]byte, error) {
	collected := collect.Collect(collect.Options{Dir: ".", Getenv: getenv, ClientVersion: clientVersion().Version})
	ctxKeys := map[string]string{}
	for _, k := range []string{"os", "arch", "client"} {
		if v := collected.Context[k]; v != "" {
			ctxKeys[k] = v
		}
	}
	body := map[string]any{
		"kind":        installCheckKind,
		"summary":     installCheckSummary,
		"occurred_at": nowFunc().UTC().Format(occurredLayout),
		"payload":     map[string]any{},
	}
	if len(ctxKeys) > 0 {
		body["context"] = ctxKeys
	}
	for member, values := range map[string][]string{
		"machine": {settings.Machine.Value, collected.Machine},
		"harness": {settings.Harness.Value, collected.Harness},
		"model":   {settings.Model.Value, collected.Model},
	} {
		for _, v := range values {
			if v != "" {
				body[member] = v

				break
			}
		}
	}

	return json.Marshal(body)
}
