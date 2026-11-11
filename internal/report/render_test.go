package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var update = flag.Bool("update", false, "rewrite testdata/example.golden.html")

// fakeGH has a known secret format (pkg/scrub's github_token) and is built
// from repeated characters, so it is no real credential.
var fakeGH = "ghp_" + strings.Repeat("a", 36)

func render(t *testing.T, raw []byte) string {
	t.Helper()
	r, probs, err := Check(raw)
	if err != nil || len(probs) > 0 {
		t.Fatalf("check: %v %v", err, probs)
	}
	var buf bytes.Buffer
	if err := Render(&buf, r); err != nil {
		t.Fatal(err)
	}

	return buf.String()
}

// elements returns every element named tag in the parsed page.
func elements(t *testing.T, page, tag string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var out []*html.Node
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
	}

	return out
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}

	return "", false
}

func text(n *html.Node) string {
	var b strings.Builder
	for c := range n.Descendants() {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}

	return b.String()
}

func TestRender_Golden(t *testing.T) {
	page := render(t, examples(t)[0])
	path := filepath.Join("testdata", "example.golden.html")
	if *update {
		if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if page != string(want) {
		t.Fatalf("the page differs from %s; rerun with -update and review the diff", path)
	}
}

// The page is one file: styles and script inline, nothing it would load,
// no URL, and the CSP admits exactly the inline style and script.
func TestRender_SelfContained(t *testing.T) {
	page := render(t, examples(t)[0])
	for _, bad := range []string{"http://", "https://", "<link", "<img", "<iframe"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page contains %q", bad)
		}
	}
	if !strings.Contains(page, "agentfeedback.dev") {
		t.Error("the page does not name agentfeedback.dev")
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		for _, key := range []string{"src", "href", "action", "srcset", "style"} {
			if _, ok := attr(n, key); ok {
				t.Errorf("<%s> has a %s attribute", n.Data, key)
			}
		}
	}
	styles, scripts := elements(t, page, "style"), elements(t, page, "script")
	if len(styles) != 1 || len(scripts) != 2 {
		t.Fatalf("want one style and two script elements, got %d and %d", len(styles), len(scripts))
	}
	if typ, _ := attr(scripts[0], "type"); typ != "application/json" {
		t.Fatalf("the first script is %q, want the application/json data block", typ)
	}
	var csp string
	for _, m := range elements(t, page, "meta") {
		if v, _ := attr(m, "http-equiv"); v == "Content-Security-Policy" {
			csp, _ = attr(m, "content")
		}
	}
	for _, want := range []string{"default-src 'none'", "style-src " + hash(text(styles[0])), "script-src " + hash(text(scripts[1]))} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
}

// Neither a session ref nor a project path reaches the page.
func TestRender_NoRefsOrPaths(t *testing.T) {
	raw := examples(t)[0]
	page := render(t, raw)
	r, _, _ := Check(raw)
	for _, s := range r.Sessions {
		if strings.Contains(page, s.Ref) {
			t.Errorf("the page shows session ref %s", s.Ref)
		}
	}
	for _, p := range r.Scope.Projects {
		if strings.Contains(page, p) {
			t.Errorf("the page shows project path %s", p)
		}
	}
}

func TestRender_EscapesMarkup(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	page := render(t, mutated(t, func(doc map[string]any) {
		doc["top_findings"].([]any)[0].(map[string]any)["text"] = "A finding " + evil
		doc["suggestions"].([]any)[0].(map[string]any)["diff"] = "+</pre>" + evil + "\n"
		doc["suggestions"].([]any)[0].(map[string]any)["surface"] = `"><img src=x>`
	}))
	if n := len(elements(t, page, "script")); n != 2 {
		t.Fatalf("want the two page scripts, got %d script elements", n)
	}
	if n := len(elements(t, page, "img")); n != 0 {
		t.Fatalf("an img element was injected")
	}
	var shown bool
	for _, li := range elements(t, page, "li") {
		if strings.Contains(text(li), "A finding "+evil) {
			shown = true
		}
	}
	if !shown {
		t.Error("the finding is not shown as text")
	}
	pres := elements(t, page, "pre")
	if len(pres) != 1 || !strings.Contains(text(pres[0]), evil) {
		t.Errorf("the diff is not shown as text in one pre")
	}
	var data card
	if err := json.Unmarshal([]byte(text(elements(t, page, "script")[0])), &data); err != nil {
		t.Fatalf("card data: %v", err)
	}
	if data.Findings[0] != "A finding "+evil {
		t.Errorf("card finding %q", data.Findings[0])
	}
}

func TestRender_ScrubsSecrets(t *testing.T) {
	page := render(t, mutated(t, func(doc map[string]any) {
		doc["top_findings"].([]any)[0].(map[string]any)["text"] = "The token " + fakeGH + " was pasted into a prompt."
		doc["suggestions"].([]any)[0].(map[string]any)["diff"] = "+token = " + fakeGH + "\n"
	}))
	if strings.Contains(page, fakeGH) {
		t.Fatal("the secret reached the page")
	}
	if !strings.Contains(page, "The token [REDACTED:github_token] was pasted") {
		t.Fatal("the finding is not redacted in place")
	}
}

func TestRender_NoGrade(t *testing.T) {
	page := render(t, mutated(t, func(doc map[string]any) {
		for i := range doc["sessions"].([]any) {
			for _, l := range labelsOf(doc, i) {
				l.(map[string]any)["label"] = InsufficientEvidence
			}
		}
	}))
	if !strings.Contains(page, `<div class="letter">n/a</div>`) || !strings.Contains(page, `"score":null`) {
		t.Fatal("an ungraded report does not render as n/a")
	}
}

// The card script makes no request: the only URL it may produce is the
// object URL of the PNG it draws.
func TestCardScriptMakesNoRequest(t *testing.T) {
	for _, bad := range []string{"fetch(", "XMLHttpRequest", "WebSocket", "sendBeacon", "import(", "EventSource",
		".src", "src=", "location", "window.open", "http", "new Image", "innerHTML", "eval(", "Function("} {
		if strings.Contains(cardJS, bad) {
			t.Errorf("card.js contains %q", bad)
		}
	}
	hrefs := regexp.MustCompile(`href[^\n]*`).FindAllString(cardJS, -1)
	if len(hrefs) != 1 || hrefs[0] != "href = blobURL;" {
		t.Errorf("card.js assigns href other than the blob URL: %q", hrefs)
	}
	if !strings.Contains(cardJS, "var blobURL = URL.createObjectURL(blob);") {
		t.Error("blobURL is not the canvas blob's object URL")
	}
	if !strings.Contains(cardJS, `"agentfeedback.dev"`) {
		t.Error("the card does not name agentfeedback.dev")
	}
}

// The CSP hashes the bytes the page inlines, which are LF-only whatever
// line ends the checkout gave the assets.
func TestAssetsLF(t *testing.T) {
	if got := lf("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("lf: %q", got)
	}
	if strings.Contains(pageCSS, "\r") || strings.Contains(cardJS, "\r") {
		t.Fatal("an inlined asset holds a CR")
	}
}
