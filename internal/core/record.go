package core

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/store"
	"github.com/agentfeedback/agentfeedback/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// Record is one stored submission as the contract serialises it. Its JSON
// form lists the members in the order of docs/openapi.yaml's Submission
// properties (id, uid, kind, schema_version, key, summary, machine, model,
// harness, project, occurred_at, context, payload, content_hash,
// created_at, processed_at, redacted_at, verdict, resolution, ref,
// processed_by), omits unset
// optionals (never null), writes timestamps as RFC 3339 UTC with six
// fractional digits, writes payload and context as the stored bytes
// unchanged, and never HTML-escapes. A list row fetched without its payload
// has no payload member. Get, list and export share this encoder, so they
// agree byte for byte.
type Record struct {
	store.Submission
}

// MarshalJSON implements json.Marshaler.
func (r Record) MarshalJSON() ([]byte, error) { return r.AppendJSON(nil), nil }

// AppendJSON appends the record's JSON form to dst.
func (r Record) AppendJSON(dst []byte) []byte {
	s := r.Submission
	w := objectWriter{dst: append(dst, '{')}
	w.int("id", s.ID)
	w.str("uid", s.UID)
	w.str("kind", s.Kind)
	w.int("schema_version", s.SchemaVersion)
	w.optStr("key", s.Key)
	w.optStr("summary", s.Summary)
	w.optStr("machine", s.Machine)
	w.optStr("model", s.Model)
	w.optStr("harness", s.Harness)
	w.optStr("project", s.Project)
	w.optTime("occurred_at", s.OccurredAt)
	w.raw("context", s.Context)
	w.raw("payload", s.Payload)
	w.str("content_hash", s.ContentHash)
	w.time("created_at", s.CreatedAt)
	w.optTime("processed_at", s.ProcessedAt)
	w.optTime("redacted_at", s.RedactedAt)
	w.optStr("verdict", s.Verdict)
	w.optStr("resolution", s.Resolution)
	w.optStr("ref", s.Ref)
	w.optStr("processed_by", s.ProcessedBy)
	return append(w.dst, '}')
}

type objectWriter struct {
	dst []byte
	n   int
}

func (w *objectWriter) name(name string) {
	if w.n > 0 {
		w.dst = append(w.dst, ',')
	}
	w.n++
	w.dst = canonjson.AppendString(w.dst, name)
	w.dst = append(w.dst, ':')
}

func (w *objectWriter) int(name string, v int64) {
	w.name(name)
	w.dst = strconv.AppendInt(w.dst, v, 10)
}

func (w *objectWriter) str(name, v string) {
	w.name(name)
	w.dst = canonjson.AppendString(w.dst, v)
}

func (w *objectWriter) optStr(name, v string) {
	if v != "" {
		w.str(name, v)
	}
}

func (w *objectWriter) time(name string, micros int64) {
	w.str(name, formatMicros(micros))
}

func (w *objectWriter) optTime(name string, micros *int64) {
	if micros != nil {
		w.time(name, *micros)
	}
}

func (w *objectWriter) raw(name string, v json.RawMessage) {
	if v != nil {
		w.name(name)
		w.dst = append(w.dst, v...)
	}
}

// formatMicros writes unix microseconds in the stored timestamp form.
func formatMicros(micros int64) string {
	return schema.FormatDateTime(time.UnixMicro(micros))
}

// parseStoredTime parses a timestamp with the contract's RFC 3339 rules into
// unix microseconds.
func parseStoredTime(text string) (int64, bool) {
	t, ok := schema.ParseDateTime(text)
	if !ok {
		return 0, false
	}
	return t.UnixMicro(), true
}

// Marshal writes v as JSON without HTML escaping. Records inside v keep
// their stored bytes; other strings are escaped as encoding/json does apart
// from <, > and &.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func records(subs []store.Submission) []Record {
	out := make([]Record, len(subs))
	for i, s := range subs {
		out[i] = Record{s}
	}
	return out
}
