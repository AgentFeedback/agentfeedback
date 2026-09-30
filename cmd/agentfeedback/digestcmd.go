package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/pkg/client"
)

// digestPage is the first page size; a var so tests can lower it.
var digestPage = listMaxWithPayload

// digestRow is one pulled record: the raw body for the files and the fields
// digest.md reads.
type digestRow struct {
	raw         json.RawMessage
	row         row
	category    string
	fix         string
	fallback    string
	processedAt bool
}

// runDigest pulls every open submission with its payload into a directory:
// <id>.json per row, index.json, and digest.md grouped by project then
// category. The directory is the last stdout line; a pulled row that is
// already processed exits 2.
func runDigest(args []string, _ io.Reader, stdout, stderr io.Writer) (err error) {
	fs := newFlagSet("digest")
	out := fs.String("out", "", "write into this new or empty directory (default: a fresh one under the cache dir)")
	kind := fs.String("kind", "", "only this kind (lifts the default install-check exclusion)")
	var include multiFlag
	fs.Var(&include, "include-kind", "lift the default exclusion of this kind (repeatable)")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("digest", err)
	}
	if fs.NArg() != 0 {
		return errArgs("digest", "digest [--out DIR] [--kind K] [--include-kind K]")
	}
	set := visited(fs)
	q := url.Values{}
	q.Set("processed", "false")
	q.Set("include", "payload")
	if set["kind"] {
		q.Set("kind", *kind)
	}
	defaultExclusion(q, set["kind"], include)

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	dir, created, err := digestDir(*out)
	if err != nil {
		return err
	}
	// A directory digest made itself goes again when no digest.md was
	// written into it.
	written := false
	defer func() {
		if created && !written {
			_ = os.RemoveAll(dir)
		}
	}()

	var rows []digestRow
	var total int64
	var cursor *int64
	limit := digestPage
	gotTotal := false
	for {
		if cursor != nil {
			q.Set("before_id", strconv.FormatInt(*cursor, 10))
		}
		q.Set("limit", strconv.Itoa(limit))
		res, err := c.Do(context.Background(), http.MethodGet, submissionsPath, q, nil)
		if errors.Is(err, client.ErrTooLarge) && limit > 1 {
			// Payloads can be large: halve the page and ask for the same
			// cursor again.
			limit /= 2

			continue
		}
		if err != nil {
			return apiErr(err, stderr)
		}
		var page listPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return errBadResponse("list", err)
		}
		if !gotTotal {
			total, gotTotal = page.Total, true
		}
		for _, raw := range page.Submissions {
			r, err := readDigestRow(raw)
			if err != nil {
				return errBadResponse("list", err)
			}
			rows = append(rows, r)
		}
		next, err := nextCursor(page, cursor, false)
		if err != nil {
			return err
		}
		if next == nil {
			break
		}
		cursor = next
	}

	if err := writeDigest(dir, rows, total, *kind); err != nil {
		return err
	}
	written = true
	contaminated := 0
	for _, r := range rows {
		if r.processedAt {
			contaminated++
		}
	}
	fmt.Fprintf(stderr, "agentfeedback digest: pulled %d row(s) (server total %d) into %s\n", len(rows), total, dir)
	fmt.Fprintln(stdout, dir)
	if contaminated > 0 {
		return errDigestContaminated(contaminated)
	}

	return nil
}

// digestDir creates the output directory, mode 0700: --out when given (new
// or empty, never a symbolic link), else <cache>/digest/<UTC
// timestamp>-XXXXXX. created reports whether digest made it.
func digestDir(out string) (dir string, created bool, err error) {
	if out != "" {
		info, err := os.Lstat(out)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return "", false, errDigestSymlink(out)
		case errors.Is(err, os.ErrNotExist):
			created = true
		case err != nil:
			return "", false, errDigestDir(out, err)
		}
		if err := os.MkdirAll(out, 0o700); err != nil {
			return "", false, errDigestDir(out, err)
		}
		entries, err := os.ReadDir(out)
		if err != nil {
			return "", false, errDigestDir(out, err)
		}
		if len(entries) > 0 {
			return "", false, errDigestNotEmpty(out)
		}
		if err := os.Chmod(out, 0o700); err != nil {
			return "", false, errDigestDir(out, err)
		}

		return out, created, nil
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		return "", false, err
	}
	base := filepath.Join(cache, "digest")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, errDigestDir(base, err)
	}
	dir, err = os.MkdirTemp(base, nowFunc().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return "", false, errDigestDir(base, err)
	}

	return dir, true, nil
}

// readDigestRow reads the fields digest.md needs without decoding the
// payload into a map of values.
func readDigestRow(raw json.RawMessage) (digestRow, error) {
	d := digestRow{raw: raw}
	if err := json.Unmarshal(raw, &d.row); err != nil {
		return d, err
	}
	d.processedAt = d.row.ProcessedAt != ""
	if len(d.row.Payload) == 0 || d.row.Payload[0] != '{' {
		return d, nil
	}
	members, err := orderedMembers(d.row.Payload)
	if err != nil {
		return d, err
	}
	var strs []string
	for _, m := range members {
		var s string
		if json.Unmarshal(m.raw, &s) != nil {
			continue
		}
		switch m.name {
		case "category":
			d.category = s
		case "suggested_fix":
			d.fix = s
		}
		strs = append(strs, s)
	}
	d.fallback = strings.Join(strs, " · ")

	return d, nil
}

// writeDigest writes <id>.json, index.json and digest.md into dir.
func writeDigest(dir string, rows []digestRow, total int64, kind string) error {
	index := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		index = append(index, r.raw)
		if err := writeIndented(filepath.Join(dir, strconv.FormatInt(r.row.ID, 10)+".json"), r.raw); err != nil {
			return err
		}
	}
	joined, _ := json.Marshal(index)
	if err := writeIndented(filepath.Join(dir, "index.json"), joined); err != nil {
		return err
	}
	path := filepath.Join(dir, "digest.md")

	return createFile(path, renderDigest(rows, total, kind))
}

// writeIndented writes raw JSON indented; json.Indent keeps every number's
// spelling.
func writeIndented(path string, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return errBadResponse("list", err)
	}
	buf.WriteByte('\n')

	return createFile(path, buf.Bytes())
}

// createFile writes a new file, mode 0600; an existing one is never
// followed or replaced.
func createFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errDigestWrite(path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return errDigestWrite(path, err)
	}
	if err := f.Close(); err != nil {
		return errDigestWrite(path, err)
	}

	return nil
}

// renderDigest groups by project, then category, both in name order; rows
// oldest first; a content_hash pulled more than once is flagged on each row.
func renderDigest(rows []digestRow, total int64, kind string) []byte {
	repeats := map[string]int{}
	for _, r := range rows {
		repeats[r.row.ContentHash]++
	}
	groups := 0
	for _, n := range repeats {
		if n > 1 {
			groups++
		}
	}
	sorted := append([]digestRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].row.ID < sorted[j].row.ID })
	byProject := map[string][]digestRow{}
	for _, r := range sorted {
		p := clean(r.row.Project)
		if p == "" {
			p = "(no project)"
		}
		byProject[p] = append(byProject[p], r)
	}
	kind = clean(kind)
	if kind == "" {
		kind = "all"
	}

	var b strings.Builder
	b.WriteString("# agentfeedback digest\n\n")
	fmt.Fprintf(&b, "kind: %s · pulled: %d · server total: %d · exact-repeat groups: %d\n\n", kind, len(rows), total, groups)
	for _, project := range sortedKeys(byProject) {
		prows := byProject[project]
		fmt.Fprintf(&b, "## project: %s (%d)\n\n", project, len(prows))
		byCategory := map[string][]digestRow{}
		for _, r := range prows {
			cat := clean(r.category)
			if cat == "" {
				cat = "(no category)"
			}
			byCategory[cat] = append(byCategory[cat], r)
		}
		for _, cat := range sortedKeys(byCategory) {
			crows := byCategory[cat]
			fmt.Fprintf(&b, "### %s (%d)\n\n", cat, len(crows))
			for _, r := range crows {
				fmt.Fprintf(&b, "- **#%d** %s", r.row.ID, clean(dateOf(r.row.CreatedAt)))
				for _, v := range []string{r.row.Machine, r.row.Model, r.row.Harness} {
					if v != "" {
						fmt.Fprintf(&b, " `%s`", clean(v))
					}
				}
				if n := repeats[r.row.ContentHash]; n > 1 {
					fmt.Fprintf(&b, " ×%d repeats", n)
				}
				text := r.row.Summary
				if text == "" {
					text = r.fallback
				}
				fmt.Fprintf(&b, "\n  %s\n", truncate(clean(text), 220))
				if r.fix != "" {
					fmt.Fprintf(&b, "  fix: %s\n", truncate(clean(r.fix), 220))
				}
			}
			b.WriteString("\n")
		}
	}

	return []byte(b.String())
}

func sortedKeys(m map[string][]digestRow) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

func dateOf(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}

	return ts
}
