package core

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// TestStatsAgreeWithList seeds a varied dataset and checks, for many random
// filters, that the stats and the list count the same rows.
func TestStatsAgreeWithList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kinds := []string{"friction", "review", "note", ""}
	projects := []string{"alpha", "beta", "gamma", ""}
	categories := []string{"tooling", "documentation", "config"}
	harnesses := []string{"claude-code", "opencode", ""}
	verdicts := []string{"fixed", "invalid", "duplicate"}
	words := []string{"linter", "flag", "readme", "crash"}

	for _, seed := range []uint64{1, 42, 20260928} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, seed))
			pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
			s, _, clock := newTestService(t)
			start := clock.now()
			const rows = 300
			for i := range rows {
				clock.advance(time.Duration(rng.IntN(6)) * time.Hour)
				body := fmt.Sprintf(`{"summary":"%s %d","machine":"m%d"`, pick(words), i, rng.IntN(3))
				if k := pick(kinds); k != "" {
					body += fmt.Sprintf(`,"kind":%q`, k)
				}
				if p := pick(projects); p != "" {
					body += fmt.Sprintf(`,"project":%q`, p)
				}
				if h := pick(harnesses); h != "" {
					body += fmt.Sprintf(`,"harness":%q`, h)
				}
				if rng.IntN(2) == 0 {
					body += fmt.Sprintf(`,"occurred_at":%q`, start.Add(time.Duration(rng.IntN(1000))*time.Hour).Format(time.RFC3339))
				}
				body += fmt.Sprintf(`,"payload":{"category":%q,"fix_status":"none","details":"%s"}}`, pick(categories), pick(words))
				res := mustCreate(t, s, body)
				switch rng.IntN(4) {
				case 0:
					if _, err := s.Mark(ctx, res.Record.ID, []byte(fmt.Sprintf(`{"verdict":%q}`, pick(verdicts)))); err != nil {
						t.Fatal(err)
					}
				case 1:
					if _, err := s.Redact(ctx, res.Record.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			end := clock.now()

			nonEmpty := 0
			for range 150 {
				var f Filter
				if rng.IntN(3) == 0 {
					f.Kind = pick(kinds[:3])
				}
				if rng.IntN(3) == 0 {
					f.Project = pick(projects[:3])
				}
				if rng.IntN(4) == 0 {
					f.Category = pick(categories)
				}
				if rng.IntN(4) == 0 {
					f.Harness = pick(harnesses[:2])
				}
				if rng.IntN(5) == 0 {
					f.Verdict = pick(verdicts)
				}
				if rng.IntN(5) == 0 {
					f.ExcludeKind = []string{pick(kinds[:3])}
				}
				if rng.IntN(3) == 0 {
					f.Processed = ptr(rng.IntN(2) == 0)
				}
				if rng.IntN(3) == 0 {
					f.Redacted = ptr(rng.IntN(2) == 0)
				}
				if rng.IntN(4) == 0 {
					f.Q = pick(words)
				}
				if rng.IntN(3) == 0 {
					f.On = pick([]string{"created_at", "occurred_at"})
					a := start.Add(time.Duration(rng.Int64N(int64(end.Sub(start)) + 1)))
					b := a.Add(time.Duration(rng.IntN(500)) * time.Hour)
					f.Since, f.Until = &a, &b
				}
				stats, err := s.Stats(ctx, StatsParams{Filter: f, By: []string{"kind", "project"}})
				if err != nil {
					t.Fatal(err)
				}
				list, err := s.List(ctx, ListParams{Filter: f, Limit: ptr(1)})
				if err != nil {
					t.Fatal(err)
				}
				if stats.Total != list.Total {
					t.Fatalf("filter %+v: stats total %d, list total %d", f, stats.Total, list.Total)
				}
				if stats.Total > 0 {
					nonEmpty++
				}
				if stats.Open+stats.Processed != stats.Total || stats.Redacted > stats.Total {
					t.Fatalf("filter %+v: stats %+v", f, stats)
				}
				var groups int64
				for _, g := range stats.Groups {
					groups += g.Total
				}
				if groups > stats.Total {
					t.Fatalf("filter %+v: group totals %d over total %d", f, groups, stats.Total)
				}
				if f.Processed == nil {
					open := f
					open.Processed = ptr(false)
					queue, err := s.List(ctx, ListParams{Filter: open, Limit: ptr(1)})
					if err != nil {
						t.Fatal(err)
					}
					if queue.Total != stats.Open {
						t.Fatalf("filter %+v: queue total %d, stats open %d", f, queue.Total, stats.Open)
					}
				}
			}
			if nonEmpty < 50 {
				t.Fatalf("only %d of 150 filters matched any row; the property is barely tested", nonEmpty)
			}
		})
	}
}
