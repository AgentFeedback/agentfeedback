package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harnessreg "github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// skillDirsOf is every harness's skill directories under home, with no
// environment variable set.
func skillDirsOf(home string) []harnessreg.SkillDirs {
	return harnessreg.SkillLocations(harnessreg.Env{Home: home, Getenv: func(string) string { return "" }})
}

// putSkill writes body as root/dir/SKILL.md.
func putSkill(t *testing.T, root, dir, body string) string {
	t.Helper()
	p := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return p
}

func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: Does " + name + ".\n---\n\nBody.\n"
}

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}

	return p
}

// projectReaders put one session per reader that records cwd, and return
// the session id it is listed under.
var projectReaders = []struct {
	harness string
	put     func(w *ocWorld, cwd, sid string) string
}{
	{HarnessClaudeCode, func(w *ocWorld, cwd, sid string) string {
		w.put(cwd, sid, fixture(w.t, "basic.jsonl", sid, cwd))
		return sid
	}},
	{HarnessCodex, func(w *ocWorld, cwd, sid string) string {
		p := filepath.Join(w.home, ".codex", "sessions", "2026", "10", "08", "rollout-2026-10-08T07-39-40-"+sid+"1.jsonl")
		mkdir(w.t, filepath.Dir(p))
		content := fixture(w.t, filepath.Join("stores", "codex", "sessions", "2026", "10", "08", "rollout-2026-10-08T07-39-40-{{SID}}1.jsonl"), sid, cwd)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
		return sid + "1"
	}},
	{HarnessCopilot, func(w *ocWorld, cwd, sid string) string {
		cpPut(w.world, sid, cpFixture(w.t, sid, cwd), cpYAML(sid, cwd))
		return sid
	}},
	{HarnessGeminiCLI, func(w *ocWorld, cwd, sid string) string {
		g := &gworld{world: w.world, dir: w.env().GeminiDir}
		g.put(sid, cwd, sid, fixture(w.t, filepath.Join("stores", "gemini-cli", "tmp", "golden-proj", "chats", "session-2026-10-08T07-39-{{SID}}.jsonl"), sid, cwd))
		return "session-" + sid
	}},
	{HarnessOpenCode, func(w *ocWorld, cwd, sid string) string {
		if _, err := os.Stat(w.dbPath()); err != nil {
			w.create(cwd)
			return ocSID
		}
		w.exec(fixture(w.t, filepath.Join("skills", "opencode", "skills.sql"), ocSID, cwd))
		return ocSID + "_skills"
	}},
	{HarnessOpenCodeV2, func(w *ocWorld, cwd, sid string) string {
		if _, err := os.Stat(w.dbPath()); err != nil {
			w.createV2(cwd)
			return ocSID
		}
		w.exec(fixture(w.t, filepath.Join("skills", "opencode-v2", "skills.sql"), ocSID, cwd))
		return ocSID + "_skills"
	}},
}

// TestListProjects: --project keeps the sessions in the project, by every
// reader; a symbolic link to the project selects the same sessions; a
// denied session inside stays listed denied, one outside is not listed.
func TestListProjects(t *testing.T) {
	for _, r := range projectReaders {
		t.Run(r.harness, func(t *testing.T) {
			w := newOCWorld(t)
			base := t.TempDir()
			inside := mkdir(t, filepath.Join(base, "proj"))
			sub := mkdir(t, filepath.Join(inside, "sub"))
			outside := mkdir(t, filepath.Join(base, "other"))
			in := r.put(w, sub, "in")
			out := r.put(w, outside, "out")
			link := filepath.Join(base, "link")
			if err := os.Symlink(inside, link); err != nil {
				t.Fatal(err)
			}
			listed := func(projects ...string) map[string]string {
				l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: r.harness, Projects: projects})
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]string{}
				for _, s := range l.Sessions {
					got[s.SessionID] = s.State
				}
				return got
			}
			for _, p := range []string{inside, link} {
				if got := listed(p); len(got) != 1 || got[in] != StateNew {
					t.Errorf("--project %s: %v, want only %s", p, got, in)
				}
			}
			if got := listed(); len(got) != 2 || got[out] != StateNew {
				t.Errorf("no --project: %v, want %s and %s", got, in, out)
			}
			w.pol.DenyPaths = []string{inside}
			if got := listed(inside); len(got) != 1 || got[in] != StateDenied {
				t.Errorf("denied inside: %v, want %s denied", got, in)
			}
		})
	}
}

// TestListProjectsRefusedAfterRead: a session refused once its recorded
// directory is read is still placed in its project, with nothing of it
// listed; a row whose file is gone is in no project.
func TestListProjectsRefusedAfterRead(t *testing.T) {
	w := newWorld(t)
	base := t.TempDir()
	inside := mkdir(t, filepath.Join(base, "proj"))
	launch := mkdir(t, filepath.Join(base, "launch"))
	w.put(launch, "s1", fixture(t, "basic.jsonl", "s1", inside))
	w.pol.DenyPaths = []string{inside}
	l := w.list(ListOptions{Projects: []string{inside}})
	if len(l.Sessions) != 1 || l.Sessions[0].State != StateDenied || l.Sessions[0].Counts != nil || l.Sessions[0].Cwds != nil {
		t.Fatalf("refused after the read: %+v", l.Sessions)
	}
	w.pol.DenyPaths = nil
	w.digestOne("s1")
	if err := os.Remove(filepath.Join(w.cfg, "projects", encodeProject(launch), "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if l := w.list(ListOptions{}); len(l.Sessions) != 1 || l.Sessions[0].State != StateAbsent {
		t.Fatalf("absent row: %+v", l.Sessions)
	}
	if l := w.list(ListOptions{Projects: []string{inside}}); len(l.Sessions) != 0 {
		t.Fatalf("absent row listed in a project: %+v", l.Sessions)
	}
}

func TestSkillsInventory(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	repo := mkdir(t, filepath.Join(base, "repo"))
	mkdir(t, filepath.Join(repo, ".git"))
	sub := mkdir(t, filepath.Join(repo, "pkg", "sub"))
	putSkill(t, filepath.Join(repo, ".claude", "skills"), "build", skillMD("build"))
	shared := putSkill(t, filepath.Join(repo, ".agents", "skills"), "shared", skillMD("shared"))
	putSkill(t, filepath.Join(sub, ".claude", "skills"), "nested", skillMD("nested"))
	bad := putSkill(t, filepath.Join(repo, ".agents", "skills"), "bad", "# no front matter\n")
	badYAML := putSkill(t, filepath.Join(repo, ".agents", "skills"), "badyaml", "---\nname: [unclosed\n---\n")
	noName := putSkill(t, filepath.Join(repo, ".agents", "skills"), "noname", "---\ndescription: nameless\n---\n")
	mkdir(t, filepath.Join(repo, ".agents", "skills", "notaskill"))
	putSkill(t, filepath.Join(base, "elsewhere"), "linked", skillMD("linked"))
	if err := os.Symlink(filepath.Join(base, "elsewhere", "linked"), filepath.Join(repo, ".claude", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	global := putSkill(t, filepath.Join(home, ".claude", "skills"), "mine", skillMD("mine"))
	// Beyond the repository root: not in its walk.
	putSkill(t, filepath.Join(base, ".claude", "skills"), "outer", skillMD("outer"))

	env := Env{Home: home, SkillDirs: skillDirsOf(home)}
	if _, err := Skills(env, SkillsRequest{}); !errors.Is(err, ErrNoSkillScope) {
		t.Fatalf("no scope: %v", err)
	}
	inv, err := Skills(env, SkillsRequest{Projects: []string{sub}, Global: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range inv.Skills {
		got = append(got, s.Scope+" "+s.Name+" "+strings.Join(s.Harnesses, ","))
	}
	want := []string{
		"global mine claude-code",
		"project build claude-code,cursor,opencode-v1,opencode-v2,copilot",
		"project linked claude-code,cursor,opencode-v1,opencode-v2,copilot",
		"project nested claude-code,cursor,opencode-v1,opencode-v2,copilot",
		"project shared codex,cursor,opencode-v1,opencode-v2,copilot,gemini-cli",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("skills:\n got %q\nwant %q", got, want)
	}
	for _, s := range inv.Skills {
		switch s.Name {
		case "mine":
			if s.Path != global || s.Description != "Does mine." {
				t.Errorf("global skill: %+v", s)
			}
		case "shared":
			if s.Path != shared {
				t.Errorf("shared skill path %s, want %s", s.Path, shared)
			}
		}
	}
	var problems []string
	for _, p := range inv.Problems {
		problems = append(problems, p.Path)
	}
	if want := []string{bad, badYAML, noName}; !slices.Equal(problems, want) {
		t.Fatalf("problems %+v, want %q", inv.Problems, want)
	}

	// Without a .git above it, the project directory alone is read.
	loose := mkdir(t, filepath.Join(t.TempDir(), "loose", "sub"))
	putSkill(t, filepath.Join(filepath.Dir(loose), ".claude", "skills"), "parent", skillMD("parent"))
	inv, err = Skills(env, SkillsRequest{Projects: []string{loose}})
	if err != nil || len(inv.Skills) != 0 {
		t.Fatalf("loose project: %+v, %v", inv, err)
	}
}

func hitNames(tr *transcript) []string {
	var out []string
	for _, h := range tr.skillHits {
		out = append(out, h.name+"|"+h.dir+"|"+h.span)
	}

	return out
}

func TestSkillDetection(t *testing.T) {
	t.Run("claude-code", func(t *testing.T) {
		log := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"plug:review"}}]}}
{"type":"user","uuid":"u2","message":{"role":"user","content":"<command-name>/build</command-name>\n<command-message>build</command-message>"}}
{"type":"user","uuid":"u3","message":{"role":"user","content":[{"type":"text","text":"<command-message>lint</command-message>\n<command-name>/lint</command-name>"}]}}
{"type":"user","uuid":"u5","isMeta":true,"message":{"role":"user","content":"<command-name>/meta</command-name>"}}
{"type":"user","uuid":"u6","message":{"role":"user","content":"Explain <command-name>/quoted</command-name> to me"}}
{"type":"assistant","uuid":"a4","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/x/skills/edit/SKILL.md"}}]}}
`
		tr, err := claudeReader{}.parse(strings.NewReader(log))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitNames(tr), []string{"plug:review||a1", "build||u2", "lint||u3"}; !slices.Equal(got, want) {
			t.Fatalf("hits %q, want %q", got, want)
		}
		tr, err = claudeReader{}.parse(&captureContextReader{Reader: strings.NewReader(log), ctx: context.Background(), captureOnly: true})
		if err != nil || len(tr.skillHits) != 0 {
			t.Fatalf("capture keeps skill hits: %+v, %v", tr.skillHits, err)
		}
	})
	t.Run("codex", func(t *testing.T) {
		log := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<skill>\n<name>review</name>\n<path>/h/.agents/skills/review/SKILL.md</path>\n</skill>"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"sed -n 1,40p /h/.agents/skills/lint/SKILL.md\"}","call_id":"c1"}}
{"type":"response_item","payload":{"type":"function_call","name":"view","arguments":"{\"path\":\"/p/.codex/skills/fmt/SKILL.md\"}","call_id":"c2"}}
{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"type\",\"C:\\\\Users\\\\me\\\\.agents\\\\skills\\\\win\\\\SKILL.md\"]}","call_id":"c4"}}
{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Update File: /p/.agents/skills/edit/SKILL.md","call_id":"c3"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<skill>\n<name>said</name>\n</skill>"}]}}
`
		tr, err := codexReader{}.parse(strings.NewReader(log))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitNames(tr), []string{"review||L0", "|lint|c1", "|fmt|c2", "|win|c4"}; !slices.Equal(got, want) {
			t.Fatalf("hits %q, want %q", got, want)
		}
	})
	t.Run("copilot", func(t *testing.T) {
		log := `{"type":"skill.invoked","data":{"name":"review","path":"/h/.copilot/skills/review/SKILL.md"},"id":"e1"}
{"type":"skill.invoked","data":{"name":"sub"},"id":"e2","agentId":"agent-1"}
`
		tr, err := copilotReader{}.parse(strings.NewReader(log), "")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitNames(tr), []string{"review||e1"}; !slices.Equal(got, want) {
			t.Fatalf("hits %q, want %q", got, want)
		}
	})
	t.Run("gemini-cli", func(t *testing.T) {
		log := `{"sessionId":"s"}
{"id":"m1","type":"gemini","content":[],"toolCalls":[{"id":"c1","name":"activate_skill","args":{"name":"draft"},"status":"executing"}]}
{"id":"m1","type":"gemini","content":[],"toolCalls":[{"id":"c1","name":"activate_skill","args":{"name":"review"},"status":"success"}]}
`
		tr, err := geminiReader{}.parse(strings.NewReader(log), "")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitNames(tr), []string{"review||m1"}; !slices.Equal(got, want) {
			t.Fatalf("hits %q, want %q", got, want)
		}
		// The rewrite that named the skill places it, not the message's
		// first line: an incremental digest from there still sees it.
		if at := int64(strings.Index(log, `{"id":"m1","type":"gemini","content":[],"toolCalls":[{"id":"c1","name":"activate_skill","args":{"name":"review"}`)); tr.skillHits[0].offset != at {
			t.Fatalf("hit offset %d, want %d", tr.skillHits[0].offset, at)
		}
	})
}

// TestSkillsUsed: hits match by exact name, a path by its skill
// directory; unknown skills (a plugin's "plugin:skill" among them) are
// dropped, each skill is reported once at its first use, and hits before
// from are not.
func TestSkillsUsed(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	putSkill(t, filepath.Join(home, ".agents", "skills"), "review-dir", skillMD("review"))
	putSkill(t, filepath.Join(cwd, ".claude", "skills"), "build", skillMD("build"))
	idx := newSkillIndex(Env{Home: home, SkillDirs: skillDirsOf(home)})
	tr := &transcript{cwds: []string{cwd}, skillHits: []skillHit{
		{name: "unknown", span: "s0", offset: 0},
		{name: "plug:review", span: "s1", offset: 10},
		{dir: "review-dir", span: "s2", offset: 20},
		{name: "build", span: "s3", offset: 30},
		{dir: "build", span: "s4", offset: 40},
	}}
	if got, want := idx.used(tr, 0), []SessionSkill{{"review", "s2"}, {"build", "s3"}}; !slices.Equal(got, want) {
		t.Fatalf("used %+v, want %+v", got, want)
	}
	if got, want := idx.used(tr, 25), []SessionSkill{{"build", "s3"}}; !slices.Equal(got, want) {
		t.Fatalf("used from 25 %+v, want %+v", got, want)
	}
	tr.cwds = nil
	if got, want := idx.used(tr, 0), []SessionSkill{{"review", "s2"}}; !slices.Equal(got, want) {
		t.Fatalf("used without the project %+v, want %+v", got, want)
	}
}

// TestSkillsWalkStopsBelowHome: a .git in the home directory does not make
// the user-level skill directories project ones, and the home directory as
// the project has no project skills.
func TestSkillsWalkStopsBelowHome(t *testing.T) {
	home := t.TempDir()
	mkdir(t, filepath.Join(home, ".git"))
	putSkill(t, filepath.Join(home, ".claude", "skills"), "mine", skillMD("mine"))
	proj := mkdir(t, filepath.Join(home, "proj"))
	putSkill(t, filepath.Join(proj, ".claude", "skills"), "local", skillMD("local"))
	env := Env{Home: home, SkillDirs: skillDirsOf(home)}
	for _, tt := range []struct {
		project string
		want    []string
	}{
		{proj, []string{"project local"}},
		{home, nil},
		{filepath.Dir(home), nil},
	} {
		inv, err := Skills(env, SkillsRequest{Projects: []string{tt.project}})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range inv.Skills {
			got = append(got, s.Scope+" "+s.Name)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("project %s: %q, want %q", tt.project, got, tt.want)
		}
	}
}

// TestSkillsProjectPolicy: a project the [collect] policy denies or
// disables is not read; a problem names the reason.
func TestSkillsProjectPolicy(t *testing.T) {
	home := t.TempDir()
	proj := mkdir(t, filepath.Join(t.TempDir(), "proj"))
	putSkill(t, filepath.Join(proj, ".claude", "skills"), "build", skillMD("build"))
	env := Env{Home: home, SkillDirs: skillDirsOf(home)}
	env.Policy.DenyPaths = []string{proj}
	inv, err := Skills(env, SkillsRequest{Projects: []string{proj}})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Skills) != 0 || len(inv.Problems) != 1 || inv.Problems[0].Path != proj || !strings.Contains(inv.Problems[0].Reason, collect.ReasonDenyPaths) {
		t.Fatalf("denied project: %+v", inv)
	}
	env.Policy = collect.Policy{Disabled: true}
	if inv, err = Skills(env, SkillsRequest{Projects: []string{proj}}); err != nil || len(inv.Skills) != 0 || len(inv.Problems) != 1 || !strings.Contains(inv.Problems[0].Reason, StateDisabled) {
		t.Fatalf("disabled: %+v, %v", inv, err)
	}
}

// TestListProjectsRefused: a denied (before the read) and a disabled
// (after it) session inside the project are listed with their state and
// nothing read from them; a session outside is not listed.
func TestListProjectsRefused(t *testing.T) {
	w := newWorld(t)
	home := w.home
	putSkill(t, filepath.Join(home, ".agents", "skills"), "review", skillMD("review"))
	base := t.TempDir()
	proj := mkdir(t, filepath.Join(base, "proj"))
	denied := mkdir(t, filepath.Join(proj, "denied"))
	off := mkdir(t, filepath.Join(proj, "off"))
	mkdir(t, filepath.Join(off, ".git"))
	if err := os.WriteFile(filepath.Join(off, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(off, collect.RepoFile), []byte("[collect]\ndisabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := mkdir(t, filepath.Join(base, "outside"))
	skill := `{"type":"assistant","uuid":"a-s","timestamp":"2026-10-08T07:41:00.000Z","cwd":"{{CWD}}","message":{"role":"assistant","content":[{"type":"tool_use","id":"t-s","name":"Skill","input":{"skill":"review"}}]}}` + "\n"
	for sid, cwd := range map[string]string{"denied": denied, "off": off, "outside": outside} {
		w.put(cwd, sid, fixture(t, "basic.jsonl", sid, cwd)+strings.ReplaceAll(skill, "{{CWD}}", cwd))
	}
	w.pol.DenyPaths = []string{denied}
	env := w.env()
	env.SkillDirs = skillDirsOf(home)
	l, err := List(w.ctx, w.db, env, ListOptions{Projects: []string{proj}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Session{}
	for _, s := range l.Sessions {
		got[s.SessionID] = s
	}
	if len(got) != 2 || got["denied"].State != StateDenied || got["off"].State != StateDisabled {
		t.Fatalf("listing: %+v", l.Sessions)
	}
	for _, s := range got {
		if s.Counts != nil || s.SkillsUsed != nil || s.Cwds != nil {
			t.Errorf("%s shows what was read: %+v", s.SessionID, s)
		}
	}
	if l, err = List(w.ctx, w.db, env, ListOptions{Projects: []string{outside}}); err != nil || len(l.Sessions) != 1 || l.Sessions[0].SkillsUsed[0].Name != "review" {
		t.Fatalf("outside: %+v, %v", l.Sessions, err)
	}
}

// TestDigestSkillsFollowTheCut: a skill first used past the cut of a
// truncated digest is left out, and the next incremental digest reports
// it.
func TestDigestSkillsFollowTheCut(t *testing.T) {
	w := newWorld(t)
	putSkill(t, filepath.Join(w.home, ".agents", "skills"), "early", skillMD("early"))
	putSkill(t, filepath.Join(w.home, ".agents", "skills"), "late", skillMD("late"))
	var b strings.Builder
	line := func(s string) { b.WriteString(strings.ReplaceAll(s, "{{CWD}}", defaultCwd) + "\n") }
	use := func(n int, name string) {
		line(fmt.Sprintf(`{"type":"assistant","uuid":"s-%d","timestamp":"2026-10-08T07:00:00.000Z","cwd":"{{CWD}}","message":{"role":"assistant","content":[{"type":"tool_use","id":"ts-%d","name":"Skill","input":{"skill":%q}}]}}`, n, n, name))
	}
	use(0, "early")
	pad := strings.Repeat("x", 300)
	for i := range 200 {
		line(fmt.Sprintf(`{"type":"assistant","uuid":"a-%d","timestamp":"2026-10-08T07:00:01.000Z","cwd":"{{CWD}}","message":{"role":"assistant","content":[{"type":"tool_use","id":"t-%d","name":"Bash","input":{"command":"make step-%d"}}]}}`, i, i, i))
		line(fmt.Sprintf(`{"type":"user","uuid":"r-%d","timestamp":"2026-10-08T07:00:02.000Z","cwd":"{{CWD}}","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-%d","is_error":true,"content":"Exit code 1\n%s"}]}}`, i, i, pad))
	}
	use(1, "late")
	w.put(defaultCwd, "s1", b.String())
	env := w.env()
	env.SkillDirs = skillDirsOf(w.home)
	digest := func() SessionDigest {
		out, err := Digest(w.ctx, w.db, env, DigestRequest{Refs: []string{Ref(HarnessClaudeCode, "s1")}})
		if err != nil || len(out.Sessions) != 1 {
			t.Fatalf("digest: %+v, %v", out, err)
		}
		return out.Sessions[0]
	}
	first := digest()
	if !first.Truncated || !slices.Equal(first.SkillsUsed, []SessionSkill{{"early", "s-0"}}) {
		t.Fatalf("truncated digest: truncated %v, skills %+v", first.Truncated, first.SkillsUsed)
	}
	w.mark("s1")
	if next := digest(); next.FromOffset == 0 || !slices.Equal(next.SkillsUsed, []SessionSkill{{"late", "s-1"}}) {
		t.Fatalf("next digest from %d: truncated %v, skills %+v", next.FromOffset, next.Truncated, next.SkillsUsed)
	}
}
