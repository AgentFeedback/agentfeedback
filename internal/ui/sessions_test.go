package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// sessionsSource makes accidental API access fail even if the page ignores
// the source's error. Session status must always stay local.
type sessionsSource struct {
	t     *testing.T
	calls int
}

func (s *sessionsSource) Do(context.Context, string, string, url.Values, []byte) (*client.Response, error) {
	s.calls++
	s.t.Error("sessions page called the API source")

	return nil, errors.New("API access forbidden")
}

func sessionFixture(t *testing.T) (*store.DB, sessions.Env) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	home := filepath.Join(base, "home-<script>alert(1)<&>")
	env := sessions.Env{
		Home: home, ClaudeConfigDir: filepath.Join(home, ".claude"),
		CodexHome: filepath.Join(home, ".codex"), CopilotHome: filepath.Join(home, ".copilot"),
		GeminiDir: filepath.Join(home, ".gemini"), DataHome: filepath.Join(home, ".local", "share"),
		Policy: collect.Policy{DenyPaths: []string{"/denied"}},
		Now:    func() time.Time { return testNow },
	}
	db, err := store.Open(ctx, filepath.Join(base, "agentfeedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	put := func(project, id, body string) string {
		t.Helper()
		path := filepath.Join(env.ClaudeConfigDir, "projects", project, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}
	line := func(id, cwd string) string {
		body, err := json.Marshal(map[string]any{
			"type": "user", "uuid": "u-1", "timestamp": "2030-01-01T00:00:00Z",
			"sessionId": id, "cwd": cwd,
			"message": map[string]string{"content": "PRIVATE_TRANSCRIPT_MUST_NOT_RENDER"},
		})
		if err != nil {
			t.Fatal(err)
		}

		return string(body) + "\n"
	}
	for _, id := range []string{"new-a", "new-b", "processed", "changed", "unreadable"} {
		body := line(id, "/nonexistent-af-ui/project")
		path := put("-nonexistent-af-ui-project", id, body)
		if id != "processed" && id != "changed" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		head := sha256.Sum256([]byte(strings.TrimSuffix(body, "\n")))
		if err := db.Write(ctx, func(q store.Querier) error {
			if err := store.UpsertSessionDigest(ctx, q, store.SessionDigest{
				Harness: sessions.HarnessClaudeCode, SessionID: id, Path: path,
				Mtime: info.ModTime().UnixNano(), Size: int64(len(body)),
				Head: hex.EncodeToString(head[:]), DigestedAt: testNow.UnixMicro(),
			}); err != nil {
				return err
			}

			return store.MarkSession(ctx, q, sessions.HarnessClaudeCode, id, testNow.UnixMicro(), sessions.OutcomeNothing, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if id == "changed" {
			put("-nonexistent-af-ui-project", id, body+line(id, "/nonexistent-af-ui/project"))
		}
	}
	put("-nonexistent-af-ui-project", "unsupported", "[1,2]\n{\"no\":1}\n")
	put("-nonexistent-af-ui-project", "unknown", line("unknown", ""))
	put("-denied", "denied", line("denied", "/denied"))
	if err := db.Write(ctx, func(q store.Querier) error {
		if err := store.UpsertSessionDigest(ctx, q, store.SessionDigest{
			Harness: sessions.HarnessClaudeCode, SessionID: "absent",
			Path:       filepath.Join(env.ClaudeConfigDir, "projects", "-gone", "absent.jsonl"),
			DigestedAt: testNow.UnixMicro(),
		}); err != nil {
			return err
		}

		return store.SetTriageState(ctx, q, "unrelated", "retain me")
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetSelection(ctx, db, sessions.Selection{
		Harnesses: []string{sessions.HarnessClaudeCode, "codex"}, Since: "<script>selection</script>&", Limit: 7,
	}); err != nil {
		t.Fatal(err)
	}
	// A file in place of the archive directory yields a real store problem
	// without relying on permission bits (tests may run as root).
	if err := os.MkdirAll(filepath.Join(env.CodexHome, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.CodexHome, "archived_sessions"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Open = func(path string) (io.ReadCloser, error) {
		if filepath.Base(path) == "unreadable.jsonl" {
			return nil, fs.ErrPermission
		}

		return os.Open(path)
	}

	return db, env
}

type sessionStateSnapshot struct {
	Rows   map[string][]store.SessionSeen
	Triage map[string]string
	Files  map[string]string
}

func snapshotSessionState(t *testing.T, db *store.DB, home string) sessionStateSnapshot {
	t.Helper()
	ctx := context.Background()
	out := sessionStateSnapshot{Rows: map[string][]store.SessionSeen{}, Triage: map[string]string{}, Files: map[string]string{}}
	if err := db.Read(ctx, func(q store.Querier) error {
		for _, harness := range sessions.Harnesses() {
			rows, err := store.ListSessions(ctx, q, harness)
			if err != nil {
				return err
			}
			out.Rows[harness] = rows
		}
		rows, err := q.QueryContext(ctx, "SELECT key, value FROM triage_state ORDER BY key")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				return err
			}
			out.Triage[key] = value
		}

		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out.Files[path] = "directory"

			return nil
		}
		body, err := os.ReadFile(path)
		if err == nil {
			info, statErr := entry.Info()
			if statErr != nil {
				return statErr
			}
			out.Files[path] = fmt.Sprintf("%v:%d:%s", info.Mode(), info.ModTime().UnixNano(), body)
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}

	return out
}

func TestSessions_RealStatusReadOnly(t *testing.T) {
	db, env := sessionFixture(t)
	before := snapshotSessionState(t, db, env.Home)
	src := &sessionsSource{t: t}
	var status sessions.StatusOutput
	calls := 0
	h, token := newHandler(t, src, func(ctx context.Context) (sessions.StatusOutput, error) {
		calls++
		var err error
		status, err = sessions.Status(ctx, db, env)

		return status, err
	})
	body := get(t, h, "/"+token+"/sessions")
	if calls != 1 || src.calls != 0 {
		t.Fatalf("provider calls %d, source calls %d", calls, src.calls)
	}
	var claude sessions.HarnessStatus
	for _, harness := range status.Harnesses {
		if !strings.Contains(body, "<h2>"+harness.Harness+"</h2>") ||
			!strings.Contains(body, "<code>"+template.HTMLEscapeString(harness.Location)+"</code>") ||
			!strings.Contains(body, "<dd>"+harness.Store+"</dd>") {
			t.Errorf("missing harness store: %+v", harness)
		}
		for state, count := range harness.States {
			if !strings.Contains(body, fmt.Sprintf("<tr><td>%s</td><td>%d</td></tr>", state, count)) {
				t.Errorf("missing %s count %d", state, count)
			}
		}
		for _, problem := range harness.Problems {
			if !strings.Contains(body, "<li>"+template.HTMLEscapeString(problem)+"</li>") {
				t.Errorf("missing escaped problem %q", problem)
			}
		}
		if harness.Harness == sessions.HarnessClaudeCode {
			claude = harness
		}
	}
	want := map[string]int{
		sessions.StateNew: 2, sessions.StateProcessed: 1, sessions.StateChanged: 1,
		sessions.StateAbsent: 1, sessions.StateDenied: 1, sessions.StateUnreadable: 1,
		sessions.StateUnknownProject: 1, sessions.StateUnsupportedFormat: 1,
	}
	if !reflect.DeepEqual(claude.States, want) {
		t.Fatalf("fixture states = %v, want %v", claude.States, want)
	}
	problems := 0
	for _, harness := range status.Harnesses {
		problems += len(harness.Problems)
	}
	if problems == 0 {
		t.Fatal("fixture did not produce a store problem")
	}
	for _, text := range []string{
		"Saved selection", "<code>claude-code</code>", "<code>codex</code>",
		"&lt;script&gt;selection&lt;/script&gt;&amp;", "<dt>Limit</dt><dd>7</dd>", "No sessions.",
	} {
		if !strings.Contains(body, text) {
			t.Errorf("missing %q", text)
		}
	}
	for _, forbidden := range []string{"<script>alert", "<script>selection", "PRIVATE_TRANSCRIPT_MUST_NOT_RENDER"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("rendered unsafe or private content %q", forbidden)
		}
	}
	w := serve(h, req{method: http.MethodHead, target: "/" + token + "/sessions"})
	checkHeaders(t, "sessions HEAD", w)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "0" {
		t.Fatalf("HEAD: %d, headers %v, body %q", w.Code, w.Header(), w.Body.String())
	}
	if after := snapshotSessionState(t, db, env.Home); !reflect.DeepEqual(before, after) {
		t.Fatal("sessions GET/HEAD mutated session watermarks, saved state or harness files")
	}
}

func TestSessions_DisabledAndNoSelection(t *testing.T) {
	db, env := sessionFixture(t)
	env.Policy.Disabled = true
	if err := db.Write(context.Background(), func(q store.Querier) error {
		_, err := q.ExecContext(context.Background(), "DELETE FROM triage_state WHERE key = ?", sessions.SelectionKey)

		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotSessionState(t, db, env.Home)
	h, token := newHandler(t, &sessionsSource{t: t}, func(ctx context.Context) (sessions.StatusOutput, error) {
		return sessions.Status(ctx, db, env)
	})
	body := get(t, h, "/"+token+"/sessions")
	if !strings.Contains(body, "<tr><td>disabled</td><td>8</td></tr>") || !strings.Contains(body, "No saved selection.") {
		t.Fatal(body)
	}
	if after := snapshotSessionState(t, db, env.Home); !reflect.DeepEqual(before, after) {
		t.Fatal("disabled status mutated local state")
	}
}

func TestSessions_UnavailableAndError(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider func(context.Context) (sessions.StatusOutput, error)
		code     int
		message  string
	}{
		{"unavailable", nil, http.StatusServiceUnavailable, "local session status is unavailable"},
		{"error", func(context.Context) (sessions.StatusOutput, error) {
			return sessions.StatusOutput{}, errors.New("status <script>failed</script>")
		}, http.StatusInternalServerError, "status &lt;script&gt;failed&lt;/script&gt;"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, token := newHandler(t, &sessionsSource{t: t}, tt.provider)
			w := serve(h, req{target: "/" + token + "/sessions"})
			checkHeaders(t, tt.name, w)
			if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.message) || strings.Contains(w.Body.String(), "<script>failed") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSessions_ProviderAfterSecurityChecks(t *testing.T) {
	type contextKey struct{}
	calls := 0
	h, token := newHandler(t, &sessionsSource{t: t}, func(ctx context.Context) (sessions.StatusOutput, error) {
		calls++
		if ctx.Value(contextKey{}) != "request-context" {
			t.Error("provider did not receive the request context")
		}

		return sessions.StatusOutput{}, nil
	})
	foreign := "http://evil.example"
	for _, request := range []req{
		{target: "/" + token + "/sessions", host: "evil.example"},
		{target: "/" + token + "/sessions", origin: &foreign},
		{target: "/wrong-token/sessions"},
		{target: "/" + token + "/sessions", method: http.MethodPost},
	} {
		serve(h, request)
	}
	if calls != 0 {
		t.Fatalf("provider called %d times for rejected requests", calls)
	}
	r := httptest.NewRequest(http.MethodGet, "http://"+testAddr+"/"+token+"/sessions", nil)
	r = r.WithContext(context.WithValue(r.Context(), contextKey{}, "request-context"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if calls != 1 || w.Code != http.StatusOK {
		t.Fatalf("provider calls %d, status %d", calls, w.Code)
	}
}
