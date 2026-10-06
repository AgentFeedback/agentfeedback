package core

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// Get returns one record. id must be positive (else 400); an unknown id is 404.
func (s *Service) Get(ctx context.Context, id int64) (Record, error) {
	if err := checkID(id); err != nil {
		return Record{}, err
	}
	var rec Record
	err := s.read(ctx, func(q store.Querier) error {
		sub, err := store.GetByID(ctx, q, id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(id)
		}
		rec = Record{sub}
		return err
	})
	return rec, err
}

// Filter is the filter set the list and the stats share, as the transport
// parsed it. An empty string, a nil pointer and a nil slice mean "no filter".
// kind, harness, category, fix_status, verdict and exclude_kind are
// normalised as tokens; key, machine, model and project match exactly.
type Filter struct {
	Kind          string
	SchemaVersion *int64
	Key           string
	Machine       string
	Model         string
	Project       string
	Harness       string
	Category      string
	FixStatus     string
	ExcludeKind   []string
	Verdict       string
	Processed     *bool
	Redacted      *bool
	ContentHash   string
	Since         *time.Time
	Until         *time.Time
	On            string // created_at (default when "") or occurred_at
	Q             string
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// short quotes v for a message when it is short enough to be useful.
func short(v string) string {
	const limit = 64
	if len(v) > limit {
		cut, _ := schema.TruncateBytes(v, limit)
		return quote(cut + "…")
	}
	return quote(v)
}

func quote(v string) string { return string(appendQuoted(nil, v)) }

func appendQuoted(dst []byte, v string) []byte {
	b, _ := Marshal(v)
	return append(dst, b...)
}

// token normalises a token parameter; a value empty after normalisation is 400.
func token(name, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	t := schema.Token(v)
	if t == "" {
		return "", invalid("invalid_value", "?"+name, "%s must be a non-blank token, got %s", name, short(v))
	}
	return t, nil
}

// storeFilter validates f and maps it onto the store's filter; List and
// Stats share it.
func (f Filter) storeFilter() (store.ListFilter, error) {
	var out store.ListFilter
	var err error
	for _, t := range []struct {
		name string
		in   string
		out  *string
	}{
		{"kind", f.Kind, &out.Kind}, {"harness", f.Harness, &out.Harness}, {"category", f.Category, &out.Category},
		{"fix_status", f.FixStatus, &out.FixStatus}, {"verdict", f.Verdict, &out.Verdict},
	} {
		if *t.out, err = token(t.name, t.in); err != nil {
			return out, err
		}
	}
	for _, k := range f.ExcludeKind {
		t, err := token("exclude_kind", k)
		if err != nil {
			return out, err
		}
		if t == "" {
			return out, invalid("invalid_value", "?exclude_kind", "exclude_kind must be a non-blank token, got \"\"")
		}
		if !slices.Contains(out.ExcludeKinds, t) {
			out.ExcludeKinds = append(out.ExcludeKinds, t)
		}
	}
	if f.SchemaVersion != nil {
		if *f.SchemaVersion < 1 {
			return out, invalid("out_of_range", "?schema_version", "schema_version must be an integer of at least 1, got %d", *f.SchemaVersion)
		}
		v := *f.SchemaVersion
		out.SchemaVersion = &v
	}
	if f.ContentHash != "" && !hexHash.MatchString(f.ContentHash) {
		return out, invalid("invalid_format", "?content_hash", "content_hash must be 64 lower-case hex digits, got %s", short(f.ContentHash))
	}
	switch f.On {
	case "", store.OnCreatedAt, store.OnOccurredAt:
		out.On = f.On
	default:
		return out, invalid("out_of_range", "?on", "on must be created_at or occurred_at, got %s", short(f.On))
	}
	if len(f.Q) > QMaxBytes {
		return out, invalid("too_long", "?q", "q must be at most %d bytes, got %d", QMaxBytes, len(f.Q))
	}
	if f.Since != nil {
		v := f.Since.UnixMicro()
		out.Since = &v
	}
	if f.Until != nil {
		v := f.Until.UnixMicro()
		out.Until = &v
	}
	if out.Since != nil && out.Until != nil && *out.Since > *out.Until {
		return out, invalid("out_of_range", "?since", "since (%s) must not be after until (%s)",
			formatMicros(*out.Since), formatMicros(*out.Until))
	}
	out.Key, out.Machine, out.Model, out.Project = f.Key, f.Machine, f.Model, f.Project
	out.Processed, out.Redacted = f.Processed, f.Redacted
	out.ContentHash, out.Q = f.ContentHash, f.Q
	return out, nil
}

// ListParams are the list route's parameters. A nil pointer is an absent
// parameter; Limit nil means ListDefault.
type ListParams struct {
	Filter
	BeforeID       *int64
	AfterID        *int64
	Limit          *int
	IncludePayload bool
}

// ListResult is one page: rows newest first (or oldest first with AfterID),
// total over the whole filtered set, and the cursor of the next page in the
// direction used.
type ListResult struct {
	Submissions  []Record `json:"submissions"`
	Limit        int      `json:"limit"`
	Total        int64    `json:"total"`
	HasMore      bool     `json:"has_more"`
	NextBeforeID *int64   `json:"next_before_id"`
	NextAfterID  *int64   `json:"next_after_id"`
}

// List validates p and returns one page and the total in one read
// transaction.
func (s *Service) List(ctx context.Context, p ListParams) (ListResult, error) {
	f, err := p.storeFilter()
	if err != nil {
		return ListResult{}, err
	}
	limit, max := ListDefault, ListMax
	if p.IncludePayload {
		max = ListMaxWithPayload
	}
	if p.Limit != nil {
		limit = *p.Limit
		if limit < 1 || limit > max {
			if p.IncludePayload {
				return ListResult{}, invalid("out_of_range", "?limit", "limit must be between 1 and %d with include=payload, got %d", max, limit)
			}
			return ListResult{}, invalid("out_of_range", "?limit", "limit must be between 1 and %d, got %d", max, limit)
		}
	}
	if p.BeforeID != nil && p.AfterID != nil {
		return ListResult{}, invalid("invalid_value", "?before_id", "before_id and after_id are mutually exclusive; send one")
	}
	if p.BeforeID != nil && *p.BeforeID < 1 {
		return ListResult{}, invalid("out_of_range", "?before_id", "before_id must be an integer of at least 1, got %d", *p.BeforeID)
	}
	if p.AfterID != nil && *p.AfterID < 0 {
		return ListResult{}, invalid("out_of_range", "?after_id", "after_id must be an integer of at least 0, got %d", *p.AfterID)
	}
	page := store.Page{BeforeID: p.BeforeID, AfterID: p.AfterID, Limit: limit + 1}
	res := ListResult{Limit: limit}
	err = s.read(ctx, func(q store.Querier) error {
		rows, err := store.List(ctx, q, f, page, p.IncludePayload)
		if err != nil {
			return err
		}
		if res.Total, err = store.Count(ctx, q, f); err != nil {
			return err
		}
		if len(rows) > limit {
			rows = rows[:limit]
			res.HasMore = true
			last := rows[limit-1].ID
			if p.AfterID != nil {
				res.NextAfterID = &last
			} else {
				res.NextBeforeID = &last
			}
		}
		res.Submissions = records(rows)
		return nil
	})
	if err != nil {
		return ListResult{}, err
	}
	return res, nil
}

// StatsParams are the stats route's parameters. By lists up to
// store.MaxGroupKeys names from store.GroupKeys; Top nil means the default;
// Bucket is "", day or week.
type StatsParams struct {
	Filter
	By     []string
	Top    *int
	Bucket string
}

// Group is one stats group.
type Group struct {
	Keys      map[string]any `json:"keys"`
	Total     int64          `json:"total"`
	Open      int64          `json:"open"`
	Processed int64          `json:"processed"`
}

// Recurrence is one recurring content hash.
type Recurrence struct {
	ContentHash string `json:"content_hash"`
	Count       int64  `json:"count"`
	FirstID     int64  `json:"first_id"`
	LastID      int64  `json:"last_id"`
	Summary     string `json:"summary,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Project     string `json:"project,omitempty"`
}

// Bucket is one point of the time series.
type Bucket struct {
	Bucket string `json:"bucket"`
	Total  int64  `json:"total"`
	Open   int64  `json:"open"`
}

// Stats is the stats response. Groups is present only when By was given,
// Series only when Bucket was; Recurring always.
type Stats struct {
	Total     int64   `json:"total"`
	Open      int64   `json:"open"`
	Processed int64   `json:"processed"`
	Redacted  int64   `json:"redacted"`
	Groups    []Group // nil when not requested; non-nil, maybe empty, when requested
	Recurring []Recurrence
	Series    []Bucket // nil when not requested; non-nil, maybe empty, when requested
}

// MarshalJSON writes the Stats body: groups and series are omitted when nil
// (not requested) and written as [] when requested and empty.
func (s Stats) MarshalJSON() ([]byte, error) {
	out := struct {
		Total     int64        `json:"total"`
		Open      int64        `json:"open"`
		Processed int64        `json:"processed"`
		Redacted  int64        `json:"redacted"`
		Groups    *[]Group     `json:"groups,omitempty"`
		Recurring []Recurrence `json:"recurring"`
		Series    *[]Bucket    `json:"series,omitempty"`
	}{Total: s.Total, Open: s.Open, Processed: s.Processed, Redacted: s.Redacted, Recurring: s.Recurring}
	if out.Recurring == nil {
		out.Recurring = []Recurrence{}
	}
	if s.Groups != nil {
		out.Groups = &s.Groups
	}
	if s.Series != nil {
		out.Series = &s.Series
	}
	return Marshal(out)
}

// Stats validates p and runs the totals, groups, recurring hashes and series
// in one read transaction.
func (s *Service) Stats(ctx context.Context, p StatsParams) (Stats, error) {
	f, err := p.storeFilter()
	if err != nil {
		return Stats{}, err
	}
	if len(p.By) > store.MaxGroupKeys {
		return Stats{}, invalid("out_of_range", "?by", "by takes at most %d keys, got %d", store.MaxGroupKeys, len(p.By))
	}
	for i, k := range p.By {
		if !slices.Contains(store.GroupKeys, k) {
			return Stats{}, invalid("out_of_range", "?by", "by key %s is not one of %s", short(k), strings.Join(store.GroupKeys, ", "))
		}
		if slices.Contains(p.By[:i], k) {
			return Stats{}, invalid("invalid_value", "?by", "by key %s is repeated", short(k))
		}
	}
	top := StatsTopDefault
	if p.Top != nil {
		top = *p.Top
		if top < 0 || top > StatsTopMax {
			return Stats{}, invalid("out_of_range", "?top", "top must be between 0 and %d, got %d", StatsTopMax, top)
		}
	}
	switch p.Bucket {
	case "", store.BucketDay, store.BucketWeek:
	default:
		return Stats{}, invalid("out_of_range", "?bucket", "bucket must be day or week, got %s", short(p.Bucket))
	}
	var out Stats
	err = s.read(ctx, func(q store.Querier) error {
		t, err := store.StatsTotals(ctx, q, f)
		if err != nil {
			return err
		}
		out.Total, out.Open, out.Processed, out.Redacted = t.Total, t.Open, t.Processed, t.Redacted
		if len(p.By) > 0 {
			groups, err := store.StatsGroups(ctx, q, f, p.By)
			if err != nil {
				return err
			}
			out.Groups = make([]Group, len(groups)) // present when requested, even empty
			for i, g := range groups {
				out.Groups[i] = Group{Keys: g.Keys, Total: g.Total, Open: g.Open, Processed: g.Processed}
			}
		}
		rec, err := store.StatsRecurring(ctx, q, f, top)
		if err != nil {
			return err
		}
		out.Recurring = make([]Recurrence, len(rec))
		for i, r := range rec {
			out.Recurring[i] = Recurrence(r)
		}
		if p.Bucket != "" {
			series, err := store.StatsSeries(ctx, q, f, p.Bucket)
			if err != nil {
				return err
			}
			out.Series = make([]Bucket, len(series))
			for i, b := range series {
				out.Series[i] = Bucket(b)
			}
		}
		return nil
	})
	if err != nil {
		return Stats{}, err
	}
	return out, nil
}
