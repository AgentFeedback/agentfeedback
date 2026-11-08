package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

// The wire goldens retain corpus metadata while replacing machine-dependent
// sizes and hashes only after their relationships have been checked.
func TestSessionsPushGolden(t *testing.T) {
	for _, harness := range []string{"claude-code", "codex", "opencode-v1"} {
		t.Run(harness, func(t *testing.T) {
			home := goldenWorld(t)
			goldenStores[harness](t, home)
			listed := runCLI(t, "", "sessions", "list", "--harness", harness, "--json")
			if listed.code != 0 {
				t.Fatal(listed)
			}
			var listing struct {
				Sessions []struct{ Ref, Path, State string }
			}
			decodeJSON(t, listed.stdout, &listing)
			var mu sync.Mutex
			var manifest map[string]any
			var transcript strings.Builder
			var requests []string
			const uploadID = "11111111-1111-4111-8111-111111111111"
			srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/openapi.json":
					fmt.Fprint(w, `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`)
				case "/api/v1/sessions/policy":
					fmt.Fprint(w, `{"org_id":"11111111-1111-4111-8111-111111111111","enabled":true,"notice_version":"1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`)
				case "/api/v1/meta":
					fmt.Fprint(w, `{}`)
				case "/api/v1/sessions/uploads":
					requests = append(requests, r.Method+" begin")
					d := json.NewDecoder(r.Body)
					d.UseNumber()
					if err := d.Decode(&manifest); err != nil {
						t.Error(err)
					}
					fmt.Fprintf(w, `{"id":%q,"expires_at":"2099-01-01T00:00:00Z","received_parts":[],"submission_id":null}`, uploadID)
				case "/api/v1/sessions/uploads/" + uploadID + "/parts/1":
					requests = append(requests, r.Method+" part")
					var part struct{ Content, SHA256 string }
					if err := json.NewDecoder(r.Body).Decode(&part); err != nil {
						t.Error(err)
					}
					if part.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(part.Content))) {
						t.Error("part digest does not match uploaded bytes")
					}
					transcript.WriteString(part.Content)
					fmt.Fprintf(w, `{"id":%q,"part":1}`, uploadID)
				case "/api/v1/sessions/uploads/" + uploadID + "/complete":
					requests = append(requests, r.Method+" complete")
					b, err := io.ReadAll(r.Body)
					if err != nil || len(b) != 0 {
						t.Error("complete must have no request entity")
					}
					w.WriteHeader(http.StatusCreated)
					fmt.Fprintf(w, `{"submission":{"id":"42","uid":%q},"warnings":[]}`, uploadID)
				default:
					t.Errorf("unexpected upload request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			var snapshots []any
			for _, session := range listing.Sessions {
				if session.State != "new" {
					continue
				}
				mu.Lock()
				manifest = nil
				transcript.Reset()
				requests = nil
				mu.Unlock()
				args := []string{"sessions", "push", session.Ref, "--since", "2020-01-01T00:00:00Z", "--json"}
				first := runCLI(t, "", args...)
				if first.code != 0 {
					t.Fatalf("first push %s: %+v", session.Ref, first)
				}
				second := runCLI(t, "", args...)
				if second.code != 0 {
					t.Fatalf("replay %s: %+v", session.Ref, second)
				}
				mu.Lock()
				text := transcript.String()
				gotRequests := strings.Join(requests, ",")
				mu.Unlock()
				if gotRequests != "POST begin,PUT part,POST complete" {
					t.Fatalf("upload/replay requests: %s", gotRequests)
				}
				if harness != "opencode-v1" {
					raw, err := os.ReadFile(session.Path)
					if err != nil {
						t.Fatal(err)
					}
					raw = raw[:bytes.LastIndexByte(raw, '\n')+1]
					want, _ := scrub.String(string(raw))
					if text != want {
						t.Fatal("uploaded text differs from scrubbed complete source lines")
					}
				}
				if manifest["content_sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) || manifest["content_bytes"] != json.Number(strconv.Itoa(len(text))) {
					t.Fatal("manifest digest/bytes differ from uploaded transcript")
				}
				envelope := manifest["envelope"].(map[string]any)
				payload := envelope["payload"].(map[string]any)
				revision := manifest["revision"].(string)
				_, sid, _ := strings.Cut(session.Ref, ":")
				if envelope["key"] != fmt.Sprintf("session-%x", sha256.Sum256([]byte(harness+"\x00"+sid+"\x00"+revision))) {
					t.Fatal("incorrect revision identity")
				}
				if payload["revision"] != revision || payload["size"] != json.Number(revision) {
					t.Fatal("inconsistent source revision")
				}
				payload["transcript"] = text
				if manifest["envelope_bytes"] != json.Number(strconv.Itoa(len(canonjson.Marshal(envelope)))) {
					t.Fatal("incorrect assembled canonical envelope size")
				}
				delete(payload, "transcript")
				manifest["installation"] = "<MACHINE>"
				manifest["revision"], payload["revision"], payload["size"] = "<SOURCE_BYTES>", "<SOURCE_BYTES>", "<SOURCE_BYTES>"
				manifest["content_sha256"], manifest["content_bytes"], manifest["envelope_bytes"] = "<SHA256>", "<CONTENT_BYTES>", "<ENVELOPE_BYTES>"
				envelope["machine"], envelope["key"] = "<MACHINE>", "<REVISION_KEY>"
				context := envelope["context"].(map[string]any)
				if _, ok := context["client_version"]; ok {
					context["client_version"] = "<VERSION>"
				}
				var outcomes []any
				for _, output := range []string{first.stdout, second.stdout} {
					var rows []map[string]any
					if err := json.Unmarshal([]byte(output), &rows); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if _, ok := row["revision"]; ok {
							row["revision"] = "<SOURCE_BYTES>"
						}
						if _, ok := row["bytes"]; ok {
							row["bytes"] = "<ENVELOPE_BYTES>"
						}
						outcomes = append(outcomes, row)
					}
				}
				snapshots = append(snapshots, map[string]any{"manifest": manifest, "outcomes": outcomes})
			}
			got, err := json.MarshalIndent(snapshots, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "sessions", harness+".push.golden.json")
			if *updateSessionsGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("upload manifest/outcomes differ from %s\n%s", path, got)
			}
		})
	}
}
