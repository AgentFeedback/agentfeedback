package core

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// problemCase expects a 400 validation_error (unless status/code say
// otherwise) with the pointer and every listed message fragment.
type problemCase struct {
	name    string
	run     func(s *Service) error
	status  int
	code    string
	pointer string
	message []string
}

func checkProblems(t *testing.T, cases []problemCase) {
	t.Helper()
	s, _, _ := newTestService(t)
	mustCreate(t, s, `{"kind":"k","summary":"one"}`)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := asProblem(t, c.run(s))
			status, code := c.status, c.code
			if status == 0 {
				status, code = 400, CodeValidation
			}
			if p.Status != status || p.Code != code {
				t.Errorf("got %d %s, want %d %s (%s)", p.Status, p.Code, status, code, p.Message)
			}
			if c.pointer != "" && (len(p.Details) == 0 || p.Details[0].Pointer != c.pointer) {
				t.Errorf("details %+v, want pointer %s", p.Details, c.pointer)
			}
			for _, m := range c.message {
				if !strings.Contains(p.Message, m) {
					t.Errorf("message %q lacks %q", p.Message, m)
				}
			}
		})
	}
}

func list(p ListParams) func(*Service) error {
	return func(s *Service) error { _, err := s.List(context.Background(), p); return err }
}

func stats(p StatsParams) func(*Service) error {
	return func(s *Service) error { _, err := s.Stats(context.Background(), p); return err }
}

func export(p ExportParams) func(*Service) error {
	return func(s *Service) error { return s.Export(context.Background(), p, io.Discard) }
}

func markBody(body string) func(*Service) error {
	return func(s *Service) error { _, err := s.Mark(context.Background(), 1, []byte(body)); return err }
}

func batchBody(body string) func(*Service) error {
	return func(s *Service) error { _, err := s.MarkBatch(context.Background(), []byte(body)); return err }
}

func TestParameterValidation(t *testing.T) {
	t.Parallel()
	early, late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	checkProblems(t, []problemCase{
		{"limit zero", list(ListParams{Limit: ptr(0)}), 0, "", "?limit", []string{"limit", "between 1 and 500", "got 0"}},
		{"limit over", list(ListParams{Limit: ptr(501)}), 0, "", "?limit", []string{"got 501"}},
		{"limit with payload", list(ListParams{Limit: ptr(101), IncludePayload: true}), 0, "", "?limit", []string{"between 1 and 100 with include=payload", "got 101"}},
		{"both cursors", list(ListParams{BeforeID: ptr(int64(5)), AfterID: ptr(int64(1))}), 0, "", "?before_id", []string{"before_id and after_id are mutually exclusive"}},
		{"before_id zero", list(ListParams{BeforeID: ptr(int64(0))}), 0, "", "?before_id", []string{"before_id", "at least 1", "got 0"}},
		{"after_id negative", list(ListParams{AfterID: ptr(int64(-1))}), 0, "", "?after_id", []string{"after_id", "at least 0", "got -1"}},
		{"schema_version zero", list(ListParams{Filter: Filter{SchemaVersion: ptr(int64(0))}}), 0, "", "?schema_version", []string{"schema_version", "at least 1", "got 0"}},
		{"blank kind", list(ListParams{Filter: Filter{Kind: "  "}}), 0, "", "?kind", []string{"kind", "non-blank token"}},
		{"blank exclude_kind", list(ListParams{Filter: Filter{ExcludeKind: []string{"a", ""}}}), 0, "", "?exclude_kind", []string{"exclude_kind"}},
		{"content_hash", list(ListParams{Filter: Filter{ContentHash: "ABC"}}), 0, "", "?content_hash", []string{"64 lower-case hex", `"ABC"`}},
		{"on", list(ListParams{Filter: Filter{On: "updated_at"}}), 0, "", "?on", []string{"created_at or occurred_at", `"updated_at"`}},
		{"q too long", list(ListParams{Filter: Filter{Q: strings.Repeat("x", 201)}}), 0, "", "?q", []string{"at most 200 bytes", "got 201"}},
		{"since after until", list(ListParams{Filter: Filter{Since: &early, Until: &late}}), 0, "", "?since", []string{"since", "until", "2026-01-01T00:00:00.000000Z"}},
		{"stats by unknown", stats(StatsParams{By: []string{"colour"}}), 0, "", "?by", []string{`"colour"`, "kind, project"}},
		{"stats by repeated", stats(StatsParams{By: []string{"kind", "kind"}}), 0, "", "?by", []string{`"kind" is repeated`}},
		{"stats by too many", stats(StatsParams{By: []string{"kind", "project", "model", "machine"}}), 0, "", "?by", []string{"at most 3", "got 4"}},
		{"stats top", stats(StatsParams{Top: ptr(51)}), 0, "", "?top", []string{"between 0 and 50", "got 51"}},
		{"stats bucket", stats(StatsParams{Bucket: "month"}), 0, "", "?bucket", []string{"day or week", `"month"`}},
		{"stats filter", stats(StatsParams{Filter: Filter{Q: strings.Repeat("x", 201)}}), 0, "", "?q", nil},
		{"export limit", export(ExportParams{Limit: ptr(0)}), 0, "", "?limit", []string{"between 1 and 500", "got 0"}},
		{"export after_id", export(ExportParams{AfterID: ptr(int64(-2))}), 0, "", "?after_id", []string{"got -2"}},
		{"get id", func(s *Service) error { _, err := s.Get(context.Background(), 0); return err }, 0, "", "/id", []string{"positive integer", "got 0"}},
		{"redact id", func(s *Service) error { _, err := s.Redact(context.Background(), -1); return err }, 0, "", "/id", nil},
		{"mark id", func(s *Service) error { _, err := s.Mark(context.Background(), 0, []byte(`{}`)); return err }, 0, "", "/id", nil},
		{"schema version", func(s *Service) error { _, err := s.Schema("friction", 0); return err }, 0, "", "/version", []string{"got 0"}},
		{"schema unknown", func(s *Service) error { _, err := s.Schema("friction", 9); return err }, 404, CodeNotFound, "", []string{`"friction" version 9`}},
	})
}

func TestMarkValidation(t *testing.T) {
	t.Parallel()
	over := `{"resolution":"` + strings.Repeat("x", 2001) + `"}`
	checkProblems(t, []problemCase{
		{"not json", markBody(`{`), 400, CodeBadRequest, "", []string{"not valid JSON"}},
		{"not object", markBody(`[]`), 400, CodeBadRequest, "", []string{"JSON object"}},
		{"trailing data", markBody(`{} {}`), 400, CodeBadRequest, "", []string{"data follows"}},
		{"duplicate", markBody(`{"verdict":"a","verdict":"b"}`), 0, "", "/verdict", []string{`"verdict" appears more than once`}},
		{"unknown member", markBody(`{"status":"done"}`), 0, "", "/status", []string{`"status" is not accepted`, "processed, verdict"}},
		{"ids on single", markBody(`{"ids":[1]}`), 0, "", "/ids", []string{`"ids" is not accepted`}},
		{"processed null", markBody(`{"processed":null}`), 0, "", "/processed", []string{"true or false", "got null"}},
		{"processed string", markBody(`{"processed":"yes"}`), 0, "", "/processed", []string{"got a string"}},
		{"verdict type", markBody(`{"verdict":1}`), 0, "", "/verdict", []string{"verdict must be a string", "got a number"}},
		{"verdict long", markBody(`{"verdict":"` + strings.Repeat("v", 65) + `"}`), 0, "", "/verdict", []string{"at most 64 bytes", "got 65"}},
		{"resolution blank", markBody(`{"resolution":"  "}`), 0, "", "/resolution", []string{"resolution must not be blank"}},
		{"resolution long", markBody(over), 0, "", "/resolution", []string{"at most 2000 bytes", "got 2001"}},
		{"ref long", markBody(`{"ref":"` + strings.Repeat("r", 201) + `"}`), 0, "", "/ref", []string{"at most 200 bytes"}},
		{"processed_by long", markBody(`{"processed_by":"` + strings.Repeat("p", 201) + `"}`), 0, "", "/processed_by", []string{"at most 200 bytes"}},
		{"fields with false", markBody(`{"processed":false,"verdict":"fixed"}`), 0, "", "/verdict", []string{"allowed only with processed=true"}},
		{"too large", markBody(`{"ref":"` + strings.Repeat("r", 10485760) + `"}`), 413, CodeRequestTooLarge, "", []string{"10485760"}},
		{"batch no ids", batchBody(`{"verdict":"fixed"}`), 0, "", "/ids", []string{"ids is required"}},
		{"batch ids empty", batchBody(`{"ids":[]}`), 0, "", "/ids", []string{"1 to 500 ids", "got 0"}},
		{"batch ids too many", batchBody(`{"ids":[` + strings.Repeat("1,", 500) + `1]}`), 0, "", "/ids", []string{"got 501"}},
		{"batch ids type", batchBody(`{"ids":"1"}`), 0, "", "/ids", []string{"array", "got a string"}},
		{"batch id zero", batchBody(`{"ids":[1,0]}`), 0, "", "/ids/1", []string{"ids[1] must be a positive integer", `"0"`}},
		{"batch id fraction", batchBody(`{"ids":[1.0]}`), 0, "", "/ids/0", []string{`"1.0"`}},
		{"batch id overflow", batchBody(`{"ids":[99999999999999999999]}`), 0, "", "/ids/0", nil},
		{"batch unknown", batchBody(`{"ids":[1],"x":1}`), 0, "", "/x", []string{"ids, processed"}},
	})
}

func TestMeta(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestService(t)
	out, err := Marshal(s.Meta())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"service_version":"4.0.0","api_version":"1.0","export_format":2,"dedupe_window_s":86400,` +
		`"limits":{"body_bytes":10485760,"import_bytes":33554432,"list_max":500,"list_max_with_payload":100,` +
		`"processed_ids_max":500,"context_entries":32,"context_value_bytes":2000,"identifier_bytes":200,"summary_bytes":2000},` +
		`"kinds":[{"kind":"friction","versions":[1]},{"kind":"review","versions":[1]}],` +
		`"features":["q","stats","export.after_id","export.limit","import","mcp","redaction"],` +
		`"client":{"min_version":"4.0.0","latest_known":"4.0.0"}}`
	if string(out) != want {
		t.Errorf("meta\n got %s\nwant %s", out, want)
	}
	if doc, err := s.Schema("envelope", 1); err != nil || !strings.Contains(string(doc), "envelope") {
		t.Errorf("envelope schema: %v", err)
	}
	if len(s.Schemas()) != 3 {
		t.Errorf("schemas %v", s.Schemas())
	}
}

// TestMetaClientVersions: min_version is the constant oldest client that
// speaks API 1.0, not the server version; latest_known is the server version.
func TestMetaClientVersions(t *testing.T) {
	t.Parallel()
	_, db, _ := newTestService(t)
	got := New(db, Config{Version: "4.0.1"}).Meta().Client
	if got != (MetaClient{MinVersion: "4.0.0", LatestKnown: "4.0.1"}) || ClientMinVersion != "4.0.0" {
		t.Errorf("client %+v, ClientMinVersion %s", got, ClientMinVersion)
	}
}

func TestStatsRequestedEmpty(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestService(t)
	st, err := s.Stats(context.Background(), StatsParams{By: []string{"kind"}, Bucket: "day"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := Marshal(st)
	if string(out) != `{"total":0,"open":0,"processed":0,"redacted":0,"groups":[],"recurring":[],"series":[]}` {
		t.Errorf("requested stats on an empty database: %s", out)
	}
	st, err = s.Stats(context.Background(), StatsParams{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = Marshal(st)
	if string(out) != `{"total":0,"open":0,"processed":0,"redacted":0,"recurring":[]}` {
		t.Errorf("unrequested groups or series: %s", out)
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newTestService(t)
	for range 5 {
		mustCreate(t, s, `{"kind":"k","summary":"<&>","payload":{"i":`+itoa(int64(len(t.Name())))+`}}`)
		mustCreate(t, s, `{"kind":"k","key":"`+itoa(int64(rowSeq(s)))+`","summary":"x"}`)
	}
	page, err := s.List(ctx, ListParams{Limit: ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 6 || !page.HasMore || *page.NextBeforeID != page.Submissions[1].ID || page.NextAfterID != nil ||
		page.Submissions[0].Payload != nil {
		t.Fatalf("first page %+v", page)
	}
	out, _ := Marshal(page)
	if !strings.Contains(string(out), `"next_after_id":null`) || strings.Contains(string(out), `"payload"`) ||
		!strings.Contains(string(out), `"summary":"x"`) {
		t.Errorf("page JSON %s", out)
	}
	up, err := s.List(ctx, ListParams{AfterID: ptr(int64(0)), Limit: ptr(4), IncludePayload: true})
	if err != nil || up.Submissions[0].ID != 1 || *up.NextAfterID != 4 || up.NextBeforeID != nil || up.Submissions[0].Payload == nil {
		t.Fatalf("after page %+v %v", up, err)
	}
	if out, _ := Marshal(up); !strings.Contains(string(out), `"summary":"<&>"`) {
		t.Errorf("HTML-escaped: %s", out)
	}
}

func rowSeq(s *Service) int {
	res, _ := s.List(context.Background(), ListParams{})
	return int(res.Total)
}
