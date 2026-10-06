package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	// cloudURL is the hosted service, the target of --to cloud.
	cloudURL   = "https://api.agentfeedback.io"
	metaPath   = "/api/v1/meta"
	exportPath = "/api/v1/export"
	importPath = "/api/v1/import"
	// migratePage is the export page size, the export route's maximum.
	migratePage = 500
	// migrateChunkFallback is the contract's import limit: the chunk cap when
	// the target's meta names no limits.import_bytes, and its ceiling.
	migrateChunkFallback = 32 << 20
	// migrateTimeout bounds one request to the target: an import chunk can
	// be 32 MiB.
	migrateTimeout = 5 * time.Minute
	importHeader   = `{"export_format":2}` + "\n"
)

// migrateChunkMax, when positive, lowers the chunk cap below the target's
// import limit; a var so tests can force several chunks.
var migrateChunkMax = 0

// migrateTransport wraps the target's transport; a var so tests can record
// or answer the target's requests.
var migrateTransport = func(rt http.RoundTripper) http.RoundTripper { return rt }

// migrateOutcome is the last stdout line of a run that sent or failed.
type migrateOutcome struct {
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
	Sent      int    `json:"sent"`
	Imported  int    `json:"imported"`
	Skipped   int    `json:"skipped"`
	Conflicts int    `json:"conflicts"`
	Excluded  int    `json:"excluded"`
}

// dryRunOutcome is the last stdout line of --dry-run.
type dryRunOutcome struct {
	Outcome              string `json:"outcome"`
	Sent                 int    `json:"sent"`
	WouldSend            int    `json:"would_send"`
	Excluded             int    `json:"excluded"`
	UnredactedTombstones int    `json:"unredacted_tombstones"`
}

// migration is the state of one migrate run.
type migration struct {
	stdout, stderr io.Writer
	source, target *client.Client
	targetURL      string
	cache          string
	dryRun         bool
	includeCheck   bool
	limit          int
	chunkCap       int

	sent, imported, skipped, conflicts, excluded int
	taken                                        int
	lastID                                       int64

	chunk     []byte
	chunkUIDs []string

	kinds   []string
	perKind map[string]int
	first   map[string]string
	redact  []string
}

// runMigrate copies the configured server's records to another server
// through its import route, and ends stdout with a JSON outcome line, on
// failure too.
func runMigrate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	m := &migration{stdout: stdout, stderr: stderr, perKind: map[string]int{}, first: map[string]string{}}
	err := m.run(args, stdin)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	if werr := writeJSON(stdout, migrateOutcome{
		Outcome: "error", Message: err.Error(), Sent: m.sent, Imported: m.imported, Skipped: m.skipped,
		Conflicts: m.conflicts, Excluded: m.excluded,
	}); werr != nil {
		return werr
	}

	return err
}

// run parses the command line, checks the target and pages the source.
func (m *migration) run(args []string, stdin io.Reader) error {
	fs := newFlagSet("migrate")
	mf := addModeFlags(fs)
	to := fs.String("to", "", "cloud or the target server's base URL")
	keyFromStdin := fs.Bool("to-key-from-stdin", false, "read the target's API key from stdin (required)")
	kind := fs.String("kind", "", "only this kind (lifts the default install-check exclusion)")
	var include multiFlag
	fs.Var(&include, "include-kind", "lift the default exclusion of this kind (repeatable); install-check is excluded unless named here or by --kind")
	since := fs.String("since", "", "RFC 3339 time or <n>m, <n>h, <n>d, <n>w ago, inclusive, on created_at")
	limit := fs.Int("limit", 0, "send at most this many records in total")
	dryRun := fs.Bool("dry-run", false, "send nothing: count what would be sent and check the target's tombstones")
	if err := parseFlags(fs, args, m.stderr); err != nil {
		return errFlags("migrate", err)
	}
	if fs.NArg() != 0 {
		return errArgs("migrate", "migrate --to cloud|URL --to-key-from-stdin [--kind K] [--include-kind K]... [--since T] [--limit N] [--dry-run] [--local | --server URL]")
	}
	set := visited(fs)
	if *to == "" {
		return errMigrateNeedsTo()
	}
	if !*keyFromStdin {
		return errMigrateNeedsStdin()
	}
	if set["limit"] && *limit < 1 {
		return errMigrateLimit(*limit)
	}
	q := url.Values{}
	if set["kind"] {
		q.Set("kind", *kind)
	}
	if set["since"] {
		t, err := parseTimeFlag("since", *since)
		if err != nil {
			return err
		}
		q.Set("since", t)
	}
	exclusion := url.Values{}
	defaultExclusion(exclusion, set["kind"], include)
	m.includeCheck = exclusion.Get("exclude_kind") == ""
	m.limit = *limit
	m.dryRun = *dryRun

	target := *to
	if target == "cloud" {
		target = cloudURL
	} else if err := checkBaseURL(target, errMigrateBadTo); err != nil {
		return err
	}
	m.targetURL = strings.TrimRight(target, "/")

	source, mode, err := newAPIClient(os.Getenv, *mf, m.stderr)
	if err != nil {
		return err
	}
	m.source = source
	// A local source has no URL, so it cannot be the target.
	if mode.Mode == modeRemote && strings.TrimRight(strings.TrimSpace(mode.Settings.URL.Value), "/") == m.targetURL {
		return errMigrateSameServer(m.targetURL)
	}
	key, err := readKey(stdin, "migrate --to-key-from-stdin")
	if err != nil {
		return err
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		return err
	}
	data, err := dataDir(os.Getenv)
	if err != nil {
		return err
	}
	m.cache = cache
	m.target, err = client.New(client.Config{
		URL:      m.targetURL,
		APIKey:   key,
		DataDir:  data,
		CacheDir: cache,
		HTTP:     migrateHTTP(),
		Now:      nowFunc,
		Stderr:   m.stderr,
		Version:  clientVersion().Version,
	})
	if err != nil {
		return errMigrateTarget(m.targetURL, "the client cannot be set up for it")
	}

	ctx := context.Background()
	if err := m.checkTarget(ctx); err != nil {
		return err
	}
	if err := m.pages(ctx, q); err != nil {
		return err
	}
	if m.dryRun {
		return m.printDryRun()
	}
	if err := m.flush(ctx); err != nil {
		return err
	}

	return writeJSON(m.stdout, migrateOutcome{
		Outcome: "migrated", Sent: m.sent, Imported: m.imported, Skipped: m.skipped,
		Conflicts: m.conflicts, Excluded: m.excluded,
	})
}

// migrateHTTP is the target's HTTP client: the default 2 s dial and the
// proxy environment, with a 5 min overall timeout for a large chunk.
func migrateHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext

	return &http.Client{Transport: migrateTransport(t), Timeout: migrateTimeout}
}

// checkTarget reads the target's meta: it must list the import feature, and
// its limits.import_bytes, never above the contract's 32 MiB, caps a chunk.
func (m *migration) checkTarget(ctx context.Context) error {
	res, err := m.target.Do(ctx, http.MethodGet, metaPath, nil, nil)
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return errMigrateTooOld(m.targetURL)
		}

		return m.targetErr(err)
	}
	var meta struct {
		Features []string `json:"features"`
		Limits   struct {
			ImportBytes *int `json:"import_bytes"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(res.Body, &meta); err != nil {
		return errMigrateTarget(m.targetURL, "its answer to GET "+metaPath+" is not AgentFeedback metadata")
	}
	if !slices.Contains(meta.Features, "import") {
		return errMigrateTooOld(m.targetURL)
	}
	m.chunkCap = migrateChunkFallback
	if meta.Limits.ImportBytes != nil && *meta.Limits.ImportBytes > 0 {
		m.chunkCap = min(*meta.Limits.ImportBytes, migrateChunkFallback)
	}
	if migrateChunkMax > 0 && migrateChunkMax < m.chunkCap {
		m.chunkCap = migrateChunkMax
	}

	return nil
}

// targetErr maps a failed request to the target to a userError that names
// the target and, for 401 and 403, the key read from stdin.
func (m *migration) targetErr(err error) error {
	var ae *client.APIError
	var te *client.TransportError
	switch {
	case errors.Is(err, client.ErrTooLarge):
		return errMigrateTarget(m.targetURL, "its answer is larger than 48 MiB")
	case errors.As(err, &ae):
		if ae.Status >= 300 && ae.Status < 400 && ae.Message == "" {
			return errMigrateTarget(m.targetURL, fmt.Sprintf("it redirected (HTTP %d) to %s", ae.Status, clean(ae.Location)))
		}
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errMigrateTargetKey(m.targetURL, ae.Status)
		case http.StatusBadRequest:
			for _, d := range ae.Details {
				fmt.Fprintf(m.stderr, "%s\n", clean(string(d)))
			}
		}

		return errMigrateTarget(m.targetURL, ae.Error())
	case errors.As(err, &te):
		return errUnreachable(m.targetURL, te.Err)
	}

	return err
}

// stopped is the error of a run that failed part way, with its counts.
func (m *migration) stopped(err error) error {
	reason := err.Error()
	var ue *userError
	if errors.As(err, &ue) {
		reason = ue.problem
	}

	return errMigrateStopped(reason, m.sent, m.imported, m.skipped)
}

// pages exports the source page by page, after the last id seen, until a
// page comes back short or the limit is reached.
func (m *migration) pages(ctx context.Context, q url.Values) error {
	for {
		page := migratePage
		if m.limit > 0 {
			page = min(page, m.limit-m.taken)
		}
		q.Set("after_id", strconv.FormatInt(m.lastID, 10))
		q.Set("limit", strconv.Itoa(page))
		n, err := m.page(ctx, q)
		if err != nil {
			return err
		}
		if n < page || (m.limit > 0 && m.taken >= m.limit) {
			return nil
		}
	}
}

// page copies one export page into a file in the cache directory, verifying
// its trailer as it is written, then closes the source and replays the
// record lines; nothing of a page that fails verification is sent, and no
// request to the target holds the source stream open. It returns how many
// records the page held.
func (m *migration) page(ctx context.Context, q url.Values) (int, error) {
	if err := os.MkdirAll(m.cache, 0o700); err != nil {
		return 0, m.stopped(fmt.Errorf("cannot create the cache directory %s: %v", m.cache, err))
	}
	f, err := os.CreateTemp(m.cache, "migrate-*.ndjson")
	if err != nil {
		return 0, m.stopped(fmt.Errorf("cannot create a page file in %s: %v", m.cache, err))
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	body, _, err := m.source.Stream(ctx, exportPath, q)
	if err != nil {
		return 0, m.stopped(apiErr(err, m.stderr))
	}
	n := 0
	prev := m.lastID
	var recErr error // a record's own failure, returned as it is
	record := func(line []byte) error {
		var rec struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(line, &rec) != nil {
			recErr = m.stopped(errors.New("the source export holds a record line that is not readable"))

			return recErr
		}
		if recErr = m.advance(prev, rec.ID); recErr != nil {
			return recErr
		}
		prev = rec.ID
		n++
		if _, err := f.Write(line); err != nil {
			recErr = m.stopped(fmt.Errorf("cannot write the page file %s: %v", f.Name(), err))

			return recErr
		}

		return nil
	}
	err = walkExport(body, nil, record)
	_ = body.Close()
	if err != nil {
		if recErr != nil {
			return 0, recErr
		}

		return 0, m.stopped(err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, m.stopped(fmt.Errorf("cannot read the page file %s: %v", f.Name(), err))
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	for {
		line, err = readLine(br, line[:0])
		if len(line) > 0 {
			if rerr := m.record(ctx, line); rerr != nil {
				return 0, rerr
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, m.stopped(fmt.Errorf("cannot read the page file %s: %v", f.Name(), err))
		}
	}
}

// advance refuses a record id that does not follow prev: the export is in
// ascending id order, and paging after the last id relies on it.
func (m *migration) advance(prev, id int64) error {
	if id <= prev {
		return m.stopped(fmt.Errorf("the source export's ids did not advance (from %d to %d)", prev, id))
	}

	return nil
}

// exportRecord is the part of a record line migrate reads.
type exportRecord struct {
	ID          int64           `json:"id"`
	UID         string          `json:"uid"`
	Kind        string          `json:"kind"`
	ContentHash string          `json:"content_hash"`
	RedactedAt  json.RawMessage `json:"redacted_at"`
}

// record handles one record line: install-check rows are excluded unless
// asked for, the rest are counted (dry run) or added to the chunk.
func (m *migration) record(ctx context.Context, line []byte) error {
	var rec exportRecord
	if err := json.Unmarshal(line, &rec); err != nil || rec.UID == "" {
		return m.stopped(errors.New("the source export holds a record line that is not readable"))
	}
	if err := m.advance(m.lastID, rec.ID); err != nil {
		return err
	}
	m.lastID = rec.ID
	if rec.Kind == installCheckKind && !m.includeCheck {
		m.excluded++

		return nil
	}
	m.taken++
	if m.dryRun {
		return m.tally(ctx, rec, line)
	}

	return m.add(ctx, rec.UID, line)
}

// trailerLen is the length of a chunk trailer, newline included, for count
// records.
func trailerLen(count int) int {
	return len(chunkTrailer(count, strings.Repeat("0", sha256.Size*2)))
}

// chunkTrailer is the trailer line of a chunk.
func chunkTrailer(count int, sum string) string {
	return fmt.Sprintf(`{"export_complete":true,"count":%d,"sha256":"%s"}`+"\n", count, sum)
}

// add appends a record line, bytes unchanged, to the chunk, sending the
// chunk first when the line would take it over the cap.
func (m *migration) add(ctx context.Context, uid string, line []byte) error {
	if len(importHeader)+len(line)+trailerLen(1) > m.chunkCap {
		return errMigrateRecordTooLarge(uid, m.chunkCap)
	}
	if len(importHeader)+len(m.chunk)+len(line)+trailerLen(len(m.chunkUIDs)+1) > m.chunkCap {
		if err := m.flush(ctx); err != nil {
			return err
		}
	}
	m.chunk = append(m.chunk, line...)
	m.chunkUIDs = append(m.chunkUIDs, uid)

	return nil
}

// importAnswer is the part of the import response migrate reads.
type importAnswer struct {
	Imported  *int `json:"imported"`
	Skipped   *int `json:"skipped"`
	Conflicts []struct {
		UID        string `json:"uid"`
		ExistingID int64  `json:"existing_id"`
		Reason     string `json:"reason"`
	} `json:"conflicts"`
	Warnings []struct {
		Line    int    `json:"line"`
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
		Message string `json:"message"`
	} `json:"warnings"`
}

// flush posts the chunk to the target's import route, prints its conflicts
// on stdout and its warnings on stderr, and empties it.
func (m *migration) flush(ctx context.Context) error {
	if len(m.chunkUIDs) == 0 {
		return nil
	}
	sum := sha256.Sum256(m.chunk)
	body := make([]byte, 0, len(importHeader)+len(m.chunk)+trailerLen(len(m.chunkUIDs)))
	body = append(body, importHeader...)
	body = append(body, m.chunk...)
	body = append(body, chunkTrailer(len(m.chunkUIDs), hex.EncodeToString(sum[:]))...)
	res, err := m.target.Send(ctx, http.MethodPost, importPath, nil, "application/x-ndjson", body)
	if err != nil {
		return m.stopped(m.targetErr(err))
	}
	m.sent += len(m.chunkUIDs)
	var ans importAnswer
	if err := json.Unmarshal(res.Body, &ans); err != nil || ans.Imported == nil || ans.Skipped == nil {
		return m.stopped(errMigrateTarget(m.targetURL, "its answer to POST "+importPath+" is not an import result"))
	}
	if *ans.Imported < 0 || *ans.Skipped < 0 || *ans.Imported+*ans.Skipped != len(m.chunkUIDs) {
		return m.stopped(errMigrateTarget(m.targetURL, fmt.Sprintf("its answer to POST %s counts %d imported and %d skipped for %d records",
			importPath, *ans.Imported, *ans.Skipped, len(m.chunkUIDs))))
	}
	m.imported += *ans.Imported
	m.skipped += *ans.Skipped
	for _, c := range ans.Conflicts {
		m.conflicts++
		fmt.Fprintf(m.stdout, "conflict uid=%s existing_id=%d reason=%s\n", clean(c.UID), c.ExistingID, clean(c.Reason))
	}
	for _, w := range ans.Warnings {
		uid := "?"
		if i := w.Line - 2; i >= 0 && i < len(m.chunkUIDs) { // line 1 is the header
			uid = m.chunkUIDs[i]
		}
		fmt.Fprintf(m.stderr, "warning uid=%s %s %s: %s\n", clean(uid), clean(w.Code), clean(w.Pointer), clean(w.Message))
	}
	m.chunk = m.chunk[:0]
	m.chunkUIDs = m.chunkUIDs[:0]

	return nil
}

// tally counts a record for --dry-run, keeps the first line of each kind,
// and asks the target whether it holds a tombstone's uid unredacted.
func (m *migration) tally(ctx context.Context, rec exportRecord, line []byte) error {
	if _, ok := m.perKind[rec.Kind]; !ok {
		m.kinds = append(m.kinds, rec.Kind)
		m.first[rec.Kind] = strings.TrimSuffix(string(line), "\n")
	}
	m.perKind[rec.Kind]++
	if len(rec.RedactedAt) == 0 || string(rec.RedactedAt) == "null" {
		return nil
	}
	q := url.Values{"content_hash": {rec.ContentHash}, "redacted": {"false"}, "limit": {"100"}}
	var cursor *int64
	for {
		if cursor != nil {
			q.Set("before_id", strconv.FormatInt(*cursor, 10))
		}
		res, err := m.target.Do(ctx, http.MethodGet, submissionsPath, q, nil)
		if err != nil {
			return m.stopped(m.targetErr(err))
		}
		var page listPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return m.stopped(errMigrateTarget(m.targetURL, "its answer to GET "+submissionsPath+" is not a list"))
		}
		for _, raw := range page.Submissions {
			var row struct {
				ID  int64  `json:"id"`
				UID string `json:"uid"`
			}
			if json.Unmarshal(raw, &row) == nil && row.UID == rec.UID {
				m.redact = append(m.redact, fmt.Sprintf("redact on target: uid=%s id=%d", clean(row.UID), row.ID))
			}
		}
		next, err := nextCursor(page, cursor, false)
		if err != nil {
			return m.stopped(err)
		}
		if next == nil {
			return nil
		}
		cursor = next
	}
}

// printDryRun prints the per-kind counts and first records, the exclusion,
// the tombstones to redact on the target and the outcome line.
func (m *migration) printDryRun() error {
	for _, k := range m.kinds {
		fmt.Fprintf(m.stdout, "kind %s: %d records\n", clean(k), m.perKind[k])
		fmt.Fprintf(m.stdout, "first %s: %s\n", clean(k), m.first[k])
	}
	if m.excluded > 0 {
		fmt.Fprintf(m.stdout, "excluded %s: %d\n", installCheckKind, m.excluded)
	}
	for _, line := range m.redact {
		fmt.Fprintln(m.stdout, line)
	}

	return writeJSON(m.stdout, dryRunOutcome{
		Outcome: "dry_run", WouldSend: m.taken, Excluded: m.excluded, UnredactedTombstones: len(m.redact),
	})
}
