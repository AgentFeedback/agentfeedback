package client

import (
	"context"
	"io"
	"path/filepath"
)

// InboxDir is the file inbox under the data directory: one envelope per
// *.json file, written by an agent that cannot run the binary and ingested
// by the CLI.
func InboxDir(data string) string { return filepath.Join(data, "inbox") }

// Delivery is the result of one Deliver.
type Delivery struct {
	// Outcome is submitted, duplicate, mismatch or rejected, or error when
	// the body was not delivered and may be later (Retry or Hold).
	Outcome Outcome
	// Retry: the destination did not take it now (busy database, network,
	// rate limit, 5xx); the same body may succeed later.
	Retry bool
	// Hold: a setting is wrong (the key or the URL); nothing else will be
	// delivered until it is fixed.
	Hold bool
	// Problem is the destination's error body of a refusal, as received, cut
	// at 64 KiB as in a rejected/ entry.
	Problem []byte
}

// Deliver sends body once, as Submit does, and keeps nothing: no spool
// entry, no rejected/ file, no client log line and no note on stderr. The
// caller owns the body and decides where it goes after the answer.
func (c *Client) Deliver(ctx context.Context, body []byte) Delivery {
	prepared, kind, key, err := PrepareBody(body)
	if err != nil {
		return Delivery{Outcome: Outcome{Outcome: OutcomeRejected, Reason: "invalid_body", Message: err.Error()}}
	}
	quiet := *c
	quiet.stderr = io.Discard
	res := quiet.send(ctx, prepared, kind, key)
	d := Delivery{Outcome: Outcome{Kind: kind, Key: key, RequestID: res.requestID, Reason: res.reason, Message: res.message}}
	switch res.action {
	case actAccept:
		d.Outcome.Outcome, d.Outcome.ID, d.Outcome.Reason = OutcomeSubmitted, res.id, ""
		if res.duplicate {
			d.Outcome.Outcome = OutcomeDuplicate
		}
	case actRetry:
		d.Outcome.Outcome, d.Retry = OutcomeError, true
	case actHold:
		d.Outcome.Outcome, d.Hold = OutcomeError, true
	case actMismatch:
		d.Outcome.Outcome, d.Problem = OutcomeMismatch, res.response
	case actReject:
		d.Outcome.Outcome, d.Problem = OutcomeRejected, res.response
	}
	if len(d.Problem) > responseKeep {
		d.Problem = d.Problem[:responseKeep]
	}

	return d
}
