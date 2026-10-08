package skillgen

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentfeedback/agentfeedback/v4"
)

const testServer = "https://feedback.example.com"

func render(t *testing.T, form, server string) string {
	t.Helper()
	out, err := Render(form, server)
	if err != nil {
		t.Fatalf("Render(%q, %q): %v", form, server, err)
	}

	return string(out)
}

func TestSourceParses(t *testing.T) {
	m, frags, err := Source()
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != len(m.Fragments) {
		t.Fatalf("%d fragments, skill.json lists %d", len(frags), len(m.Fragments))
	}
	for _, f := range frags {
		for _, c := range []string{ChannelCLI, ChannelHTTP, ChannelMCP} {
			if strings.Count(f.text(c), "\n") < 2 {
				t.Errorf("%s has no content for channel %s", f.ID, c)
			}
		}
	}
}

// paragraphs splits text at blank lines.
func paragraphs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// Every fragment's shared text appears in every form; headings may sit one
// level deeper (the agents-md snippet nests under its own heading).
func TestEveryFragmentInEveryForm(t *testing.T) {
	_, frags, err := Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range Forms() {
		for _, server := range []string{"", testServer} {
			out := render(t, form, server)
			shown := serverPlaceholder
			if server != "" && CarriesServer(form) {
				shown = server
			}
			for _, f := range frags {
				for _, p := range paragraphs(strings.ReplaceAll(f.Shared, "{{server}}", shown)) {
					if heading, ok := strings.CutPrefix(p, "#"); ok {
						heading = strings.TrimLeft(heading, "#")
						if !regexp.MustCompile(`(?m)^#+` + regexp.QuoteMeta(heading) + `$`).MatchString(out) {
							t.Errorf("%s (server %q): heading %q of %s missing", form, server, p, f.ID)
						}

						continue
					}
					if !strings.Contains(out, p) {
						t.Errorf("%s (server %q): text of %s missing:\n%s", form, server, f.ID, p)
					}
				}
			}
			if strings.Contains(out, "{{") || strings.Contains(out, "<!-- only:") || strings.Contains(out, "<!-- end -->") {
				t.Errorf("%s: source directive left in the output", form)
			}
			if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
				t.Errorf("%s: output must end with exactly one newline", form)
			}
		}
	}
}

func TestCheckedInSkillMDIsTheRender(t *testing.T) {
	want, err := os.ReadFile("../../skills/agentfeedback/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, FormSkillMD, ""); got != string(want) {
		t.Fatal("skills/agentfeedback/SKILL.md is not the current render; run just skills")
	}
}

var anyURL = regexp.MustCompile(`[a-z][a-z0-9+.-]*://`)

// No form names a URL unless a server is given, and a server changes only
// the forms that carry one.
func TestServerOnlyInTheFormsThatCarryIt(t *testing.T) {
	for _, form := range Forms() {
		plain, with := render(t, form, ""), render(t, form, testServer+"/")
		if anyURL.MatchString(plain) {
			t.Errorf("%s without a server contains a URL: %q", form, anyURL.FindString(plain))
		}
		if CarriesServer(form) {
			if plain == with || !strings.Contains(with, testServer) || strings.Contains(with, testServer+"//") {
				t.Errorf("%s: --server not applied as the base URL", form)
			}
		} else if plain != with {
			t.Errorf("%s changed with a server", form)
		} else if ignored := render(t, form, "ftp://not-a-server"); ignored != plain {
			t.Errorf("%s: an unusable --server is not ignored", form)
		}
	}
	if !CarriesServer(FormPrompt) || !CarriesServer(FormPromptPowerShell) || !CarriesServer(FormMCP) || CarriesServer(FormSkillMD) {
		t.Fatal("CarriesServer")
	}
}

// The prompt form is enough for an agent that has only HTTP.
func TestPromptIsHTTPOnly(t *testing.T) {
	out := render(t, FormPrompt, testServer)
	for _, want := range []string{
		"POST " + testServer + "/api/v1/submissions",
		"Authorization: Bearer <API key>",
		"X-Api-Key: <API key>",
		"Content-Type: application/json",
		`{"kind": "friction", "summary": "<one line>"}`,
		"`201 Created`",
		"`200 OK`",
		"`400`", "`401`", "`409`", "`413`", "`429`", "`5xx`",
		"Retry-After",
		"submission.id",
		"warnings",
		"GET " + testServer + "/api/v1/meta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, banned := range []string{"agentfeedback submit", "agentfeedback doctor", "submit_feedback"} {
		if strings.Contains(out, banned) {
			t.Errorf("prompt assumes tooling: %q", banned)
		}
	}
}

// The prompt form writes the request for curl, the prompt-powershell form
// for PowerShell; the forms that run the binary write neither.
func TestPromptRecipes(t *testing.T) {
	const curl, ps = "curl -sS -X POST", "Invoke-RestMethod -Method Post"
	for form, want := range map[string][2]bool{
		FormPrompt:           {true, false},
		FormPromptPowerShell: {false, true},
		FormSkillMD:          {false, false},
		FormAgentsMD:         {false, false},
		FormCursor:           {false, false},
		FormMCP:              {false, false},
	} {
		out := render(t, form, testServer)
		if strings.Contains(out, curl) != want[0] || strings.Contains(out, ps) != want[1] {
			t.Errorf("%s: curl %v, PowerShell %v; want %v", form, strings.Contains(out, curl), strings.Contains(out, ps), want)
		}
	}
	if got, want := Channels(FormPromptPowerShell), []string{ChannelHTTP, ChannelPowerShell}; !slices.Equal(got, want) {
		t.Errorf("prompt-powershell channels %v, want %v", got, want)
	}
	if got, want := Channels(FormPrompt), []string{ChannelHTTP, ChannelCurl}; !slices.Equal(got, want) {
		t.Errorf("prompt channels %v, want %v", got, want)
	}
	if render(t, FormPrompt, "") == render(t, FormPromptPowerShell, "") {
		t.Error("prompt and prompt-powershell render the same")
	}
}

func TestChannelsTeachTheirOwnPath(t *testing.T) {
	for form, want := range map[string]string{
		FormSkillMD:  "agentfeedback submit friction",
		FormAgentsMD: "agentfeedback submit friction",
		FormCursor:   "agentfeedback submit friction",
		FormMCP:      "`submit_feedback`",
	} {
		out := render(t, form, "")
		if !strings.Contains(out, want) {
			t.Errorf("%s lacks %q", form, want)
		}
		if strings.Contains(out, "/api/v1/submissions") {
			t.Errorf("%s teaches the raw HTTP route", form)
		}
	}
}

var exampleCaption = regexp.MustCompile(`(?m)^([A-Z][a-z]+) agent\b`)

// Every example set pairs a coding case with a non-coding one.
func TestExamplesPairCodingWithNonCoding(t *testing.T) {
	for _, form := range Forms() {
		out := render(t, form, "")
		_, examples, ok := strings.Cut(out, "# Examples\n")
		if !ok {
			t.Fatalf("%s has no Examples section", form)
		}
		coding, other := 0, 0
		for _, m := range exampleCaption.FindAllStringSubmatch(examples, -1) {
			if m[1] == "Coding" {
				coding++
			} else {
				other++
			}
		}
		if coding == 0 || other == 0 {
			t.Errorf("%s: %d coding and %d non-coding examples", form, coding, other)
		}
	}
}

func TestFraming(t *testing.T) {
	skill := render(t, FormSkillMD, "")
	for _, want := range []string{"---\nname: agentfeedback\n", "\n  version: \"5.0\"\n---\n", "\n# AgentFeedback\n"} {
		if !strings.Contains(skill, want) {
			t.Errorf("skill-md lacks %q", want)
		}
	}
	agents := render(t, FormAgentsMD, "")
	if !strings.HasPrefix(agents, "<!-- agentfeedback:begin 5.0 -->\n## AgentFeedback\n") ||
		!strings.HasSuffix(agents, "\n<!-- agentfeedback:end -->\n") || strings.Contains(agents, "\n## When to file") ||
		!strings.Contains(agents, "\n### When to file\n") {
		t.Error("agents-md framing")
	}
	cursor := render(t, FormCursor, "")
	if !strings.HasPrefix(cursor, "---\ndescription: ") || !strings.Contains(cursor, "\nalwaysApply: false\n---\n") {
		t.Error("cursor framing")
	}
	if mcp := render(t, FormMCP, ""); strings.Contains(mcp, "HTTP API") {
		t.Error("mcp without a server names an HTTP API")
	}
	if a, b := render(t, FormPrompt, testServer), render(t, FormPrompt, testServer); a != b {
		t.Error("render is not deterministic")
	}
}

func TestRenderUnknownForm(t *testing.T) {
	for _, form := range []string{"", "SKILL-MD", "agent-plugin", "docs"} {
		if _, err := Render(form, ""); !errors.Is(err, ErrUnknownForm) {
			t.Errorf("%q: %v", form, err)
		}
	}
}

func TestNormalizeServer(t *testing.T) {
	for raw, want := range map[string]string{
		"https://feedback.example.com":         "https://feedback.example.com",
		"https://feedback.example.com/":        "https://feedback.example.com",
		"http://192.0.2.10:8090":               "http://192.0.2.10:8090",
		"http://[2001:db8::1]:8090/base/":      "http://[2001:db8::1]:8090/base",
		"https://feedback.example.com/a%20b//": "https://feedback.example.com/a%20b",
		"https://x/a\"b `id` c":                "https://x/a%22b%20%60id%60%20c",
	} {
		got, err := NormalizeServer(raw)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "feedback.example.com", "ftp://x", "https://", "https://u:p@x", "https://x?a=1", "https://x/#f",
		"https://x/$(id)", "http://x/a'b", "https://x/a;b", "https://x/a*b",
	} {
		var se *ServerError
		if _, err := NormalizeServer(raw); !errors.As(err, &se) {
			t.Errorf("%q accepted", raw)
		}
		if _, err := Render(FormPrompt, raw); raw != "" && !errors.As(err, &se) {
			t.Errorf("Render accepted %q", raw)
		}
	}
}

func TestParseFragmentErrors(t *testing.T) {
	for name, text := range map[string]string{
		"no heading":       "text\n",
		"level 1":          "# Title\n",
		"unclosed":         "## H\n<!-- only: cli -->\nx\n",
		"stray end":        "## H\n<!-- end -->\n",
		"nested":           "## H\n<!-- only: cli -->\n<!-- only: mcp -->\n<!-- end -->\n",
		"bad channel":      "## H\n<!-- only: cli sms -->\nx\n<!-- end -->\n",
		"other comment":    "## H\n<!-- note -->\n",
		"indented":         "## H\n  <!-- only: cli -->\nx\n<!-- end -->\n",
		"no channel":       "## H\n<!-- only:  -->\nx\n<!-- end -->\n",
		"heading in block": "<!-- only: cli -->\n## H\n<!-- end -->\n",
		"carriage return":  "## H\r\nx\n",
	} {
		if _, err := parseFragment("t", text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f, err := parseFragment("t", "## H\n\nshared\n<!-- only: cli http -->\nboth\n<!-- end -->\n<!-- only: mcp -->\nmcp\n<!-- end -->\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.Shared != "## H\n\nshared" || f.text(ChannelCLI) != "## H\n\nshared\nboth" || f.text(ChannelMCP) != "## H\n\nshared\nmcp" {
		t.Fatalf("%q %q %q", f.Shared, f.text(ChannelCLI), f.text(ChannelMCP))
	}
}

func TestYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		"MIT":            "MIT",
		"5.0":            `"5.0"`,
		"true":           `"true"`,
		"a: b":           `"a: b"`,
		`say "x"`:        `"say \"x\""`,
		"x #y":           `"x #y"`,
		"ends in space ": `"ends in space "`,
		"a <b> & c":      `"a <b> & c"`,
	} {
		if got := yamlString(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	if !bytes.Equal([]byte(yamlQuoted("5.0")), []byte(`"5.0"`)) {
		t.Error("yamlQuoted")
	}
}

// A block's text reaches only the forms of its channel.
func TestBlocksStayInTheirChannel(t *testing.T) {
	_, frags, err := Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range Forms() {
		out := render(t, form, "")
		own := Channels(form)
		for _, f := range frags {
			ownText := f.text(own...)
			for _, other := range channels {
				if slices.Contains(own, other) {
					continue
				}
				for _, p := range paragraphs(strings.ReplaceAll(f.text(other), "{{server}}", serverPlaceholder)) {
					if !strings.Contains(ownText, p) && strings.Contains(out, p) {
						t.Errorf("%s shows %s-only text of %s:\n%s", form, other, f.ID, p)
					}
				}
			}
		}
	}
}

func TestLoadRejectsABrokenSource(t *testing.T) {
	good := `{"name":"n","title":"T","description":"d","version":"1.0","rule":"r","project_rule":"p","fragments":["a"]}`
	frag := &fstest.MapFile{Data: []byte("## A\n")}
	for name, files := range map[string]fstest.MapFS{
		"duplicate id":  {"source/skill.json": {Data: []byte(strings.Replace(good, `["a"]`, `["a","a"]`, 1))}, "source/a.md": frag},
		"unlisted file": {"source/skill.json": {Data: []byte(good)}, "source/a.md": frag, "source/b.md": frag},
		"missing file":  {"source/skill.json": {Data: []byte(good)}},
		"unknown key":   {"source/skill.json": {Data: []byte(strings.Replace(good, `"name"`, `"extra":1,"name"`, 1))}, "source/a.md": frag},
		"no version":    {"source/skill.json": {Data: []byte(strings.Replace(good, `"1.0"`, `""`, 1))}, "source/a.md": frag},
		"no rule":       {"source/skill.json": {Data: []byte(strings.Replace(good, `"rule":"r",`, ``, 1))}, "source/a.md": frag},
	} {
		if _, _, err := load(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, frags, err := load(fstest.MapFS{"source/skill.json": {Data: []byte(good)}, "source/a.md": frag}); err != nil || len(frags) != 1 {
		t.Fatalf("good source: %v", err)
	}
}

// Prime is the cli channel only, without the file inbox fallback, and small
// enough for a session-start hook.
func TestPrime(t *testing.T) {
	out, err := Prime()
	if err != nil {
		t.Fatal(err)
	}
	m, frags, err := Source()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "# "+m.Title+"\n\n"+m.Description+"\n\n") {
		t.Errorf("prime does not start with the title and description:\n%.200s", got)
	}
	for _, f := range frags {
		cli := f.text(ChannelCLI)
		for _, p := range paragraphs(f.text(ChannelFile)) {
			if !strings.Contains(cli, p) && strings.Contains(got, p) {
				t.Errorf("prime shows file-only text of %s:\n%s", f.ID, p)
			}
		}
		for _, p := range paragraphs(cli) {
			if !strings.Contains(got, p) {
				t.Errorf("prime lacks cli text of %s:\n%s", f.ID, p)
			}
		}
	}
	if len(out) >= 8000 {
		t.Errorf("prime is %d bytes, want under 8000", len(out))
	}
	for name, s := range map[string]string{"rule": m.Rule, "project_rule": m.ProjectRule} {
		if !strings.Contains(s, "agentfeedback prime") {
			t.Errorf("%s does not name agentfeedback prime: %q", name, s)
		}
	}
}

func TestTidyAndDemoteLeaveCodeAlone(t *testing.T) {
	in := "## H  \n\n\n\ntext\n\n~~~sh\n# comment  \n\n\n\nx\n~~~\n\n   ```\n# also code\n   ```\n\n### Sub\n#not-a-heading\n\n"
	got := tidy(in)
	want := "## H\n\ntext\n\n~~~sh\n# comment  \n\n\n\nx\n~~~\n\n   ```\n# also code\n   ```\n\n### Sub\n#not-a-heading"
	if got != want {
		t.Fatalf("tidy:\n%q\nwant\n%q", got, want)
	}
	demoted := demote(got)
	for _, line := range []string{"### H", "#### Sub", "# comment  ", "# also code", "#not-a-heading"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + `$`).MatchString(demoted) {
			t.Errorf("demote: %q missing in\n%s", line, demoted)
		}
	}
}

func docs(t *testing.T) []File {
	t.Helper()
	files, err := Docs()
	if err != nil {
		t.Fatal(err)
	}

	return files
}

func embeddedReferences(t *testing.T) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(agentfeedback.References, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// Every embedded file is in the tree once, byte for byte, and listed once.
func TestDocsListsEveryReference(t *testing.T) {
	files := docs(t)
	if files[0].Path != "SKILL.md" {
		t.Fatalf("first file %s", files[0].Path)
	}
	skill := string(files[0].Data)
	byPath := map[string][]byte{}
	for _, f := range files[1:] {
		if _, dup := byPath[f.Path]; dup {
			t.Errorf("%s twice in the tree", f.Path)
		}
		byPath[f.Path] = f.Data
	}
	refs := embeddedReferences(t)
	if len(refs) == 0 || len(byPath) != len(refs) {
		t.Fatalf("%d reference files, %d embedded", len(byPath), len(refs))
	}
	for _, p := range refs {
		want, _ := fs.ReadFile(agentfeedback.References, p)
		if got, ok := byPath["references/"+p]; !ok || !bytes.Equal(got, want) {
			t.Errorf("references/%s missing or not the embedded bytes", p)
		}
		link := "- [references/" + p + "](references/" + p + "): "
		if n := strings.Count(skill, link); n != 1 {
			t.Errorf("SKILL.md lists %s %d times", p, n)
		}
	}
	for _, want := range []string{"docs/api.md", "docs/openapi.yaml", "schemas/envelope.v1.json", "schemas/kinds/friction.v1.json", "AGENT-INSTALL.md"} {
		if byPath["references/"+want] == nil {
			t.Errorf("references/%s missing", want)
		}
	}
	paths := make([]string, 0, len(files))
	for _, f := range files[1:] {
		paths = append(paths, f.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Errorf("tree not in path order: %v", paths)
	}
	if !strings.Contains(skill, "): AgentFeedback API (OpenAPI document)\n") {
		t.Error("the OpenAPI document is not described by its title")
	}
	if !strings.Contains(skill, "(references/schemas/kinds/friction.v1.json): JSON Schema: ") {
		t.Error("a schema is not described by its title")
	}
}

func TestDocsDeterministic(t *testing.T) {
	a, b := docs(t), docs(t)
	if len(a) != len(b) {
		t.Fatal("tree size differs")
	}
	for i := range a {
		if a[i].Path != b[i].Path || !bytes.Equal(a[i].Data, b[i].Data) {
			t.Fatalf("%s differs between two renders", a[i].Path)
		}
	}
}

func TestDocsFrontmatter(t *testing.T) {
	skill := string(docs(t)[0].Data)
	front, _, ok := strings.Cut(strings.TrimPrefix(skill, "---\n"), "\n---\n")
	if !strings.HasPrefix(skill, "---\n") || !ok {
		t.Fatalf("no frontmatter:\n%s", skill)
	}
	if !strings.Contains(front, "\nname: agentfeedback-docs\n") && !strings.HasPrefix(front, "name: agentfeedback-docs\n") {
		t.Errorf("frontmatter lacks the name:\n%s", front)
	}
	if !strings.Contains(front, "\ndescription: ") {
		t.Errorf("frontmatter lacks the description:\n%s", front)
	}
	if !strings.Contains(skill, "<!-- Generated by `agentfeedback skill render docs`; do not edit. -->") {
		t.Error("no generated marker")
	}
}

// The checked-in skills/agentfeedback-docs/ is the render, with nothing more.
func TestCheckedInDocsSkillIsTheRender(t *testing.T) {
	const dir = "../../skills/agentfeedback-docs"
	want := map[string][]byte{}
	for _, f := range docs(t) {
		want[f.Path] = f.Data
	}
	got := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		got[filepath.ToSlash(rel)] = data

		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, data := range want {
		if g, ok := got[p]; !ok || !bytes.Equal(g, data) {
			t.Errorf("skills/agentfeedback-docs/%s is missing or not the current render; run just skills", p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("skills/agentfeedback-docs/%s is not part of the render; run just skills", p)
		}
	}
}

func TestDocsRefusesAFileWithoutADescription(t *testing.T) {
	src := fstest.MapFS{"source/docs.json": {Data: []byte(`{"name":"n","title":"T","description":"d","version":"1.0"}`)}}
	for name, refs := range map[string]fstest.MapFS{
		"markdown without a heading": {"docs/x.md": {Data: []byte("text\n```\n# in code\n```\n")}},
		"schema without a title":     {"schemas/x.json": {Data: []byte(`{"type":"object"}`)}},
		"openapi without a title":    {"docs/openapi.yaml": {Data: []byte("openapi: 3.1.1\ninfo: {}\n")}},
		"unknown file type":          {"docs/x.txt": {Data: []byte("x\n")}},
	} {
		if _, err := renderDocs(src, refs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, s := range map[string]string{
		"unknown key": `{"name":"n","title":"T","description":"d","version":"1.0","extra":1}`,
		"no version":  `{"name":"n","title":"T","description":"d"}`,
	} {
		if _, err := renderDocs(fstest.MapFS{"source/docs.json": {Data: []byte(s)}}, fstest.MapFS{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
