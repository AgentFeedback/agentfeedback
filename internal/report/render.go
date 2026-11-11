package report

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

//go:embed assets/page.html assets/page.css assets/card.js
var assets embed.FS

var (
	pageCSS  = mustAsset("assets/page.css")
	cardJS   = mustAsset("assets/card.js")
	pageTmpl = template.Must(template.New("page.html").ParseFS(assets, "assets/page.html"))
)

// mustAsset reads an asset with its line ends normalised to LF. The page
// inlines these exact bytes and the CSP hashes them; a browser hashes the
// element's text after HTML parsing has turned CR LF and CR into LF, so a
// CR left in by a CRLF checkout would make the hash refuse the asset.
func mustAsset(name string) string {
	b, err := assets.ReadFile(name)
	if err != nil {
		panic("report: " + err.Error())
	}

	return lf(string(b))
}

func lf(s string) string { return strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s) }

// rubricTitles are the rubric names as the page and the card show them.
var rubricTitles = map[string]string{
	"efficiency":           "Efficiency",
	"code_quality":         "Code quality",
	"procedure_compliance": "Procedure compliance",
	"verbosity":            "Verbosity",
}

type page struct {
	CSP            string
	CSS            template.CSS
	JS             template.JS
	Grade          string
	Score          string
	GeneratedAt    string
	Scope          pageScope
	RulesFromLog   int
	RulesFromFiles int
	Rubrics        []pageRubric
	Coverage       string
	UsedSkills     []UsedSkill
	UnusedSkills   []string
	Findings       []pageFinding
	Suggestions    []pageSuggestion
	Card           card
}

// pageScope shows counts, harnesses and the window only: no project path.
type pageScope struct {
	Projects  int
	Harnesses []string
	Since     string
	Scanned   int
	Sampled   int
}

type pageRubric struct {
	Title  string
	Score  string
	Value  float64
	Weight string
	Labels []labelCount
}

type labelCount struct {
	Label string
	Count int
}

// pageFinding shows how many sessions back a finding, never their refs.
type pageFinding struct {
	Text     string
	Sessions int
}

type pageSuggestion struct {
	Surface  string
	Change   string
	Evidence int
	FiledUID string
	Diff     string
}

// card is the share card's data, embedded as JSON for the page's script.
type card struct {
	Grade    string       `json:"grade"`
	Score    *string      `json:"score"`
	Rubrics  []cardRubric `json:"rubrics"`
	Findings []string     `json:"findings"`
}

type cardRubric struct {
	Title string   `json:"title"`
	Score *float64 `json:"score"`
}

// Render writes the page of a valid report: one HTML file with its styles
// and script inline, the grade computed by Grade, every string from the
// report scrubbed of known secret formats and escaped by html/template. The
// page shows no session ref and no project path; a finding's text is shown
// as written.
func Render(w io.Writer, r Report) error {
	s := Grade(r)
	p := page{
		// The hashes admit exactly the inline style and script; default-src
		// 'none' leaves the page no way to load or send anything.
		CSP: "default-src 'none'; style-src " + hash(pageCSS) + "; script-src " + hash(cardJS) +
			"; base-uri 'none'; form-action 'none'",
		CSS:          template.CSS(pageCSS),
		JS:           template.JS(cardJS),
		Grade:        s.Grade,
		GeneratedAt:  clean(r.GeneratedAt),
		UnusedSkills: cleanAll(s.UnusedSkills),
		Scope: pageScope{
			Projects: len(r.Scope.Projects), Harnesses: cleanAll(r.Scope.Harnesses), Since: clean(r.Scope.Since),
			Scanned: int(r.Scope.SessionsScanned), Sampled: len(r.Sessions),
		},
		Card: card{Grade: s.Grade, Rubrics: []cardRubric{}, Findings: []string{}},
	}
	if s.Score != nil {
		p.Score = num(*s.Score)
		p.Card.Score = &p.Score
	}
	for _, sess := range r.Sessions {
		if sess.RulesSource == "session-log" {
			p.RulesFromLog++
		} else {
			p.RulesFromFiles++
		}
	}
	for i, rs := range s.Rubrics {
		pr := pageRubric{Title: rubricTitles[rs.Name], Weight: strconv.FormatFloat(rs.Weight*100, 'f', 1, 64) + "%"}
		if rs.Score != nil {
			pr.Score, pr.Value = num(*rs.Score), *rs.Score
		}
		for _, ls := range Rubrics[i].Labels {
			pr.Labels = append(pr.Labels, labelCount{ls.Label, rs.Labels[ls.Label]})
		}
		pr.Labels = append(pr.Labels, labelCount{InsufficientEvidence, rs.Insufficient})
		p.Rubrics = append(p.Rubrics, pr)
		p.Card.Rubrics = append(p.Card.Rubrics, cardRubric{Title: pr.Title, Score: rs.Score})
	}
	p.Coverage = fmt.Sprintf("%d of %d sampled session(s) used an installed skill. Coverage is shown, not graded.", s.Coverage.WithSkills, s.Coverage.Sessions)
	for _, u := range s.UsedSkills {
		p.UsedSkills = append(p.UsedSkills, UsedSkill{Name: clean(u.Name), Sessions: u.Sessions})
	}
	for _, f := range r.TopFindings {
		t := clean(f.Text)
		p.Findings = append(p.Findings, pageFinding{Text: t, Sessions: len(f.Refs)})
		p.Card.Findings = append(p.Card.Findings, t)
	}
	for _, sg := range r.Suggestions {
		p.Suggestions = append(p.Suggestions, pageSuggestion{
			Surface: clean(sg.Surface), Change: clean(sg.Change), Evidence: len(sg.Evidence),
			FiledUID: clean(sg.FiledUID), Diff: clean(sg.Diff),
		})
	}

	return pageTmpl.Execute(w, p)
}

func clean(s string) string {
	out, _ := scrub.String(s)

	return out
}

func cleanAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = clean(s)
	}

	return out
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}
