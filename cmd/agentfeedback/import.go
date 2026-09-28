package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"os"
	"sort"
	"strings"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// importRecord is one line of an export stream. Every nullable field is a
// pointer so "absent" and "null" both read as unset.
type importRecord struct {
	ID               int64           `json:"id"`
	Family           string          `json:"family"`
	SubmissionType   string          `json:"submission_type"`
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	RunID            *string         `json:"run_id"`
	Payload          json.RawMessage `json:"payload"`
	PayloadHash      *string         `json:"payload_hash"`
	CreatedAt        string          `json:"created_at"`
	ProcessedAt      *string         `json:"processed_at"`
	Resolution       *string         `json:"resolution"`
}

// importHeader is the first line. Unknown keys (a producer's "source", for
// example) are ignored on purpose: the format is allowed to grow.
type importHeader struct {
	ExportFormat int     `json:"export_format"`
	Family       *string `json:"family"`
	Since        *string `json:"since"`
}

// importTerminator is the last line. count and sha256 are pointers because
// their absence is a hard error: a stream that declares neither cannot be
// verified, and an unverifiable stream must not be imported.
type importTerminator struct {
	ExportComplete bool    `json:"export_complete"`
	Count          *int64  `json:"count"`
	SHA256         *string `json:"sha256"`
}

// familyFilter collects --family values (repeatable, or comma-separated).
type familyFilter struct {
	families map[string]bool
}

func (f *familyFilter) String() string {
	if f == nil || len(f.families) == 0 {
		return ""
	}
	out := make([]string, 0, len(f.families))
	for name := range f.families {
		out = append(out, name)
	}
	sort.Strings(out)

	return strings.Join(out, ",")
}

func (f *familyFilter) Set(v string) error {
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch name {
		case store.FamilyFriction, store.FamilyReview, store.FamilyEvent:
		default:
			return fmt.Errorf("unknown family %q: expected friction, review or event", name)
		}
		if f.families == nil {
			f.families = map[string]bool{}
		}
		f.families[name] = true
	}

	return nil
}

// selected reports whether records of this family are imported. An empty
// filter imports everything.
func (f *familyFilter) selected(family string) bool {
	if f == nil || len(f.families) == 0 {
		return true
	}

	return f.families[family]
}

type importOptions struct {
	allowNonEmpty  bool
	allowPartial   bool
	trustHashes    bool
	reserveThrough int64
	families       *familyFilter
	dbPath         string
}

type importStats struct {
	imported int64
	skipped  int64
	// maxID is the highest id actually imported; maxSeen is the highest id in
	// the stream, skipped rows included. The sequence follows maxSeen so the
	// ids of rows a --family filter left behind (archived elsewhere) can never
	// be handed out again.
	maxID    int64
	maxSeen  int64
	sequence int64
	byFamily map[string]int64
}

// runImport restores an export stream into the database. Everything happens in
// one transaction opened before the first line is parsed: records are inserted
// as they are read (the stream is never materialized), and any failure —
// including the final count and digest verification — rolls the transaction
// back, so a rejected stream leaves the database untouched.
func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	allowNonEmpty := fs.Bool("allow-nonempty", false, "import into a database that already holds rows")
	allowPartial := fs.Bool("allow-partial", false, "import a filtered (partial) export")
	trustHashes := fs.Bool("trust-hashes", false, "keep each record's declared payload_hash instead of failing when it disagrees with the recomputed one")
	reserveThrough := fs.Int64("reserve-ids-through", 0, "advance the id sequence at least this far after importing (it always passes the highest id in the stream)")
	families := &familyFilter{}
	fs.Var(families, "family", "import only these families (friction, review, event; repeatable or comma-separated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentfeedback import [--allow-nonempty] [--allow-partial] [--trust-hashes] " +
			"[--family friction|review|event] [--reserve-ids-through N] <file.jsonl>")
	}
	path := fs.Arg(0)

	cfg, err := loadConfig(false)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	opts := importOptions{
		allowNonEmpty:  *allowNonEmpty,
		allowPartial:   *allowPartial,
		trustHashes:    *trustHashes,
		reserveThrough: *reserveThrough,
		families:       families,
		dbPath:         cfg.DatabasePath,
	}
	stats := importStats{byFamily: map[string]int64{}}

	if err := db.Write(ctx, func(q store.Querier) error {
		return importStream(ctx, q, f, path, opts, &stats)
	}); err != nil {
		return err
	}

	names := make([]string, 0, len(stats.byFamily))
	for name := range stats.byFamily {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%q:%d", name, stats.byFamily[name]))
	}

	fmt.Printf("{\"imported\":%d,\"skipped\":%d,\"max_id\":%d,\"sequence\":%d,\"by_family\":{%s}}\n",
		stats.imported, stats.skipped, stats.maxID, stats.sequence, strings.Join(parts, ","))

	return nil
}

// lineKind classifies a stream line by its top-level keys. Substring matching
// would misread a record whose payload merely mentions export_complete or
// export_format, so every line is decoded first.
type lineKind int

const (
	kindRecord lineKind = iota
	kindHeader
	kindTerminator
)

func classifyLine(line []byte, lineNo int) (lineKind, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		return kindRecord, fmt.Errorf("line %d: not a valid JSON object: %w", lineNo, err)
	}
	if _, ok := top["export_complete"]; ok {
		return kindTerminator, nil
	}
	if _, ok := top["export_format"]; ok {
		return kindHeader, nil
	}

	return kindRecord, nil
}

// importStream parses the stream and inserts as it goes, inside the caller's
// write transaction. Verification that can only happen at the end (terminator,
// count, digest) still returns an error, which rolls everything back.
func importStream(
	ctx context.Context,
	q store.Querier,
	r *os.File,
	path string,
	opts importOptions,
	stats *importStats,
) error {
	existing, err := store.CountAll(ctx, q)
	if err != nil {
		return err
	}
	if existing > 0 && !opts.allowNonEmpty {
		return fmt.Errorf("database %s already holds %d row(s); pass --allow-nonempty to import anyway",
			opts.dbPath, existing)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	var (
		haveHeader bool
		haveTerm   bool
		terminator importTerminator
		digest     hash.Hash = sha256.New()
		records    int64
		lineNo     int
	)

	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if haveTerm {
			return fmt.Errorf("line %d: content after the export terminator", lineNo)
		}

		kind, err := classifyLine(line, lineNo)
		if err != nil {
			return err
		}

		if !haveHeader {
			if kind != kindHeader {
				return fmt.Errorf("line %d: expected the export header (export_format 1) as the first line", lineNo)
			}
			// The header comes first, so both refusals below happen before a
			// single row is inserted.
			var header importHeader
			if err := json.Unmarshal(line, &header); err != nil {
				return fmt.Errorf("line %d: header is not valid JSON: %w", lineNo, err)
			}
			if header.ExportFormat != 1 {
				return fmt.Errorf("line %d: unsupported export_format %d, expected 1", lineNo, header.ExportFormat)
			}
			if !opts.allowPartial && (header.Family != nil || header.Since != nil) {
				return errors.New("refusing a filtered export: it restores a subset of the data; pass --allow-partial to import it anyway")
			}
			haveHeader = true

			continue
		}

		if kind == kindTerminator {
			if err := json.Unmarshal(line, &terminator); err != nil {
				return fmt.Errorf("line %d: terminator is not valid JSON: %w", lineNo, err)
			}
			haveTerm = true

			continue
		}

		rec, err := parseImportRecord(line, lineNo, opts.trustHashes)
		if err != nil {
			return err
		}
		// The digest covers every record line in the stream, including the
		// ones a --family filter leaves out: the verification is about the
		// stream's integrity, not about what was imported.
		digest.Write(append(append([]byte{}, line...), '\n'))
		records++
		if rec.ID > stats.maxSeen {
			stats.maxSeen = rec.ID
		}

		if !opts.families.selected(rec.Family) {
			stats.skipped++

			continue
		}
		if err := store.InsertSubmissionWithID(ctx, q, rec); err != nil {
			return err
		}
		stats.imported++
		stats.byFamily[rec.Family]++
		if rec.ID > stats.maxID {
			stats.maxID = rec.ID
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if !haveHeader {
		return errors.New("missing export header line (export_format 1)")
	}
	if !haveTerm || !terminator.ExportComplete {
		return errors.New("missing export terminator: the stream is truncated and must not be imported")
	}
	if terminator.Count == nil {
		return errors.New("export terminator has no count; refusing to import an unverifiable stream")
	}
	if *terminator.Count != records {
		return fmt.Errorf("export declares %d record(s) but the stream holds %d", *terminator.Count, records)
	}
	if terminator.SHA256 == nil || strings.TrimSpace(*terminator.SHA256) == "" {
		return errors.New("export terminator has no sha256; refusing to import an unverifiable stream")
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != *terminator.SHA256 {
		return fmt.Errorf("export digest mismatch: declared %s, computed %s", *terminator.SHA256, got)
	}

	stats.sequence = opts.reserveThrough
	if stats.maxSeen > stats.sequence {
		stats.sequence = stats.maxSeen
	}
	if stats.sequence > 0 {
		// ids are never reused: push the AUTOINCREMENT counter past every id
		// the stream carried — imported or skipped — and past any archived
		// range the operator reserved on top of that.
		return store.SetSequence(ctx, q, stats.sequence)
	}

	return nil
}

func parseImportRecord(line []byte, lineNo int, trustHashes bool) (store.Submission, error) {
	var rec importRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&rec); err != nil {
		return store.Submission{}, fmt.Errorf("line %d: not valid JSON: %w", lineNo, err)
	}

	if rec.ID <= 0 {
		return store.Submission{}, fmt.Errorf("line %d: id must be a positive integer", lineNo)
	}
	// Identifiers are trimmed exactly as the create path trims them, BEFORE
	// validation, storage and hash recomputation: an imported row padded with
	// whitespace would otherwise never match the (family, submission_type,
	// run_id) key a later submission builds, and its recomputed hash would
	// differ from that submission's. A 1.x export's declared hashes were
	// already computed over trimmed values (1.x trimmed on create), so
	// recomputing from the trimmed values here agrees with them.
	rec.SubmissionType = strings.TrimSpace(rec.SubmissionType)
	rec.MachineName = strings.TrimSpace(rec.MachineName)
	rec.CoordinatorModel = strings.TrimSpace(rec.CoordinatorModel)
	if rec.RunID != nil {
		trimmed := strings.TrimSpace(*rec.RunID)
		rec.RunID = &trimmed
	}
	switch rec.Family {
	case store.FamilyFriction, store.FamilyReview, store.FamilyEvent:
	default:
		return store.Submission{}, fmt.Errorf("line %d: family must be friction, review or event, got %q", lineNo, rec.Family)
	}
	for field, v := range map[string]string{
		"submission_type":   rec.SubmissionType,
		"machine_name":      rec.MachineName,
		"coordinator_model": rec.CoordinatorModel,
		"created_at":        rec.CreatedAt,
	} {
		if strings.TrimSpace(v) == "" {
			return store.Submission{}, fmt.Errorf("line %d: %s is required", lineNo, field)
		}
	}
	// Reviews and events dedupe on their idempotency key; a keyed row without
	// one would be invisible to replay detection forever.
	if rec.Family == store.FamilyReview || rec.Family == store.FamilyEvent {
		if rec.RunID == nil || strings.TrimSpace(*rec.RunID) == "" {
			return store.Submission{}, fmt.Errorf("line %d: run_id is required for %s records", lineNo, rec.Family)
		}
	}
	if len(bytes.TrimSpace(rec.Payload)) == 0 || bytes.TrimSpace(rec.Payload)[0] != '{' {
		return store.Submission{}, fmt.Errorf("line %d: payload must be a JSON object", lineNo)
	}

	var payload bytes.Buffer
	if err := json.Compact(&payload, rec.Payload); err != nil {
		return store.Submission{}, fmt.Errorf("line %d: payload is not valid JSON: %w", lineNo, err)
	}

	createdAt, err := core.ParseMicros(rec.CreatedAt)
	if err != nil {
		return store.Submission{}, fmt.Errorf("line %d: created_at: %w", lineNo, err)
	}

	sub := store.Submission{
		ID:               rec.ID,
		Family:           rec.Family,
		SubmissionType:   rec.SubmissionType,
		MachineName:      rec.MachineName,
		CoordinatorModel: rec.CoordinatorModel,
		Payload:          payload.Bytes(),
		CreatedAt:        createdAt,
		Resolution:       trimmedPtr(rec.Resolution),
	}
	if rec.RunID != nil && *rec.RunID != "" {
		v := *rec.RunID
		sub.RunID = &v
	}
	if rec.ProcessedAt != nil && *rec.ProcessedAt != "" {
		us, err := core.ParseMicros(*rec.ProcessedAt)
		if err != nil {
			return store.Submission{}, fmt.Errorf("line %d: processed_at: %w", lineNo, err)
		}
		sub.ProcessedAt = &us
	}

	declared := ""
	if rec.PayloadHash != nil {
		declared = strings.TrimSpace(*rec.PayloadHash)
	}
	if declared != "" && trustHashes {
		// The operator vouched for the stream's hashes: store them verbatim
		// without recomputing.
		sub.PayloadHash = declared

		return sub, nil
	}

	// Every record's hash is recomputed from its own content: rows exported
	// before hashing existed get the hash their family's rule produces (so they
	// dedupe against new submissions), and rows that declare one must agree
	// with it — a disagreement means the row would never dedupe as intended.
	recomputed, err := core.HashForFamily(rec.Family, rec.MachineName, rec.CoordinatorModel, payload.Bytes())
	if err != nil {
		return store.Submission{}, fmt.Errorf("line %d: recompute payload_hash: %w", lineNo, err)
	}
	if declared != "" && declared != recomputed {
		return store.Submission{}, fmt.Errorf(
			"line %d: payload_hash %s does not match the hash recomputed from the record's content (%s); "+
				"pass --trust-hashes to keep the declared hash",
			lineNo, declared, recomputed)
	}
	sub.PayloadHash = recomputed

	return sub, nil
}

func trimmedPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}

	return &v
}
