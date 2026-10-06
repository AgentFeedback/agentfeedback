package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

// ExportParams are the export route's parameters; a nil pointer or an empty
// Kind is absent. Limit nil exports every row.
type ExportParams struct {
	Kind    string
	Since   *time.Time
	AfterID *int64
	Limit   *int
}

// Export writes export format 2 to w from one read transaction, streaming:
// a header line {"export_format":2,"kind","since","after_id","exported_at"},
// one line per full record in ascending id (tombstones as tombstones) and a
// trailer {"export_complete":true,"count","first_id","last_id","sha256"},
// where sha256 is the lower-case hex SHA-256 over the record lines, each
// with its trailing newline. Validation failures are returned before
// anything is written.
func (s *Service) Export(ctx context.Context, p ExportParams, w io.Writer) error {
	kind, err := token("kind", p.Kind)
	if err != nil {
		return err
	}
	f := store.ExportFilter{Kind: kind}
	if p.Since != nil {
		v := p.Since.UnixMicro()
		f.Since = &v
	}
	if p.AfterID != nil {
		if *p.AfterID < 0 {
			return invalid("out_of_range", "?after_id", "after_id must be an integer of at least 0, got %d", *p.AfterID)
		}
		f.AfterID = p.AfterID
	}
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > ExportMax {
			return invalid("out_of_range", "?limit", "limit must be between 1 and %d, got %d", ExportMax, *p.Limit)
		}
		f.Limit = *p.Limit
	}
	exportedAt := s.nowMicros()
	return s.read(ctx, func(q store.Querier) error {
		h := objectWriter{dst: []byte{'{'}}
		h.int("export_format", ExportFormat)
		h.name("kind")
		h.dst = appendNullableString(h.dst, kind)
		h.name("since")
		if f.Since != nil {
			h.dst = canonjson.AppendString(h.dst, formatMicros(*f.Since))
		} else {
			h.dst = append(h.dst, "null"...)
		}
		h.name("after_id")
		h.dst = appendNullableInt(h.dst, f.AfterID)
		h.time("exported_at", exportedAt)
		if _, err := w.Write(append(h.dst, '}', '\n')); err != nil {
			return err
		}

		digest := sha256.New()
		var count int64
		var first, last *int64
		var line []byte
		err := store.Iterate(ctx, q, f, func(sub store.Submission) error {
			line = append(Record{sub}.AppendJSON(line[:0]), '\n')
			digest.Write(line)
			if _, err := w.Write(line); err != nil {
				return err
			}
			count++
			id := sub.ID
			if first == nil {
				first = &id
			}
			last = &id
			return nil
		})
		if err != nil {
			return err
		}
		t := objectWriter{dst: []byte{'{'}}
		t.name("export_complete")
		t.dst = append(t.dst, "true"...)
		t.int("count", count)
		t.name("first_id")
		t.dst = appendNullableInt(t.dst, first)
		t.name("last_id")
		t.dst = appendNullableInt(t.dst, last)
		t.str("sha256", hex.EncodeToString(digest.Sum(nil)))
		_, err = w.Write(append(t.dst, '}', '\n'))
		return err
	})
}

func appendNullableString(dst []byte, v string) []byte {
	if v == "" {
		return append(dst, "null"...)
	}
	return canonjson.AppendString(dst, v)
}

func appendNullableInt(dst []byte, v *int64) []byte {
	if v == nil {
		return append(dst, "null"...)
	}
	return strconv.AppendInt(dst, *v, 10)
}
