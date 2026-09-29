package api

import (
	"encoding/json"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

// ReviewerDTO is a single reviewer's contribution within a review submission.
type ReviewerDTO struct {
	Slot      string `json:"slot"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	DurationS *int   `json:"duration_s,omitempty"`
	Bytes     *int   `json:"bytes,omitempty"`
	Output    string `json:"output,omitempty"`
	Score     *int   `json:"score,omitempty"`
	Valid     *int   `json:"valid,omitempty"`
	Invalid   *int   `json:"invalid,omitempty"`
	Note      string `json:"note,omitempty"`
}

// CreateReviewRequest is the request body for POST /api/v1/reviews.
type CreateReviewRequest struct {
	Skill            string        `json:"skill"`
	MachineName      string        `json:"machine_name"`
	CoordinatorModel string        `json:"coordinator_model"`
	RunID            string        `json:"run_id"`
	Prompt           string        `json:"prompt,omitempty"`
	Reviewers        []ReviewerDTO `json:"reviewers"`
}

// CreateFrictionRequest is the request body for POST /api/v1/frictions.
// Context is auto-collected client metadata (flat string map); it is stored
// verbatim and excluded from the duplicate-absorption hash.
type CreateFrictionRequest struct {
	MachineName      string            `json:"machine_name"`
	CoordinatorModel string            `json:"coordinator_model"`
	Category         string            `json:"category"`
	Summary          string            `json:"summary"`
	Details          string            `json:"details,omitempty"`
	SuggestedFix     string            `json:"suggested_fix,omitempty"`
	Project          string            `json:"project,omitempty"`
	Harness          string            `json:"harness,omitempty"`
	Context          map[string]string `json:"context,omitempty"`
}

// CreateEventRequest is the request body for POST /api/v1/events. Payload stays
// raw: it is stored and returned exactly as submitted.
type CreateEventRequest struct {
	Kind             string          `json:"kind"`
	Key              string          `json:"key"`
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	Payload          json.RawMessage `json:"payload"`
}

// ListSubmissionsResponse is the response body for GET /api/v1/submissions.
type ListSubmissionsResponse struct {
	Submissions  []core.SummaryRecord `json:"submissions"`
	Limit        int                  `json:"limit"`
	Offset       int                  `json:"offset"`
	Total        int64                `json:"total"`
	HasMore      bool                 `json:"has_more"`
	NextBeforeID *int64               `json:"next_before_id"`
}

// SetProcessedRequest is the request body for POST /api/v1/submissions/processed.
// Processed defaults to true when omitted; false unmarks. A null or absent
// Resolution means "not given"; a blank string is rejected.
type SetProcessedRequest struct {
	IDs        []int64 `json:"ids"`
	Processed  *bool   `json:"processed,omitempty"`
	Resolution *string `json:"resolution,omitempty"`
}

// SetProcessedResponse classifies every requested id.
type SetProcessedResponse struct {
	Processed  bool    `json:"processed"`
	Resolution *string `json:"resolution,omitempty"`
	Updated    []int64 `json:"updated"`
	Unchanged  []int64 `json:"unchanged"`
	NotFound   []int64 `json:"not_found"`
}
