package client

import (
	"bytes"
	"encoding/json"
	"io"
)

// Outcome words. A command prints exactly one of them per submission.
const (
	OutcomeSubmitted = "submitted"
	OutcomeDuplicate = "duplicate"
	OutcomeSpooled   = "spooled"
	OutcomeMismatch  = "mismatch"
	OutcomeRejected  = "rejected"
	OutcomeValid     = "valid"
	OutcomeError     = "error"
	// OutcomeFlushed is logged for a spooled entry a flush delivered.
	OutcomeFlushed = "flushed"
	// OutcomeDisabled is logged when submitting is switched off.
	OutcomeDisabled = "disabled"
)

// Outcome is the result of one submission. The caller prints it LAST on
// stdout with Write, after anything else it prints, so the final stdout line
// is always the machine-readable result.
type Outcome struct {
	Outcome   string `json:"outcome"`
	Kind      string `json:"kind,omitempty"`
	Key       string `json:"key,omitempty"`
	ID        int64  `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ExitCode is the process exit status for the outcome: 0 when the
// submission is delivered, kept for later, or only validated; 1 otherwise.
func (o Outcome) ExitCode() int {
	switch o.Outcome {
	case OutcomeSubmitted, OutcomeDuplicate, OutcomeSpooled, OutcomeValid, OutcomeDisabled, OutcomeFlushed:
		return 0
	}

	return 1
}

// Write prints the outcome as exactly one line of compact JSON.
func (o Outcome) Write(w io.Writer) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(o); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())

	return err
}

// Valid is the outcome of a submission checked but not sent.
func Valid(kind, key string) Outcome {
	return Outcome{Outcome: OutcomeValid, Kind: kind, Key: key}
}
