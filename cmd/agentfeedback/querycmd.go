package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/pkg/client"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

const (
	submissionsPath = "/api/v1/submissions"
	// listMax and listMaxWithPayload are the server's list limits.
	listMax            = 500
	listMaxWithPayload = 100
	installCheckKind   = "install-check"
)

// nowFunc is the clock relative --since and --until count back from.
var nowFunc = time.Now

// apiClient builds the client from the config file and the environment; the
// read commands take no --url flag.
func apiClient(getenv func(string) string, stderr io.Writer) (*client.Client, error) {
	c, _, err := newAPIClient(getenv, "", errURLUnsetNoFlag, stderr)

	return c, err
}

// newAPIClient resolves the settings with urlFlag on top and builds the
// client; noURL is the error for a missing URL, which names --url only for
// a command that takes it.
func newAPIClient(getenv func(string) string, urlFlag string, noURL func() error, stderr io.Writer) (*client.Client, clientSettings, error) {
	path, err := configPath(getenv)
	if err != nil {
		return nil, clientSettings{}, err
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		return nil, clientSettings{}, err
	}
	settings := resolveClient(flagConfig{URL: urlFlag}, getenv, file)
	if settings.URL.Value == "" {
		return nil, settings, noURL()
	}
	if settings.APIKey.Value == "" {
		return nil, settings, errKeyUnset()
	}
	cache, err := cacheDir(getenv)
	if err != nil {
		return nil, settings, err
	}
	c, err := client.New(client.Config{
		URL:      settings.URL.Value,
		APIKey:   settings.APIKey.Value,
		CacheDir: cache,
		Now:      nowFunc,
		Stderr:   stderr,
		Version:  clientVersion().Version,
	})
	if err != nil {
		return nil, settings, errClientSetup(err)
	}

	return c, settings, nil
}

// apiErr maps a failed request to a userError. A 400's details go to stderr
// one per line; a 404 is reported by the caller when it names an id.
func apiErr(err error, stderr io.Writer) error {
	var ae *client.APIError
	var te *client.TransportError
	switch {
	case errors.Is(err, client.ErrTooLarge):
		return errTooLarge()
	case errors.As(err, &ae):
		if ae.Status >= 300 && ae.Status < 400 && ae.Message == "" {
			return errRedirected(ae.Status, ae.Location)
		}
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errKeyRefused(ae.Status)
		case http.StatusBadRequest:
			for _, d := range ae.Details {
				fmt.Fprintf(stderr, "%s\n", d)
			}

			return errBadRequest(ae.Message)
		}

		return errHTTP(ae.Status, ae.Message, ae.RequestID)
	case errors.As(err, &te):
		return errUnreachable("the server", te.Err)
	}

	return err
}

// describeErr is the message apiErr would give, without printing details;
// it is for errors reported inside another message.
func describeErr(err error) string {
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
		return errBadRequest(ae.Message).Error()
	}

	return apiErr(err, io.Discard).Error()
}

// submissionPath is the route of one submission, built from the parsed id.
func submissionPath(id int64) string {
	return submissionsPath + "/" + strconv.FormatInt(id, 10)
}

// idErr is apiErr for a request that names one submission: 404 is that
// submission missing.
func idErr(err error, id int64, stderr io.Writer) error {
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return errNotFound(id)
	}

	return apiErr(err, stderr)
}

// parseInterleaved parses flags given anywhere among the positional
// arguments; "--" ends the flags.
func parseInterleaved(fs *flag.FlagSet, args []string, stderr io.Writer) ([]string, error) {
	var pos []string
	for {
		if err := parseFlags(fs, args, stderr); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseIDs reads positive submission ids.
func parseIDs(args []string) ([]int64, error) {
	ids := make([]int64, 0, len(args))
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil || id < 1 {
			return nil, errID(a)
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// filterParams maps the plain filter flags to their query parameters.
var filterParams = []struct{ flag, param, usage string }{
	{"kind", "kind", "only this kind (lifts the default install-check exclusion)"},
	{"schema-version", "schema_version", "only this schema version"},
	{"key", "key", "only this key"},
	{"machine", "machine", "only this machine"},
	{"model", "model", "only this model"},
	{"project", "project", "only this project"},
	{"harness", "harness", "only this harness"},
	{"category", "category", "only this friction category"},
	{"fix-status", "fix_status", "only this friction fix_status"},
	{"verdict", "verdict", "only this verdict"},
	{"redacted", "redacted", "true or false: only redacted or only unredacted rows"},
	{"content-hash", "content_hash", "only this content hash"},
	{"on", "on", "created_at or occurred_at: the timestamp --since and --until apply to"},
	{"q", "q", "case-insensitive substring of the summary or a payload string"},
}

// filters holds the shared filter flags of list and stats.
type filters struct {
	fs        *flag.FlagSet
	values    map[string]*string
	exclude   multiFlag
	include   multiFlag
	open      *bool
	processed *bool
	since     *string
	until     *string
}

func addFilters(fs *flag.FlagSet) *filters {
	f := &filters{fs: fs, values: map[string]*string{}}
	for _, p := range filterParams {
		f.values[p.flag] = fs.String(p.flag, "", p.usage)
	}
	fs.Var(&f.exclude, "exclude-kind", "omit this kind (repeatable)")
	fs.Var(&f.include, "include-kind", "lift the default exclusion of this kind (repeatable); install-check is excluded unless named here or by --kind")
	f.open = fs.Bool("open", false, "only unprocessed rows")
	f.processed = fs.Bool("processed", false, "only processed rows")
	f.since = fs.String("since", "", "RFC 3339 time or <n>m, <n>h, <n>d, <n>w ago, inclusive")
	f.until = fs.String("until", "", "RFC 3339 time or <n>m, <n>h, <n>d, <n>w ago, inclusive")

	return f
}

// query returns the parameters of the flags given; the server rejects empty
// values, so a flag not given is never sent.
func (f *filters) query() (url.Values, error) {
	set := visited(f.fs)
	if *f.open && *f.processed {
		return nil, errExclusive("--open", "--processed")
	}
	q := url.Values{}
	for _, p := range filterParams {
		if set[p.flag] {
			q.Set(p.param, *f.values[p.flag])
		}
	}
	for _, k := range f.exclude {
		q.Add("exclude_kind", k)
	}
	switch {
	case *f.open:
		q.Set("processed", "false")
	case *f.processed:
		q.Set("processed", "true")
	}
	for name, v := range map[string]*string{"since": f.since, "until": f.until} {
		if set[name] {
			t, err := parseTimeFlag(name, *v)
			if err != nil {
				return nil, err
			}
			q.Set(name, t)
		}
	}
	defaultExclusion(q, set["kind"], f.include)

	return q, nil
}

// defaultExclusion adds exclude_kind=install-check unless a kind is named or
// --include-kind names install-check.
func defaultExclusion(q url.Values, kindGiven bool, include []string) {
	if kindGiven {
		return
	}
	for _, k := range include {
		if schema.Token(k) == installCheckKind {
			return
		}
	}
	q.Add("exclude_kind", installCheckKind)
}

// visited is the set of flags given on the command line.
func visited(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	return set
}

var relativeTime = regexp.MustCompile(`^([0-9]+)([mhdw])$`)

// maxRelative bounds a relative time: 100 years.
const maxRelative = 100 * 8766 * time.Hour

// parseTimeFlag accepts RFC 3339 as given or <n>m|h|d|w, turned into now
// minus n as UTC RFC 3339.
func parseTimeFlag(name, v string) (string, error) {
	if m := relativeTime.FindStringSubmatch(v); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return "", errTimeFlag(name, v)
		}
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		if n > int64(maxRelative/unit) {
			return "", errTimeFlag(name, v)
		}

		return nowFunc().UTC().Add(-time.Duration(n) * unit).Format(time.RFC3339), nil
	}
	if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
		return "", errTimeFlag(name, v)
	}

	return v, nil
}

// listPage is the part of a list response the commands read.
type listPage struct {
	Submissions  []json.RawMessage `json:"submissions"`
	Total        int64             `json:"total"`
	HasMore      bool              `json:"has_more"`
	NextBeforeID *int64            `json:"next_before_id"`
	NextAfterID  *int64            `json:"next_after_id"`
}

// nextCursor returns the cursor of the next page, nil at the end. The
// cursor must move strictly in its direction, and a page that claims more
// must not be empty.
func nextCursor(p listPage, prev *int64, after bool) (*int64, error) {
	if !p.HasMore {
		return nil, nil
	}
	if len(p.Submissions) == 0 {
		return nil, errPagination("an empty page claims more rows")
	}
	next := p.NextBeforeID
	if after {
		next = p.NextAfterID
	}
	if next == nil {
		return nil, errPagination("a page claims more rows but names no cursor")
	}
	if prev != nil && ((after && *next <= *prev) || (!after && *next >= *prev)) {
		return nil, errPagination(fmt.Sprintf("the cursor went from %d to %d", *prev, *next))
	}

	return next, nil
}

// row is a list item or record, as the human and TSV output read it.
type row struct {
	ID            int64           `json:"id"`
	UID           string          `json:"uid"`
	Kind          string          `json:"kind"`
	SchemaVersion json.Number     `json:"schema_version"`
	Key           string          `json:"key"`
	CreatedAt     string          `json:"created_at"`
	OccurredAt    string          `json:"occurred_at"`
	Machine       string          `json:"machine"`
	Model         string          `json:"model"`
	Harness       string          `json:"harness"`
	Project       string          `json:"project"`
	ProcessedAt   string          `json:"processed_at"`
	Verdict       string          `json:"verdict"`
	RedactedAt    string          `json:"redacted_at"`
	ContentHash   string          `json:"content_hash"`
	Summary       string          `json:"summary"`
	Context       json.RawMessage `json:"context"`
	Payload       json.RawMessage `json:"payload"`
}

// truncate keeps the first n characters and marks a cut with an ellipsis.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n]) + "…"
}

// clean folds runs of line breaks and tabs into one space and makes every
// other control character visible as U+FFFD.
func clean(s string) string {
	return safe(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' }), " "))
}

// safe replaces C0 controls other than line feed, DEL and C1 controls with
// U+FFFD, so nothing a submitter wrote can drive a terminal.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) {
			return utf8.RuneError
		}

		return r
	}, s)
}

func humanRow(w io.Writer, r row) {
	project := clean(r.Project)
	if project == "" {
		project = "-"
	}
	state := "open"
	switch {
	case r.ProcessedAt != "" && r.Verdict != "":
		state = clean(r.Verdict)
	case r.ProcessedAt != "":
		state = "processed"
	}
	summary := truncate(clean(r.Summary), 100)
	if r.RedactedAt != "" {
		summary = "[redacted]"
	}
	created := r.CreatedAt
	if len(created) > 16 {
		created = created[:16]
	}
	fmt.Fprintf(w, "#%d  %s  %s  %s  %s  %s\n", r.ID, clean(created), clean(r.Kind), project, state, summary)
}

var tsvEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

var tsvColumns = []string{
	"id", "uid", "kind", "schema_version", "key", "created_at", "occurred_at", "machine", "model", "harness",
	"project", "processed_at", "verdict", "redacted_at", "content_hash", "summary",
}

func tsvHeader(w io.Writer, payload bool) {
	cols := tsvColumns
	if payload {
		cols = append(cols[:len(cols):len(cols)], "payload")
	}
	fmt.Fprintln(w, strings.Join(cols, "\t"))
}

func tsvRow(w io.Writer, r row, payload bool) {
	vals := []string{
		strconv.FormatInt(r.ID, 10), r.UID, r.Kind, r.SchemaVersion.String(), r.Key, r.CreatedAt, r.OccurredAt,
		r.Machine, r.Model, r.Harness, r.Project, r.ProcessedAt, r.Verdict, r.RedactedAt, r.ContentHash, r.Summary,
	}
	if payload {
		vals = append(vals, string(r.Payload))
	}
	for i, v := range vals {
		vals[i] = tsvEscaper.Replace(v)
	}
	fmt.Fprintln(w, strings.Join(vals, "\t"))
}

// writeBody prints an API body as one line.
func writeBody(w io.Writer, body []byte) error {
	body = bytes.TrimRight(body, "\n")
	_, err := w.Write(append(body[:len(body):len(body)], '\n'))

	return err
}

// writeCompact prints an API body as one compact line.
func writeCompact(w io.Writer, body []byte) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return errBadResponse("the request", err)
	}

	return writeBody(w, buf.Bytes())
}

const listSynopsis = "list [filters] [--limit N] [--before-id N | --after-id N] [--include payload] [--all] [--json | --tsv]"

// runList prints one page of submissions, or every page with --all.
func runList(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("list")
	f := addFilters(fs)
	limit := fs.Int("limit", 0, "rows per page: 1-500, or 1-100 with --include payload")
	beforeID := fs.Int64("before-id", 0, "newest-first page of rows with a smaller id")
	afterID := fs.Int64("after-id", 0, "oldest-first page of rows with a larger id")
	include := fs.String("include", "", "payload: rows carry their payload")
	all := fs.Bool("all", false, "follow the cursor through every page")
	asJSON := fs.Bool("json", false, "print the API body (one per page with --all)")
	asTSV := fs.Bool("tsv", false, "print tab-separated values with a header")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("list", err)
	}
	if fs.NArg() != 0 {
		return errArgs("list", listSynopsis)
	}
	if *asJSON && *asTSV {
		return errExclusive("--json", "--tsv")
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	set := visited(fs)
	if set["include"] {
		q.Set("include", *include)
	}
	withPayload := *include == "payload"
	after := set["after-id"]
	var cursor *int64
	switch {
	case after:
		cursor = afterID
	case set["before-id"]:
		cursor = beforeID
	}
	if set["before-id"] && after {
		// Sent as given: the server names the conflict.
		q.Set("before_id", strconv.FormatInt(*beforeID, 10))
	}
	// A given --limit is always sent, 0 included, so the server names a bad
	// one; only --all's page size is implicit.
	switch {
	case set["limit"]:
		q.Set("limit", strconv.Itoa(*limit))
	case *all && withPayload:
		q.Set("limit", strconv.Itoa(listMaxWithPayload))
	case *all:
		q.Set("limit", strconv.Itoa(listMax))
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var shown int
	headerDone := false
	for {
		if cursor != nil {
			if after {
				q.Set("after_id", strconv.FormatInt(*cursor, 10))
			} else {
				q.Set("before_id", strconv.FormatInt(*cursor, 10))
			}
		}
		res, err := c.Do(ctx, http.MethodGet, submissionsPath, q, nil)
		if err != nil {
			return apiErr(err, stderr)
		}
		var page listPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return errBadResponse("list", err)
		}
		// The header waits for the first page, so a failed request prints
		// nothing on stdout.
		if *asTSV && !headerDone {
			tsvHeader(stdout, withPayload)
			headerDone = true
		}
		if *asJSON {
			if err := writeBody(stdout, res.Body); err != nil {
				return err
			}
		} else {
			for _, raw := range page.Submissions {
				var r row
				if err := json.Unmarshal(raw, &r); err != nil {
					return errBadResponse("list", err)
				}
				if *asTSV {
					tsvRow(stdout, r, withPayload)
				} else {
					humanRow(stdout, r)
				}
			}
		}
		shown += len(page.Submissions)
		if !*all {
			if !*asJSON && !*asTSV {
				listFooter(stdout, page, shown, after)
			}

			return nil
		}
		next, err := nextCursor(page, cursor, after)
		if err != nil {
			return err
		}
		if next == nil {
			if !*asJSON && !*asTSV {
				fmt.Fprintf(stdout, "%d of %d\n", shown, page.Total)
			}

			return nil
		}
		cursor = next
	}
}

// listFooter prints the count and, when there is more, the flag that pages on.
func listFooter(w io.Writer, page listPage, shown int, after bool) {
	fmt.Fprintf(w, "%d of %d", shown, page.Total)
	switch {
	case page.HasMore && after && page.NextAfterID != nil:
		fmt.Fprintf(w, "; next page: --after-id %d", *page.NextAfterID)
	case page.HasMore && page.NextBeforeID != nil:
		fmt.Fprintf(w, "; next page: --before-id %d", *page.NextBeforeID)
	}
	fmt.Fprintln(w)
}

// runGet prints one record.
func runGet(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("get")
	asJSON := fs.Bool("json", false, "print the API body")
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("get", err)
	}
	if len(pos) != 1 {
		return errArgs("get", "get <id> [--json]")
	}
	ids, err := parseIDs(pos)
	if err != nil {
		return err
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	res, err := c.Do(context.Background(), http.MethodGet, submissionPath(ids[0]), nil, nil)
	if err != nil {
		return idErr(err, ids[0], stderr)
	}
	if *asJSON {
		return writeBody(stdout, res.Body)
	}

	return printRecord(stdout, res.Body)
}

// printRecord prints the scalar members as name: value lines in record
// order, then each object or array member as indented JSON.
func printRecord(w io.Writer, body []byte) error {
	members, err := orderedMembers(body)
	if err != nil {
		return errBadResponse("get", err)
	}
	var nested []member
	for _, m := range members {
		switch m.raw[0] {
		case '{', '[':
			nested = append(nested, m)
		case '"':
			var s string
			_ = json.Unmarshal(m.raw, &s)
			fmt.Fprintf(w, "%s: %s\n", clean(m.name), clean(s))
		default:
			fmt.Fprintf(w, "%s: %s\n", clean(m.name), clean(string(m.raw)))
		}
	}
	for _, m := range nested {
		var buf bytes.Buffer
		if err := json.Indent(&buf, m.raw, "  ", "  "); err != nil {
			return errBadResponse("get", err)
		}
		fmt.Fprintf(w, "%s:\n  %s\n", clean(m.name), safe(buf.String()))
	}

	return nil
}

// member is one top-level member of a JSON object.
type member struct {
	name string
	raw  json.RawMessage
}

// orderedMembers returns an object's members in document order, values raw.
func orderedMembers(body []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{name, raw})
	}

	return out, nil
}

// statsBody is the part of a stats response the human output reads.
type statsBody struct {
	Total     int64 `json:"total"`
	Open      int64 `json:"open"`
	Processed int64 `json:"processed"`
	Redacted  int64 `json:"redacted"`
	Groups    []struct {
		Keys      map[string]any `json:"keys"`
		Total     int64          `json:"total"`
		Open      int64          `json:"open"`
		Processed int64          `json:"processed"`
	} `json:"groups"`
	Recurring []struct {
		ContentHash string `json:"content_hash"`
		Count       int64  `json:"count"`
		FirstID     int64  `json:"first_id"`
		LastID      int64  `json:"last_id"`
		Summary     string `json:"summary"`
		Kind        string `json:"kind"`
		Project     string `json:"project"`
	} `json:"recurring"`
	Series []struct {
		Bucket string `json:"bucket"`
		Total  int64  `json:"total"`
		Open   int64  `json:"open"`
	} `json:"series"`
}

// runStats prints the aggregates over the filtered rows.
func runStats(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("stats")
	f := addFilters(fs)
	by := fs.String("by", "", "comma list of up to three group keys, e.g. project,category")
	top := fs.Int("top", 0, "how many recurring content hashes: 0-50, server default 10")
	bucket := fs.String("bucket", "", "day or week: add a time series")
	asJSON := fs.Bool("json", false, "print the API body")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("stats", err)
	}
	if fs.NArg() != 0 {
		return errArgs("stats", "stats [filters] [--by a,b] [--top N] [--bucket day|week] [--json]")
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	set := visited(fs)
	if set["by"] {
		q.Set("by", *by)
	}
	if set["top"] {
		q.Set("top", strconv.Itoa(*top))
	}
	if set["bucket"] {
		q.Set("bucket", *bucket)
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	res, err := c.Do(context.Background(), http.MethodGet, "/api/v1/stats", q, nil)
	if err != nil {
		return apiErr(err, stderr)
	}
	if *asJSON {
		return writeBody(stdout, res.Body)
	}
	var s statsBody
	if err := json.Unmarshal(res.Body, &s); err != nil {
		return errBadResponse("stats", err)
	}
	printStats(stdout, s)

	return nil
}

func printStats(w io.Writer, s statsBody) {
	fmt.Fprintf(w, "total %d  open %d  processed %d  redacted %d\n", s.Total, s.Open, s.Processed, s.Redacted)
	if len(s.Groups) > 0 {
		fmt.Fprintln(w, "\ngroups (total, open, processed):")
		for _, g := range s.Groups {
			names := make([]string, 0, len(g.Keys))
			for k := range g.Keys {
				names = append(names, k)
			}
			sort.Strings(names)
			parts := make([]string, 0, len(names))
			for _, k := range names {
				parts = append(parts, clean(fmt.Sprintf("%s=%v", k, g.Keys[k])))
			}
			fmt.Fprintf(w, "  %6d %6d %6d  %s\n", g.Total, g.Open, g.Processed, strings.Join(parts, " "))
		}
	}
	if len(s.Recurring) > 0 {
		fmt.Fprintln(w, "\nrecurring (count, first..last id):")
		for _, r := range s.Recurring {
			project := clean(r.Project)
			if project == "" {
				project = "-"
			}
			fmt.Fprintf(w, "  ×%d  #%d..#%d  %s  %s  %s\n", r.Count, r.FirstID, r.LastID, clean(r.Kind), project, truncate(clean(r.Summary), 100))
		}
	}
	if len(s.Series) > 0 {
		fmt.Fprintln(w, "\nseries (bucket, total, open):")
		for _, b := range s.Series {
			fmt.Fprintf(w, "  %s %6d %6d\n", clean(b.Bucket), b.Total, b.Open)
		}
	}
}
