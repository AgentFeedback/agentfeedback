package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// mark is a validated processing mark.
type mark struct {
	processed bool
	fields    store.Mark // nil fields were absent and keep the stored value
}

var plainInteger = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseMark reads a ProcessingMark body (withIDs false) or a BatchMark body
// (withIDs true) strictly: one JSON object at most envelope.BodyLimit bytes,
// no duplicate or unknown members, processed a boolean (default true, null
// is 400), verdict a token of at most VerdictBytes, resolution trimmed and
// non-blank of at most ResolutionBytes, ref and processed_by trimmed of at
// most RefBytes and ProcessedByBytes, the four allowed only with
// processed=true. A blank resolution is 400; a verdict, ref or processed_by
// blank after normalisation clears that field (stored as NULL). Over a
// limit is 400, never a truncation. ids is 1 to ProcessedIDsMax positive
// integers, duplicates collapsed, returned ascending.
func parseMark(body []byte, withIDs bool) (mark, []int64, error) {
	if len(body) > envelope.BodyLimit {
		return mark{}, nil, &Problem{Status: 413, Code: CodeRequestTooLarge,
			Message: fmt.Sprintf("body over %d bytes", envelope.BodyLimit)}
	}
	members, err := readObject(body)
	if err != nil {
		return mark{}, nil, bodyProblem(err)
	}
	m := mark{processed: true}
	var ids []int64
	idsSeen := false
	var given []string
	for _, mem := range members {
		ptr := "/" + schema.EscapeToken(mem.name)
		switch mem.name {
		case "processed":
			switch string(mem.raw) {
			case "true":
				m.processed = true
			case "false":
				m.processed = false
			default:
				return mark{}, nil, invalid("type_mismatch", ptr, "processed must be true or false, got %s", jsonType(mem.raw))
			}
		case "verdict", "resolution", "ref", "processed_by":
			var v string
			if jsonType(mem.raw) != "a string" || json.Unmarshal(mem.raw, &v) != nil {
				return mark{}, nil, invalid("type_mismatch", ptr, "%s must be a string, got %s", mem.name, jsonType(mem.raw))
			}
			var limit int
			var field **string
			switch mem.name {
			case "verdict":
				v, limit, field = schema.Token(v), VerdictBytes, &m.fields.Verdict
			case "resolution":
				v, limit, field = schema.Trim(v), ResolutionBytes, &m.fields.Resolution
			case "ref":
				v, limit, field = schema.Trim(v), RefBytes, &m.fields.Ref
			default:
				v, limit, field = schema.Trim(v), ProcessedByBytes, &m.fields.ProcessedBy
			}
			if v == "" && mem.name == "resolution" {
				return mark{}, nil, invalid("out_of_range", ptr, "resolution must not be blank; omit it to keep the stored value")
			}
			if len(v) > limit {
				return mark{}, nil, invalid("too_long", ptr, "%s must be at most %d bytes after trimming, got %d", mem.name, limit, len(v))
			}
			*field = &v
			given = append(given, mem.name)
		case "ids":
			if !withIDs {
				return mark{}, nil, unknownMember(mem.name, "processed, verdict, resolution, ref, processed_by")
			}
			idsSeen = true
			if ids, err = parseIDs(mem.raw); err != nil {
				return mark{}, nil, err
			}
		default:
			if withIDs {
				return mark{}, nil, unknownMember(mem.name, "ids, processed, verdict, resolution, ref, processed_by")
			}
			return mark{}, nil, unknownMember(mem.name, "processed, verdict, resolution, ref, processed_by")
		}
	}
	if !m.processed && len(given) > 0 {
		return mark{}, nil, invalid("invalid_value", "/"+given[0], "%s is allowed only with processed=true", given[0])
	}
	if withIDs && !idsSeen {
		return mark{}, nil, invalid("required", "/ids", "ids is required: an array of 1 to %d positive integers", ProcessedIDsMax)
	}
	return m, ids, nil
}

func unknownMember(name, accepted string) *Problem {
	return invalid("unknown_field", "/"+schema.EscapeToken(name), "member %s is not accepted; accepted members: %s", short(name), accepted)
}

func parseIDs(raw json.RawMessage) ([]int64, error) {
	var items []json.RawMessage
	if jsonType(raw) != "an array" || json.Unmarshal(raw, &items) != nil {
		return nil, invalid("type_mismatch", "/ids", "ids must be an array of 1 to %d positive integers, got %s", ProcessedIDsMax, jsonType(raw))
	}
	if len(items) < 1 || len(items) > ProcessedIDsMax {
		return nil, invalid("out_of_range", "/ids", "ids must hold 1 to %d ids, got %d", ProcessedIDsMax, len(items))
	}
	ids := make([]int64, 0, len(items))
	for i, item := range items {
		id, err := strconv.ParseInt(string(item), 10, 64)
		if !plainInteger.Match(item) || err != nil {
			return nil, invalid("out_of_range", fmt.Sprintf("/ids/%d", i), "ids[%d] must be a positive integer, got %s", i, short(string(item)))
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// BatchResult classifies the ids of a batch mark, each list ascending.
type BatchResult struct {
	Processed bool    `json:"processed"`
	Verdict   *string `json:"verdict,omitempty"`
	Updated   []int64 `json:"updated"`
	Unchanged []int64 `json:"unchanged"`
	NotFound  []int64 `json:"not_found"`
}

// applyMark classifies ids against their stored state and updates the
// updated ones with one statement, all on q. unchanged means already
// processed with every given field equal (processed=true), or not processed
// (processed=false).
func (s *Service) applyMark(ctx context.Context, q store.Querier, m mark, ids []int64) (BatchResult, error) {
	res := BatchResult{Processed: m.processed, Verdict: m.fields.Verdict,
		Updated: []int64{}, Unchanged: []int64{}, NotFound: []int64{}}
	states, err := store.ProcessingStates(ctx, q, ids)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		st, ok := states[id]
		switch {
		case !ok:
			res.NotFound = append(res.NotFound, id)
		case unchanged(st, m):
			res.Unchanged = append(res.Unchanged, id)
		default:
			res.Updated = append(res.Updated, id)
		}
	}
	var n int64
	if m.processed {
		n, err = store.MarkProcessed(ctx, q, res.Updated, s.nowMicros(), m.fields)
	} else {
		n, err = store.UnmarkProcessed(ctx, q, res.Updated)
	}
	if err != nil {
		return res, err
	}
	if n != int64(len(res.Updated)) {
		return res, fmt.Errorf("mark touched %d rows, expected %d", n, len(res.Updated))
	}
	return res, nil
}

func unchanged(st store.ProcessingState, m mark) bool {
	if !m.processed {
		return st.ProcessedAt == nil
	}
	if st.ProcessedAt == nil {
		return false
	}
	for _, f := range []struct {
		given  *string
		stored string
	}{
		{m.fields.Verdict, st.Verdict}, {m.fields.Resolution, st.Resolution},
		{m.fields.Ref, st.Ref}, {m.fields.ProcessedBy, st.ProcessedBy},
	} {
		if f.given != nil && *f.given != f.stored {
			return false
		}
	}
	return true
}

// Mark applies a ProcessingMark body to one row and returns the record as
// it stands after the update, read in the same write transaction. The first
// mark sets processed_at; a re-mark keeps it and replaces the given fields;
// processed=false clears all five processing fields. Tombstones can be
// marked. An unknown id is 404.
func (s *Service) Mark(ctx context.Context, id int64, body []byte) (Record, error) {
	if err := checkID(id); err != nil {
		return Record{}, err
	}
	m, _, err := parseMark(body, false)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	err = s.write(ctx, func(q store.Querier) error {
		res, err := s.applyMark(ctx, q, m, []int64{id})
		if err != nil {
			return err
		}
		if len(res.NotFound) > 0 {
			return notFound(id)
		}
		sub, err := store.GetByID(ctx, q, id)
		rec = Record{sub}
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// MarkBatch applies a BatchMark body in one write transaction and classifies
// the ids into updated, unchanged and not_found.
func (s *Service) MarkBatch(ctx context.Context, body []byte) (BatchResult, error) {
	m, ids, err := parseMark(body, true)
	if err != nil {
		return BatchResult{}, err
	}
	var res BatchResult
	err = s.write(ctx, func(q store.Querier) error {
		res, err = s.applyMark(ctx, q, m, ids)
		return err
	})
	if err != nil {
		return BatchResult{}, err
	}
	return res, nil
}

// Redact turns the row into its tombstone and returns it; repeating it
// returns the tombstone unchanged. An unknown id is 404.
func (s *Service) Redact(ctx context.Context, id int64) (Record, error) {
	if err := checkID(id); err != nil {
		return Record{}, err
	}
	var rec Record
	err := s.write(ctx, func(q store.Querier) error {
		if _, err := store.Redact(ctx, q, id, s.nowMicros()); err != nil {
			return err
		}
		sub, err := store.GetByID(ctx, q, id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(id)
		}
		rec = Record{sub}
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}
