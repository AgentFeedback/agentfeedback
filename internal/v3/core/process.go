package core

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

const maxProcessedIDs = 500

// SetProcessedInput identifies submissions to mark (Processed=true) or unmark
// (Processed=false). The processing mark is the only mutable submission state;
// the payload stays write-once.
//
// Resolution is nil when the caller did not send one. An empty or
// whitespace-only string is a client error, not "no resolution": it would
// silently erase a stored one.
type SetProcessedInput struct {
	IDs        []int64
	Processed  bool
	Resolution *string
}

// SetProcessedResult classifies every requested id: Updated (state changed),
// Unchanged (already in the requested state with the same resolution),
// NotFound (no such submission). Resolution echoes the trimmed value stored.
type SetProcessedResult struct {
	Updated    []int64
	Unchanged  []int64
	NotFound   []int64
	Resolution *string
}

// SetProcessed marks or unmarks submissions in one transaction: every id is
// classified against the state it had when the batch was read.
func (s *Service) SetProcessed(ctx context.Context, in SetProcessedInput) (SetProcessedResult, error) {
	if len(in.IDs) == 0 {
		return SetProcessedResult{}, fmt.Errorf("%w: ids must be non-empty", ErrInvalidInput)
	}
	if len(in.IDs) > maxProcessedIDs {
		return SetProcessedResult{}, fmt.Errorf("%w: at most %d ids per request", ErrInvalidInput, maxProcessedIDs)
	}

	var resolution *string
	if in.Resolution != nil {
		if !in.Processed {
			return SetProcessedResult{}, fmt.Errorf("%w: resolution is only allowed with processed=true", ErrInvalidInput)
		}
		trimmed := strings.TrimSpace(*in.Resolution)
		if trimmed == "" {
			return SetProcessedResult{}, fmt.Errorf("%w: resolution must not be blank", ErrInvalidInput)
		}
		if err := fieldTooLong("resolution", trimmed, maxSummaryLen); err != nil {
			return SetProcessedResult{}, err
		}
		resolution = &trimmed
	}

	seen := make(map[int64]struct{}, len(in.IDs))
	ids := make([]int64, 0, len(in.IDs))
	for _, id := range in.IDs {
		if id <= 0 {
			return SetProcessedResult{}, fmt.Errorf("%w: ids must be positive, got %d", ErrInvalidInput, id)
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	// Empty slices, not nil: the contract promises [] over null.
	res := SetProcessedResult{
		Updated:    []int64{},
		Unchanged:  []int64{},
		NotFound:   []int64{},
		Resolution: resolution,
	}

	err := s.db.Write(ctx, func(q store.Querier) error {
		states, err := store.GetProcessingStates(ctx, q, ids)
		if err != nil {
			return err
		}

		for _, id := range ids {
			st, found := states[id]
			switch {
			case !found:
				res.NotFound = append(res.NotFound, id)
			case in.Processed && st.ProcessedAt == nil:
				res.Updated = append(res.Updated, id)
			case in.Processed && resolution != nil && (st.Resolution == nil || *st.Resolution != *resolution):
				// Already processed, but the resolution changes: the row
				// changes, the original processed_at is kept.
				res.Updated = append(res.Updated, id)
			case in.Processed:
				res.Unchanged = append(res.Unchanged, id)
			case st.ProcessedAt != nil:
				res.Updated = append(res.Updated, id)
			default:
				res.Unchanged = append(res.Unchanged, id)
			}
		}

		if len(res.Updated) == 0 {
			return nil
		}
		if in.Processed {
			return store.MarkProcessed(ctx, q, res.Updated, nowMicros(), resolution)
		}

		return store.UnmarkProcessed(ctx, q, res.Updated)
	})
	if err != nil {
		return SetProcessedResult{}, err
	}

	slices.Sort(res.Updated)
	slices.Sort(res.Unchanged)
	slices.Sort(res.NotFound)

	if len(res.Updated) > 0 {
		slog.InfoContext(ctx, "submissions processed state changed",
			"processed", in.Processed, "updated", res.Updated)
	}

	return res, nil
}
