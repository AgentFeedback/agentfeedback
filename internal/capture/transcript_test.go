package capture

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

func TestScrubTranscript(t *testing.T) {
	for _, tc := range transcriptScrubCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, counts, err := scrubTranscript(strings.NewReader(tc.source), maxEnvelope)
			if err != nil || got != tc.want {
				t.Fatal("scrubbed transcript differs from expected content")
			}
			if !reflect.DeepEqual(counts, tc.counts) {
				t.Fatalf("scrub counts = %v, want %v", counts, tc.counts)
			}
		})
	}
}

// Check the actual upload content, not only the helper's output: serialized
// JSONL must never bypass decoded-string scrubbing on the consumer path.
func TestPushTranscriptScrubbing(t *testing.T) {
	for _, tc := range transcriptScrubCases() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.source != "" && !strings.HasSuffix(tc.source, "\n") {
				tc.source += "\n"
				tc.want += "\n"
			}
			f := newPushFixture(t)
			base := testFixtureText(t, "basic.jsonl", "scrub", pushTestCwd)
			base = strings.NewReplacer(
				"AKIAABCDEFGHIJKLMNOP", "public-example",
				"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "public-example",
			).Replace(base)
			f.claude("scrub", base+tc.source)
			out := f.push(f.options("claude-code:scrub"))
			if len(out) != 1 || out[0].Status != "pushed" {
				t.Fatalf("outcomes: %+v", out)
			}
			want := base + tc.want
			if f.parts[1] != want {
				t.Fatal("uploaded transcript differs from expected scrubbed content")
			}
			if f.manifest["content_sha256"] != shaText(want) || treeInt(f.manifest["content_bytes"]) != len(want) {
				t.Fatal("upload digest or byte count does not describe scrubbed content")
			}
		})
	}
}

type transcriptScrubCase struct {
	name, source, want string
	counts             scrub.Counts
}

func transcriptScrubCases() []transcriptScrubCase {
	return []transcriptScrubCase{
		{
			name:   "escaped credential and embedded token",
			source: `{"password":"before\"after","content":"use gh\u0070_abcdefghijklmnopqrstuvwxyz0123456789"}` + "\r\n",
			want:   `{"password":"[REDACTED:assignment]","content":"use [REDACTED:github_token]"}` + "\r\n",
			counts: scrub.Counts{"assignment": 1, "github_token": 1},
		},
		{
			name:   "escaped member name and bearer prefix",
			source: `{"pass\u0077ord":"opaque","content":"\u0042earer abcdefghijklmnop"}` + "\n",
			want:   `{"password":"[REDACTED:assignment]","content":"Bearer [REDACTED:bearer_token]"}` + "\n",
			counts: scrub.Counts{"assignment": 1, "bearer_token": 1},
		},
		{
			name:   "unparsed text",
			source: "not json: ghp_abcdefghijklmnopqrstuvwxyz0123456789\r\nAPI_KEY=opaque\n",
			want:   "not json: [REDACTED:github_token]\r\nAPI_KEY=[REDACTED:env_line]\n",
			counts: scrub.Counts{"github_token": 1, "env_line": 1},
		},
		{
			name:   "raw multiline private key",
			source: "-----BEGIN PRIVATE KEY-----\r\nopaque-key-body\r\n-----END PRIVATE KEY-----\r\n{\"content\":\"safe\"}\n",
			want:   "[REDACTED:private_key]\r\n{\"content\":\"safe\"}\n",
			counts: scrub.Counts{"private_key": 1},
		},
		{
			name:   "decoded multiline private key",
			source: `{"content":"-----BEGIN PRIVATE KEY-----\nopaque-key-body\n-----END PRIVATE KEY-----"}` + "\n",
			want:   `{"content":"[REDACTED:private_key]"}` + "\n",
			counts: scrub.Counts{"private_key": 1},
		},
		{
			name:   "cross line bearer",
			source: "Bearer\nabcdefghijklmnop\n",
			want:   "Bearer\n[REDACTED:bearer_token]\n",
			counts: scrub.Counts{"bearer_token": 1},
		},
		{
			name:   "clean bytes and line endings",
			source: " \t{ \"content\" : \"safe \\u0061\", \"n\": 1e+03 } \r\n\r\nnot json\n[ true, null ]\r\nfinal line without newline",
			want:   " \t{ \"content\" : \"safe \\u0061\", \"n\": 1e+03 } \r\n\r\nnot json\n[ true, null ]\r\nfinal line without newline",
			counts: scrub.Counts{},
		},
		{
			name: "empty source", counts: scrub.Counts{},
		},
	}
}

func TestTranscriptStreamingLimits(t *testing.T) {
	source := io.MultiReader(
		strings.NewReader("-----BEGIN PRIVATE KEY-----\n"),
		&repeatedCaptureText{text: strings.Repeat("x", 4095) + "\n", remaining: 16385},
		strings.NewReader("-----END PRIVATE KEY-----\npublic\n"),
	)
	got, counts, err := scrubTranscript(source, 64)
	if err != nil || got != "[REDACTED:private_key]\npublic\n" || counts["private_key"] != 1 {
		t.Fatalf("large raw input: %q %v %v", got, counts, err)
	}
	got, _, err = scrubTranscript(strings.NewReader(strings.Repeat("public\n", 100)), 64)
	if !errors.Is(err, errTranscriptLarge) || got != "" {
		t.Fatalf("oversized scrubbed output: %q %v", got, err)
	}
	for _, source := range []string{"password\n:\nopaque\n", "Bearer\n\nabcdefghijklmnop\n", "password=\n\nopaque\n"} {
		want, _ := scrub.String(source)
		got, _, err := scrubTranscript(strings.NewReader(source), 128)
		if err != nil || got != want {
			t.Fatalf("cross-record redaction differs: %v", err)
		}
	}
}

type repeatedCaptureText struct {
	text      string
	remaining int
	offset    int
}

func (r *repeatedCaptureText) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.text[r.offset:])
	r.offset += n
	if r.offset == len(r.text) {
		r.offset = 0
		r.remaining--
	}
	return n, nil
}
