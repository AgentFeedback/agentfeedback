package store

import (
	"context"
	"strings"
	"testing"
)

// explain returns the detail column of EXPLAIN QUERY PLAN, one line per step.
func explain(t *testing.T, q Querier, query string, args ...any) []string {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, query)
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return lines
}

// TestQueryPlans pins how SQLite (the version modernc.org/sqlite bundles, see
// go.mod) plans the queries the service runs, on an empty table and without
// ANALYZE, which is how a fresh deployment runs them. Where the schema has an
// index for the access path, the plan must use that index by name and the
// main table must not be scanned; where it has none, the scan is expected and
// recorded here so a change in either direction is a visible decision.
func TestQueryPlans(t *testing.T) {
	t.Parallel()

	db := openTest(t)
	q := db.reader

	one, two := int64(1), int64(2)
	open := false
	// statement pairs a query with its arguments so the builders' results
	// can be passed around as one value.
	type statement struct {
		sql  string
		args []any
	}
	built := func(sql string, args []any, err error) statement {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}

		return statement{sql, args}
	}
	mustList := func(f ListFilter, p Page) statement { return built(listSQL(f, p, false)) }
	mustGroups := func(f ListFilter, by ...string) statement { return built(groupsSQL(f, by)) }
	mustCount := func(f ListFilter) statement { return built(countSQL(f)) }
	mustRecurring := func(f ListFilter) statement { return built(recurringSQL(f, 10)) }
	mustSeries := func(f ListFilter, bucket string) statement { return built(seriesSQL(f, bucket)) }
	export := func(f ExportFilter) statement { s, a := iterateSQL(f); return statement{s, a} }
	lookup := func(where string, args ...any) statement {
		return statement{`SELECT ` + columns + ` FROM submissions WHERE ` + where, args}
	}

	type plan struct {
		name string
		stmt statement
		// index is the access path the first step over submissions must
		// use: an index name, "rowid" for the integer primary key, or ""
		// when the schema offers none and a table scan is the expected plan.
		index string
		// noSort asserts the rows come out in the requested order straight
		// from the access path, without a temporary b-tree.
		noSort bool
	}
	cases := []plan{}
	add := func(name string, index string, noSort bool, stmt statement) {
		cases = append(cases, plan{name, stmt, index, noSort})
	}

	// The queue and its pages.
	add("queue", "ix_submissions_open", true, mustList(ListFilter{Processed: &open}, Page{Limit: 50}))
	add("queue before_id", "ix_submissions_open", true, mustList(ListFilter{Processed: &open}, Page{Limit: 50, BeforeID: &two}))
	add("queue after_id", "ix_submissions_open", true, mustList(ListFilter{Processed: &open}, Page{Limit: 50, AfterID: &two}))
	// Identity lookups.
	add("get by id", "rowid", true, lookup(whereByID, one))
	add("get by uid", "sqlite_autoindex_submissions_1", true, lookup(whereByUID, "u"))
	add("keyed dedupe", "ux_submissions_key", true, lookup(whereByKey, "friction", "k"))
	add("keyless window", "ix_submissions_hash_created", true, lookup(whereRecentByHash, "h", one))
	// Lists by one filter and their pages.
	add("list by project", "ix_submissions_project", true, mustList(ListFilter{Project: "p"}, Page{Limit: 50}))
	add("list by project before_id", "ix_submissions_project", true, mustList(ListFilter{Project: "p"}, Page{Limit: 50, BeforeID: &two}))
	add("list by machine", "ix_submissions_machine", true, mustList(ListFilter{Machine: "m"}, Page{Limit: 50}))
	add("list by verdict", "ix_submissions_verdict", true, mustList(ListFilter{Verdict: "fixed"}, Page{Limit: 50}))
	add("list by category", "ix_submissions_category", true, mustList(ListFilter{Category: "docs"}, Page{Limit: 50}))
	add("list by fix_status", "ix_submissions_fix_status", true, mustList(ListFilter{FixStatus: "applied"}, Page{Limit: 50}))
	add("list by kind and key", "ux_submissions_key", true, mustList(ListFilter{Kind: "friction", Key: "k"}, Page{Limit: 50}))
	add("list by content_hash", "ix_submissions_hash_created", false, mustList(ListFilter{ContentHash: "h"}, Page{Limit: 50}))
	// kind and kind+since read the (kind, created_at) index, then sort by
	// id: the index orders by created_at, not id, so the page is sorted.
	add("list by kind", "ix_submissions_kind_created", false, mustList(ListFilter{Kind: "friction"}, Page{Limit: 50}))
	add("list by kind since", "ix_submissions_kind_created", false, mustList(ListFilter{Kind: "friction", Since: &one}, Page{Limit: 50}))
	add("list occurred_at range", "ix_submissions_occurred", false, mustList(ListFilter{Since: &one, Until: &two, On: OnOccurredAt}, Page{Limit: 50}))
	add("count occurred_at since", "ix_submissions_occurred", true, mustCount(ListFilter{Since: &one, On: OnOccurredAt}))
	add("count by project", "ix_submissions_project", true, mustCount(ListFilter{Project: "p"}))
	add("page before_id", "rowid", true, mustList(ListFilter{}, Page{Limit: 50, BeforeID: &two}))
	add("page after_id", "rowid", true, mustList(ListFilter{}, Page{Limit: 50, AfterID: &two}))
	// q narrows through whichever indexed filter accompanies it; alone it
	// walks the table (decision: no FTS before a query pattern exists).
	add("q with project", "ix_submissions_project", true, mustList(ListFilter{Q: "x", Project: "p"}, Page{Limit: 50}))
	add("q alone", "", true, mustList(ListFilter{Q: "x"}, Page{Limit: 50}))
	// Half-open ranges on a timestamp, newest first with a limit: the schema
	// has no created_at index, and for occurred_at the planner walks rowids
	// backwards, which serves ORDER BY id DESC LIMIT n without a sort.
	add("list created_at since", "", true, mustList(ListFilter{Since: &one}, Page{Limit: 50}))
	add("list occurred_at since", "", true, mustList(ListFilter{Since: &one, On: OnOccurredAt}, Page{Limit: 50}))
	// Stats groups: one key each. Keys with an index group in index order;
	// model, harness and schema_version have none and build a temporary
	// b-tree over the whole table.
	for _, k := range []string{"kind", "project", "category", "fix_status", "machine", "verdict"} {
		add("group by "+k, "ix_submissions_"+k, false, mustGroups(ListFilter{}, k))
	}
	for _, k := range []string{"model", "harness", "schema_version"} {
		add("group by "+k, "", false, mustGroups(ListFilter{}, k))
	}
	add("group by kind,category with project", "ix_submissions_project", false, mustGroups(ListFilter{Project: "p"}, "kind", "category"))
	add("recurring", "ix_submissions_hash_created", false, mustRecurring(ListFilter{}))
	add("recurring with project", "ix_submissions_project", false, mustRecurring(ListFilter{Project: "p"}))
	// The series groups on a date expression: always a walk of the filtered rows.
	add("series day", "", false, mustSeries(ListFilter{}, BucketDay))
	add("series week with kind", "ix_submissions_kind_created", false, mustSeries(ListFilter{Kind: "friction"}, BucketWeek))
	// Export.
	add("export after_id", "rowid", true, export(ExportFilter{AfterID: &one}))
	add("export kind since", "ix_submissions_kind_created", false, export(ExportFilter{Kind: "friction", Since: &one}))
	add("export everything", "", true, export(ExportFilter{}))

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := explain(t, q, c.stmt.sql, c.stmt.args...)
			joined := strings.Join(lines, " | ")
			t.Logf("%s", joined)

			// The first step over the main table decides the access path.
			var access string
			for _, l := range lines {
				if strings.Contains(l, " submissions") {
					access = l

					break
				}
			}
			if access == "" {
				t.Fatalf("no step over submissions in %q", joined)
			}
			switch c.index {
			case "":
				if !strings.HasPrefix(access, "SCAN submissions") || strings.Contains(access, "USING") {
					t.Fatalf("expected a plain table scan, got %q", access)
				}
			case "rowid":
				if !strings.Contains(access, "USING INTEGER PRIMARY KEY") {
					t.Fatalf("expected the integer primary key, got %q", access)
				}
			default:
				if !strings.Contains(access, "INDEX "+c.index) {
					t.Fatalf("expected index %s, got %q", c.index, access)
				}
			}
			if c.noSort && strings.Contains(joined, "TEMP B-TREE FOR ORDER BY") {
				t.Fatalf("expected the access path to deliver the order, got %q", joined)
			}
		})
	}
}
