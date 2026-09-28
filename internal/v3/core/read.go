package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

const (
	defaultListLimit           = 50
	maxListLimit               = 500
	maxListLimitWithPayload    = 100
	exportFormatVersion        = 1
	exportFlushEveryNthRecords = 200
)

// ListSubmissionsInput holds the list filters. Zero values mean "no filter".
type ListSubmissionsInput struct {
	Family         string
	Type           string
	Machine        string
	Model          string
	Since          *time.Time
	Until          *time.Time
	Processed      *bool
	BeforeID       *int64
	Limit          int
	Offset         int
	IncludePayload bool
}

// ListSubmissionsResult is one page plus the counters a pager needs.
type ListSubmissionsResult struct {
	Rows         []store.Summary
	Limit        int
	Offset       int
	Total        int64
	HasMore      bool
	NextBeforeID *int64
}

// NormalizeListWindow clamps limit and offset to the bounds the query applies,
// so callers can echo the values actually used. includePayload lowers the
// ceiling: full records are far larger than summaries.
func NormalizeListWindow(limit, offset int, includePayload bool) (int, int) {
	ceiling := maxListLimit
	if includePayload {
		ceiling = maxListLimitWithPayload
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > ceiling {
		limit = ceiling
	}
	if offset < 0 {
		offset = 0
	}
	if offset > math.MaxInt32 {
		offset = math.MaxInt32
	}

	return limit, offset
}

// ListSubmissions returns one page, newest first, with the total for the same
// filters computed in the same read transaction.
func (s *Service) ListSubmissions(ctx context.Context, in ListSubmissionsInput) (ListSubmissionsResult, error) {
	if in.Family != "" && in.Family != store.FamilyFriction && in.Family != store.FamilyReview && in.Family != store.FamilyEvent {
		return ListSubmissionsResult{}, fmt.Errorf("%w: family must be friction, review or event", ErrInvalidInput)
	}

	limit, offset := NormalizeListWindow(in.Limit, in.Offset, in.IncludePayload)
	filter := store.ListFilter{
		Family:    in.Family,
		Type:      in.Type,
		Machine:   in.Machine,
		Model:     in.Model,
		Since:     microsPtr(in.Since),
		Until:     microsPtr(in.Until),
		Processed: in.Processed,
		BeforeID:  in.BeforeID,
	}

	res := ListSubmissionsResult{Limit: limit, Offset: offset, Rows: []store.Summary{}}
	err := s.db.Read(ctx, func(q store.Querier) error {
		total, err := store.CountSubmissions(ctx, q, filter)
		if err != nil {
			return err
		}
		res.Total = total

		// One row beyond the page answers has_more without a second count.
		rows, err := store.ListSubmissions(ctx, q, filter, limit+1, offset, in.IncludePayload)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			res.HasMore = true
			rows = rows[:limit]
		}
		res.Rows = rows

		return nil
	})
	if err != nil {
		return ListSubmissionsResult{}, err
	}

	if res.HasMore && len(res.Rows) > 0 {
		last := res.Rows[len(res.Rows)-1].ID
		res.NextBeforeID = &last
	}

	return res, nil
}

// GetSubmission returns one full record.
func (s *Service) GetSubmission(ctx context.Context, id int64) (store.Submission, error) {
	var sub store.Submission
	err := s.db.Read(ctx, func(q store.Querier) error {
		got, err := store.GetSubmissionByID(ctx, q, id)
		if err != nil {
			return err
		}
		sub = got

		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.Submission{}, ErrSubmissionNotFound
	}
	if err != nil {
		return store.Submission{}, err
	}

	return sub, nil
}

// ExportInput filters an export. Both filters are optional; an export that
// uses either is partial and is marked as such in the header.
type ExportInput struct {
	Family string
	Since  *time.Time
}

func microsPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UTC().UnixMicro()

	return &v
}

// Export streams every matching record as newline-delimited JSON in one read
// transaction: a header line, the records in ascending id order, then a
// terminator carrying the record count and a digest over the record lines.
// A stream without the terminator is truncated and must not be trusted.
func (s *Service) Export(ctx context.Context, in ExportInput, w io.Writer, flush func()) error {
	if in.Family != "" && in.Family != store.FamilyFriction && in.Family != store.FamilyReview && in.Family != store.FamilyEvent {
		return fmt.Errorf("%w: family must be friction, review or event", ErrInvalidInput)
	}

	return s.db.Read(ctx, func(q store.Querier) error {
		enc := newExportEncoder(w)
		if err := enc.writeHeader(in.Family, in.Since, nowMicros()); err != nil {
			return err
		}

		n := 0
		err := store.IterateSubmissions(ctx, q, store.ListFilter{
			Family: in.Family,
			Since:  microsPtr(in.Since),
		}, func(sub store.Submission) error {
			if err := enc.writeRecord(sub); err != nil {
				return err
			}
			n++
			if flush != nil && n%exportFlushEveryNthRecords == 0 {
				flush()
			}

			return nil
		})
		if err != nil {
			return err
		}

		if err := enc.writeTerminator(); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}

		return nil
	})
}

// CountUnprocessed reports how many submissions still await processing.
func (s *Service) CountUnprocessed(ctx context.Context) (int64, error) {
	processed := false
	var n int64
	err := s.db.Read(ctx, func(q store.Querier) error {
		got, err := store.CountSubmissions(ctx, q, store.ListFilter{Processed: &processed})
		if err != nil {
			return err
		}
		n = got

		return nil
	})

	return n, err
}
