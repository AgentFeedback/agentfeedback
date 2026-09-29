package skillgen

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
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
	if got := []string{FormPrompt, FormMCP}; !CarriesServer(got[0]) || !CarriesServer(got[1]) || CarriesServer(FormSkillMD) {
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
		own := Channel(form)
		for _, f := range frags {
			ownText := f.text(own)
			for _, other := range []string{ChannelCLI, ChannelHTTP, ChannelMCP} {
				if other == own {
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
	good := `{"name":"n","title":"T","description":"d","version":"1.0","fragments":["a"]}`
	frag := &fstest.MapFile{Data: []byte("## A\n")}
	for name, files := range map[string]fstest.MapFS{
		"duplicate id":  {"source/skill.json": {Data: []byte(strings.Replace(good, `["a"]`, `["a","a"]`, 1))}, "source/a.md": frag},
		"unlisted file": {"source/skill.json": {Data: []byte(good)}, "source/a.md": frag, "source/b.md": frag},
		"missing file":  {"source/skill.json": {Data: []byte(good)}},
		"unknown key":   {"source/skill.json": {Data: []byte(strings.Replace(good, `"name"`, `"extra":1,"name"`, 1))}, "source/a.md": frag},
		"no version":    {"source/skill.json": {Data: []byte(strings.Replace(good, `"1.0"`, `""`, 1))}, "source/a.md": frag},
	} {
		if _, _, err := load(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, frags, err := load(fstest.MapFS{"source/skill.json": {Data: []byte(good)}, "source/a.md": frag}); err != nil || len(frags) != 1 {
		t.Fatalf("good source: %v", err)
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
