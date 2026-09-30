package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/internal/store"
	"github.com/agentfeedback/agentfeedback/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// Conflict is an imported record skipped because its (kind, key) names a
// stored row under another uid with different content.
type Conflict struct {
	Line       int    `json:"line"`
	UID        string `json:"uid"`
	ExistingID int64  `json:"existing_id"`
	Reason     string `json:"reason"`
}

// ImportWarning is a normalisation warning of an imported record.
type ImportWarning struct {
	Line    int    `json:"line"`
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// ImportResult is the import response. FirstID and LastID are the ids
// assigned to the first and last imported record, absent when none was.
type ImportResult struct {
	Imported  int             `json:"imported"`
	Skipped   int             `json:"skipped"`
	Conflicts []Conflict      `json:"conflicts"`
	Warnings  []ImportWarning `json:"warnings"`
	FirstID   *int64          `json:"first_id,omitempty"`
	LastID    *int64          `json:"last_id,omitempty"`
}

// importRecord is one verified record line, ready to insert.
type importRecord struct {
	line     int
	sub      store.Submission
	warnings []schema.Detail
}

// recordMembers are the members of an export record line, the Submission
// schema's properties; required ones are marked.
var recordMembers = map[string]bool{
	"id": true, "uid": true, "kind": true, "schema_version": true, "key": false, "summary": false,
	"machine": false, "model": false, "harness": false, "project": false, "occurred_at": false,
	"context": false, "payload": true, "content_hash": true, "created_at": true, "processed_at": false,
	"verdict": false, "resolution": false, "ref": false, "processed_by": false, "redacted_at": false,
}

// requiredRecordMembers are recordMembers' required members in the
// Submission schema's order, so a missing-member error is deterministic.
var requiredRecordMembers = []string{"id", "uid", "kind", "schema_version", "payload", "content_hash", "created_at"}

// envelopeMembers are the record members rebuilt into a body for the decoder.
var envelopeMembers = []string{"kind", "schema_version", "key", "summary", "machine", "model", "harness",
	"project", "occurred_at", "context", "payload"}

// uuidForm is an RFC 9562 UUID in 8-4-4-4-12 hex form: version 1 to 8 in
// the third group's first digit, the RFC variant (8, 9, a or b) in the
// fourth group's first digit.
var uuidForm = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// Import reads export format 2 and stores its records. The whole body is
// verified before the write transaction opens (header export_format 2; the
// trailer last, its count and sha256 matching the raw record lines; each
// record line one strict JSON object), and any failure is 400
// validation_error naming the 1-based line with nothing written. Envelope
// members go through the decoder, so normalisation and limits are create's
// (with ImportLimit as the body limit); warnings carry their line. An
// ordinary record's content_hash is recomputed and must match; a tombstone
// (redacted_at present) keeps its exported hash and reports no warnings.
// In one write transaction, per record: an existing uid is skipped; an
// existing (kind, key) under another uid is skipped, and listed in conflicts
// when its hash differs; anything else is inserted with a new id, keeping
// uid, created_at, occurred_at, the processing fields and redacted_at.
func (s *Service) Import(ctx context.Context, body []byte) (ImportResult, error) {
	if len(body) > ImportLimit {
		return ImportResult{}, &Problem{Status: 413, Code: CodeRequestTooLarge,
			Message: fmt.Sprintf("import body over %d bytes", ImportLimit)}
	}
	recs, err := verifyImport(body)
	if err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Conflicts: []Conflict{}, Warnings: []ImportWarning{}}
	err = s.write(ctx, func(q store.Querier) error {
		for _, r := range recs {
			_, err := store.GetByUID(ctx, q, r.sub.UID)
			if err == nil {
				res.Skipped++
				continue
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if r.sub.Key != "" {
				existing, err := store.GetByKey(ctx, q, r.sub.Kind, r.sub.Key)
				if err == nil {
					res.Skipped++
					if existing.ContentHash != r.sub.ContentHash {
						res.Conflicts = append(res.Conflicts, Conflict{Line: r.line, UID: r.sub.UID,
							ExistingID: existing.ID, Reason: "key_mismatch"})
					}
					continue
				}
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			id, err := store.Insert(ctx, q, r.sub)
			if err != nil {
				return err
			}
			res.Imported++
			if res.FirstID == nil {
				first := id
				res.FirstID = &first
			}
			last := id
			res.LastID = &last
			for _, w := range r.warnings {
				res.Warnings = append(res.Warnings, ImportWarning{Line: r.line, Code: w.Code, Pointer: w.Pointer, Message: w.Message})
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// errDryRun rolls back a dry-run restore's transaction.
var errDryRun = errors.New("dry run")

// Restore reads export format 2 and stores its records keeping their ids: the
// server-host restore path, never the API's import route. The body is
// verified exactly as Import verifies it, with nothing written on failure,
// but has no body limit (the file can be a whole database). In one write
// transaction, per record in file order: an existing uid is skipped, so a
// re-run is a no-op; an id held by another uid is skipped and listed in
// conflicts as id_taken; an existing (kind, key) under another uid is
// skipped, and listed as key_mismatch when its hash differs; anything else is
// inserted with its exported id. The id sequence is then advanced past the
// highest id in the stream, skipped records included, so restored ids are
// never handed out again. A dry run runs the same transaction and rolls it
// back, so its counts are a real run's.
func (s *Service) Restore(ctx context.Context, body []byte, dryRun bool) (ImportResult, error) {
	recs, err := verifyImport(body)
	if err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Conflicts: []Conflict{}, Warnings: []ImportWarning{}}
	err = s.write(ctx, func(q store.Querier) error {
		var maxID int64
		for _, r := range recs {
			maxID = max(maxID, r.sub.ID)
			_, err := store.GetByUID(ctx, q, r.sub.UID)
			if err == nil {
				res.Skipped++
				continue
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			_, err = store.GetByID(ctx, q, r.sub.ID)
			if err == nil {
				res.Skipped++
				res.Conflicts = append(res.Conflicts, Conflict{Line: r.line, UID: r.sub.UID,
					ExistingID: r.sub.ID, Reason: "id_taken"})
				continue
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if r.sub.Key != "" {
				existing, err := store.GetByKey(ctx, q, r.sub.Kind, r.sub.Key)
				if err == nil {
					res.Skipped++
					if existing.ContentHash != r.sub.ContentHash {
						res.Conflicts = append(res.Conflicts, Conflict{Line: r.line, UID: r.sub.UID,
							ExistingID: existing.ID, Reason: "key_mismatch"})
					}
					continue
				}
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			if err := store.InsertWithID(ctx, q, r.sub); err != nil {
				return err
			}
			res.Imported++
			if res.FirstID == nil {
				first := r.sub.ID
				res.FirstID = &first
			}
			last := r.sub.ID
			res.LastID = &last
			for _, w := range r.warnings {
				res.Warnings = append(res.Warnings, ImportWarning{Line: r.line, Code: w.Code, Pointer: w.Pointer, Message: w.Message})
			}
		}
		if maxID > 0 {
			if err := store.SetSequence(ctx, q, maxID); err != nil {
				return err
			}
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !(dryRun && errors.Is(err, errDryRun)) {
		return ImportResult{}, err
	}
	return res, nil
}

// VerifyExport verifies an export (format 2) exactly as Import and Restore
// do, writing nothing; a rejection is the same *Problem they return.
func VerifyExport(body []byte) error {
	_, err := verifyImport(body)
	return err
}

func lineProblem(line int, code, pointer, format string, args ...any) *Problem {
	return invalid(code, pointer, "line %d: %s", line, fmt.Sprintf(format, args...))
}

// verifyImport is the phase before any write: framing, digest and every
// record line.
func verifyImport(body []byte) ([]importRecord, error) {
	lines := bytes.Split(body, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1] // one newline after the trailer is allowed
	}
	for i, l := range lines {
		if len(l) == 0 {
			return nil, lineProblem(i+1, "invalid_format", "", "blank line; every line must be one JSON object")
		}
		if l[len(l)-1] == '\r' {
			return nil, lineProblem(i+1, "invalid_format", "", "line ends in CR; lines must end in LF alone")
		}
	}
	if len(lines) < 2 {
		return nil, invalid("invalid_format", "", "import must hold a header line and a trailer line, got %d lines", len(lines))
	}

	header, err := readObject(lines[0])
	if err != nil {
		return nil, lineProblem(1, "invalid_format", "", "header is not a JSON object: %v", err)
	}
	if raw, ok := find(header, "export_format"); !ok || string(raw) != "2" {
		return nil, lineProblem(1, "invalid_format", "/export_format", "export_format must be 2")
	}

	last := len(lines)
	trailer, err := readObject(lines[last-1])
	if err != nil {
		return nil, lineProblem(last, "invalid_format", "", "trailer is not a JSON object: %v", err)
	}
	if raw, ok := find(trailer, "export_complete"); !ok || string(raw) != "true" {
		return nil, lineProblem(last, "invalid_format", "/export_complete",
			"the last line must be the trailer with export_complete true; the export is incomplete")
	}
	records := lines[1 : last-1]
	rawCount, _ := find(trailer, "count")
	count, err := strconv.ParseInt(string(rawCount), 10, 64)
	if err != nil || count != int64(len(records)) {
		return nil, lineProblem(last, "invalid_value", "/count", "trailer count %s does not match the %d record lines",
			short(string(rawCount)), len(records))
	}
	digest := sha256.New()
	for _, l := range records {
		digest.Write(l)
		digest.Write([]byte{'\n'})
	}
	rawSum, _ := find(trailer, "sha256")
	var sum string
	if err := json.Unmarshal(rawSum, &sum); err != nil {
		return nil, lineProblem(last, "invalid_value", "/sha256", "trailer sha256 must be a string, got %s", short(string(rawSum)))
	}
	if sum != hex.EncodeToString(digest.Sum(nil)) {
		return nil, lineProblem(last, "invalid_value", "/sha256", "trailer sha256 %s does not match the record lines", short(sum))
	}

	out := make([]importRecord, 0, len(records))
	for i, l := range records {
		r, err := verifyRecord(i+2, l)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func find(members []member, name string) (json.RawMessage, bool) {
	for _, m := range members {
		if m.name == name {
			return m.raw, true
		}
	}
	return nil, false
}

// verifyRecord checks one record line and rebuilds its row.
func verifyRecord(line int, raw []byte) (importRecord, error) {
	members, err := readObject(raw)
	if err != nil {
		var dup *duplicateError
		if errors.As(err, &dup) {
			return importRecord{}, lineProblem(line, "duplicate_key", "/"+schema.EscapeToken(dup.name), "member %s appears more than once", short(dup.name))
		}
		return importRecord{}, lineProblem(line, "invalid_format", "", "record is not one JSON object: %v", err)
	}
	byName := map[string]json.RawMessage{}
	for _, m := range members {
		if _, ok := recordMembers[m.name]; !ok {
			return importRecord{}, lineProblem(line, "unknown_field", "/"+schema.EscapeToken(m.name), "member %s is not a record member", short(m.name))
		}
		byName[m.name] = m.raw
	}
	for _, name := range requiredRecordMembers {
		if _, ok := byName[name]; !ok {
			return importRecord{}, lineProblem(line, "required", "/"+name, "%s is required", name)
		}
	}
	str := func(name string) (string, bool, error) {
		raw, ok := byName[name]
		if !ok {
			return "", false, nil
		}
		var v string
		if jsonType(raw) != "a string" || json.Unmarshal(raw, &v) != nil {
			return "", false, lineProblem(line, "type_mismatch", "/"+name, "%s must be a string, got %s", name, jsonType(raw))
		}
		return v, true, nil
	}
	timestamp := func(name string) (*int64, error) {
		v, ok, err := str(name)
		if err != nil || !ok {
			return nil, err
		}
		micros, ok := parseStoredTime(v)
		if !ok {
			return nil, lineProblem(line, "invalid_format", "/"+name, "%s must be an RFC 3339 date-time, got %s", name, short(v))
		}
		return &micros, nil
	}

	rawID := byName["id"]
	id, idErr := strconv.ParseInt(string(rawID), 10, 64)
	if !plainInteger.Match(rawID) || idErr != nil {
		return importRecord{}, lineProblem(line, "type_mismatch", "/id", "id must be a positive integer, got %s", short(string(rawID)))
	}
	var sub store.Submission
	uid, _, err := str("uid")
	if err != nil {
		return importRecord{}, err
	}
	if !uuidForm.MatchString(uid) {
		return importRecord{}, lineProblem(line, "invalid_format", "/uid", "uid must be an RFC 9562 UUID in 8-4-4-4-12 hex form (version 1-8, variant 8, 9, a or b), got %s", short(uid))
	}
	hash, _, err := str("content_hash")
	if err != nil {
		return importRecord{}, err
	}
	if !hexHash.MatchString(hash) {
		return importRecord{}, lineProblem(line, "invalid_format", "/content_hash", "content_hash must be 64 lower-case hex digits, got %s", short(hash))
	}
	kind, _, err := str("kind")
	if err != nil {
		return importRecord{}, err
	}
	created, err := timestamp("created_at")
	if err != nil {
		return importRecord{}, err
	}
	for _, name := range []string{"occurred_at", "processed_at", "redacted_at"} {
		if _, err := timestamp(name); err != nil {
			return importRecord{}, err
		}
	}
	processedAt, _ := timestamp("processed_at")
	redactedAt, _ := timestamp("redacted_at")
	processing := map[string]*string{}
	for _, name := range []string{"verdict", "resolution", "ref", "processed_by"} {
		v, ok, err := str(name)
		if err != nil {
			return importRecord{}, err
		}
		if ok && processedAt == nil {
			return importRecord{}, lineProblem(line, "invalid_value", "/"+name, "%s is present but processed_at is not", name)
		}
		processing[name] = &v
	}

	tombstone := redactedAt != nil
	if tombstone {
		var compact bytes.Buffer
		if err := json.Compact(&compact, byName["payload"]); err != nil || compact.String() != store.Tombstone {
			return importRecord{}, lineProblem(line, "invalid_value", "/payload", "a redacted record's payload must be %s", store.Tombstone)
		}
		for _, name := range []string{"summary", "context"} {
			if _, ok := byName[name]; ok {
				return importRecord{}, lineProblem(line, "invalid_value", "/"+name, "a redacted record has no %s", name)
			}
		}
	}

	// Rebuild the envelope as a body. An inferred kind is left out so the
	// decoder infers it again, as create did.
	var b strings.Builder
	b.WriteByte('{')
	for _, name := range envelopeMembers {
		v, ok := byName[name]
		if !ok || (name == "kind" && kind == store.KindUnknown) {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(name))
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	env, warnings, err := envelope.DecodeWithLimits([]byte(b.String()), ImportLimit, ImportDepth)
	if err != nil {
		var r *envelope.Rejection
		if errors.As(err, &r) {
			return importRecord{}, lineProblem(line, "invalid_format", "", "record envelope refused: %s", r.Message)
		}
		return importRecord{}, err
	}
	if tombstone {
		warnings = nil
	} else if got := env.ContentHash(); got != hash {
		return importRecord{}, lineProblem(line, "invalid_value", "/content_hash",
			"content_hash %s does not match the recomputed %s", hash, got)
	}
	sub, err = submissionOf(env)
	if err != nil {
		return importRecord{}, err
	}
	sub.ID = id
	sub.UID = strings.ToLower(uid)
	sub.ContentHash = hash
	sub.CreatedAt = *created
	sub.ProcessedAt = processedAt
	sub.RedactedAt = redactedAt
	for name, v := range processing {
		switch name {
		case "verdict":
			sub.Verdict = *v
		case "resolution":
			sub.Resolution = *v
		case "ref":
			sub.Ref = *v
		case "processed_by":
			sub.ProcessedBy = *v
		}
	}
	return importRecord{line: line, sub: sub, warnings: warnings}, nil
}
