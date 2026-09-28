package core

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// TimeFormat is the wire format for every timestamp: RFC 3339, UTC,
// microsecond precision.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// FormatMicros renders a stored unix-microsecond timestamp for the wire.
func FormatMicros(us int64) string {
	return time.UnixMicro(us).UTC().Format(TimeFormat)
}

// ParseMicros parses a wire timestamp back into unix microseconds. Any RFC 3339
// offset is accepted.
func ParseMicros(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("parse timestamp %q: %w", s, err)
	}

	return t.UTC().UnixMicro(), nil
}

// Record is the wire representation of a submission: the response body of the
// write and get endpoints, and one line of an export. Null-valued fields are
// omitted, as in API 1.0.
type Record struct {
	ID               int64           `json:"id"`
	Family           string          `json:"family"`
	SubmissionType   string          `json:"submission_type"`
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	RunID            *string         `json:"run_id,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	PayloadHash      string          `json:"payload_hash"`
	CreatedAt        string          `json:"created_at"`
	ProcessedAt      *string         `json:"processed_at,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
}

// ToRecord converts a stored submission to its wire form.
func ToRecord(s store.Submission) Record {
	return Record{
		ID:               s.ID,
		Family:           s.Family,
		SubmissionType:   s.SubmissionType,
		MachineName:      s.MachineName,
		CoordinatorModel: s.CoordinatorModel,
		RunID:            s.RunID,
		Payload:          s.Payload,
		PayloadHash:      s.PayloadHash,
		CreatedAt:        FormatMicros(s.CreatedAt),
		ProcessedAt:      formatMicrosPtr(s.ProcessedAt),
		Resolution:       s.Resolution,
	}
}

// SummaryRecord is a list row: the record without the payload, plus the
// friction fields lifted out of it so a list is scannable. With
// include=payload the payload is present too.
type SummaryRecord struct {
	ID               int64           `json:"id"`
	Family           string          `json:"family"`
	SubmissionType   string          `json:"submission_type"`
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	RunID            *string         `json:"run_id,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	PayloadHash      string          `json:"payload_hash"`
	CreatedAt        string          `json:"created_at"`
	ProcessedAt      *string         `json:"processed_at,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	Category         string          `json:"category,omitempty"`
	Summary          string          `json:"summary,omitempty"`
	Project          string          `json:"project,omitempty"`
	Harness          string          `json:"harness,omitempty"`
}

// ToSummaryRecord converts a stored list row to its wire form.
func ToSummaryRecord(s store.Summary) SummaryRecord {
	return SummaryRecord{
		ID:               s.ID,
		Family:           s.Family,
		SubmissionType:   s.SubmissionType,
		MachineName:      s.MachineName,
		CoordinatorModel: s.CoordinatorModel,
		RunID:            s.RunID,
		Payload:          s.Payload,
		PayloadHash:      s.PayloadHash,
		CreatedAt:        FormatMicros(s.CreatedAt),
		ProcessedAt:      formatMicrosPtr(s.ProcessedAt),
		Resolution:       s.Resolution,
		Category:         s.Category,
		Summary:          s.Summary,
		Project:          s.Project,
		Harness:          s.Harness,
	}
}

func formatMicrosPtr(us *int64) *string {
	if us == nil {
		return nil
	}
	v := FormatMicros(*us)

	return &v
}
