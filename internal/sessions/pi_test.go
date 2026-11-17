package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

func piFixture() string {
	return `{"type":"session","version":3,"id":"pi-native","cwd":"/work/pi","timestamp":"2026-11-01T00:00:00Z","parentSession":"/not/read.jsonl"}` + "\n" +
		`{"type":"model_change","id":"model","parentId":null,"modelId":"native-model"}` + "\n" +
		`{"type":"message","id":"user","parentId":"model","message":{"role":"user","content":"human prompt","timestamp":1793491200000}}` + "\n" +
		`{"type":"message","id":"assistant","parentId":"user","message":{"role":"assistant","content":[{"type":"thinking","thinking":"not prose"},{"type":"toolCall","id":"call","name":"bash","arguments":{"command":"false"}}],"timestamp":1793491201000,"durationMs":5}}` + "\n" +
		`{"type":"message","id":"result","parentId":"assistant","message":{"role":"toolResult","toolCallId":"call","toolName":"bash","content":[{"type":"text","text":"failed"}],"details":{"__interrupted":true},"isError":true,"timestamp":1793491202000}}` + "\n"
}
func parsePi(t *testing.T, raw string) (*transcript, error) {
	t.Helper()
	_, m, err := piHeader(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return (piReader{}).parse(strings.NewReader(raw), m)
}
func TestPiNativeSchema(t *testing.T) {
	raw := piFixture()
	tr, err := parsePi(t, raw+"{unfinished")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if tr.size != int64(len(raw)) || tr.head != hex.EncodeToString(sum[:]) || tr.total.Prompts != 1 || tr.total.ToolCalls != 1 || tr.total.ToolErrors != 1 || tr.total.Interrupts != 0 || tr.lastModel != "native-model" {
		t.Fatalf("native metadata: size=%d counts=%+v model=%q", tr.size, tr.total, tr.lastModel)
	}
}
func TestPiMalformedUnknownAndDuplicateRecords(t *testing.T) {
	tr, err := parsePi(t, piFixture()+"not json\n"+`{"type":"future","id":"future","parentId":"result","content":"not human"}`+"\n")
	if err != nil || tr.total.Unparsed != 1 || tr.total.Prompts != 1 {
		t.Fatalf("unknown/malformed: %v %+v", err, tr)
	}
	for _, record := range []string{
		`{"type":"message","id":"user","message":{"role":"user","content":"duplicate"}}`,
		`{"type":"session","version":3,"id":"second"}`,
		`{"type":"message","id":"duplicate-result","parentId":"result","message":{"role":"toolResult","toolCallId":"call","content":[],"isError":false}}`,
	} {
		if _, err := parsePi(t, piFixture()+record+"\n"); !errors.Is(err, errUnsupported) {
			t.Fatalf("duplicate accepted: %s: %v", record, err)
		}
	}
	branch := `{"type":"message","id":"sibling","parentId":"assistant","message":{"role":"toolResult","toolCallId":"call","content":[{"type":"text","text":"ok"}],"isError":false}}` + "\n"
	tr, err = parsePi(t, piFixture()+branch)
	if err != nil || tr.total.ToolCalls != 2 || tr.total.ToolErrors != 1 {
		t.Fatalf("sibling result: %v %+v", err, tr)
	}
}
func TestPiDiscoveryAndPolicy(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, "subagents"), 0700); err != nil {
		t.Fatal(err)
	}
	put := func(path, raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(project, "native.jsonl"), piFixture())
	put(filepath.Join(project, "subagents", "hidden.jsonl"), strings.ReplaceAll(piFixture(), "pi-native", "child"))
	env := Env{PiSessionsDir: root}
	r := piReader{}
	cs, problems, err := r.candidates(env)
	if err != nil || len(problems) != 0 || len(cs) != 1 {
		t.Fatalf("discovery: %d %+v %v", len(cs), problems, err)
	}
	env.Policy = collect.Policy{DenyPaths: []string{"/work/pi"}}
	if state, _ := r.gate(env, cs[0]); state != StateDenied {
		t.Fatalf("denied state: %s", state)
	}
	env.Policy = collect.Policy{}
	put(cs[0].path, strings.ReplaceAll(piFixture(), "/work/pi", "/changed"))
	if _, err := r.load(env, cs[0]); !errors.Is(err, errOMPChanged) {
		t.Fatalf("metadata changed: %v", err)
	}
	put(cs[0].path, piFixture())
	put(filepath.Join(project, "duplicate.jsonl"), piFixture())
	cs, problems, err = r.candidates(env)
	if err != nil || len(cs) != 0 || len(problems) != 2 {
		t.Fatalf("duplicate discovery: %d %+v %v", len(cs), problems, err)
	}
}
func TestPiHeaderVersionsAndBounds(t *testing.T) {
	for _, version := range []string{"1", "2", "4"} {
		raw := strings.Replace(piFixture(), `"version":3`, `"version":`+version, 1)
		_, m, err := piHeader(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if state, _ := (piReader{}).gate(Env{}, candidate{omp: m}); state != StateUnsupportedFormat {
			t.Fatalf("version %s accepted", version)
		}
	}
	for _, raw := range []string{strings.Repeat(" ", ompHeaderCap) + "\n", `{"type":"session"}`, "{malformed}\n"} {
		if _, _, err := piHeader(strings.NewReader(raw)); !errors.Is(err, errUnsupported) {
			t.Fatalf("invalid header accepted: %v", err)
		}
	}
}

func TestPiPolicyBeforeOpeningBody(t *testing.T) {
	env := Env{Policy: collect.Policy{OptInOnly: true}}
	c := candidate{sessionID: "denied", path: "/does/not/exist.jsonl", omp: &ompMeta{Version: 3, ID: "denied", Cwd: "/private"}}
	_, err := (piReader{}).load(env, c)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.State != StateDenied {
		t.Fatalf("opened denied source: %v", err)
	}
}

func TestPiDiscoveryRejectsLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(piFixture()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	cs, _, err := (piReader{}).candidates(Env{PiSessionsDir: root})
	if err != nil || len(cs) != 0 {
		t.Fatalf("linked transcript discovered: %d %v", len(cs), err)
	}
}
