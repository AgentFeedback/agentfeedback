package report

import (
	"math"
	"slices"
	"strings"
)

// InsufficientEvidence is the label every rubric allows when a session does
// not show enough to judge it; it is left out of the rubric's mean.
const InsufficientEvidence = "insufficient_evidence"

// NoGrade is the grade of a report in which no rubric has a judged session.
const NoGrade = "n/a"

// Rubric is one graded dimension: its name, its weight in the overall score
// and the score of each of its labels.
type Rubric struct {
	Name   string
	Weight float64
	Labels []LabelScore
}

// LabelScore is one label of a rubric and its score.
type LabelScore struct {
	Label string
	Score float64
}

// Rubrics are the four rubrics in report order. The weights sum to 1.
var Rubrics = []Rubric{
	{"efficiency", 0.30, []LabelScore{{"efficient", 1}, {"minor_detours", 0.67}, {"major_detours", 0.33}, {"unproductive", 0}}},
	{"code_quality", 0.25, []LabelScore{{"sound", 1}, {"minor_issues", 0.5}, {"unsound", 0}}},
	{"procedure_compliance", 0.25, []LabelScore{{"followed", 1}, {"minor_lapse", 0.5}, {"breached", 0}}},
	{"verbosity", 0.20, []LabelScore{{"proportionate", 1}, {"uneven", 0.5}, {"disproportionate", 0}}},
}

// letters are the grade thresholds, highest first; below the last is F.
var letters = []struct {
	min    float64
	letter string
}{{0.90, "A"}, {0.80, "B"}, {0.70, "C"}, {0.60, "D"}}

// Scores is what the binary computes from a valid report.
type Scores struct {
	// Grade is A to F, or NoGrade when no rubric has a judged session.
	Grade string `json:"grade"`
	// Score is the weighted mean, rounded to four decimals; nil with NoGrade.
	Score    *float64      `json:"score"`
	Rubrics  []RubricScore `json:"rubrics"`
	Coverage Coverage      `json:"coverage"`
	// UsedSkills counts, per skill, the sessions whose skills_used names it,
	// sorted by name.
	UsedSkills []UsedSkill `json:"used_skills"`
	// UnusedSkills are the found skills no session used, sorted.
	UnusedSkills []string `json:"unused_skills"`
}

// RubricScore is one rubric's part of the grade.
type RubricScore struct {
	Name string `json:"name"`
	// Weight is the effective weight: the rubric's weight rescaled over the
	// rubrics that have a judged session, 0 for one that has none.
	Weight float64 `json:"weight"`
	// Score is the mean label score over the judged sessions, rounded to
	// four decimals; nil when none was judged.
	Score        *float64       `json:"score"`
	Judged       int            `json:"judged"`
	Insufficient int            `json:"insufficient"`
	Labels       map[string]int `json:"labels"`
}

// Coverage is how many sessions used an installed skill. It is shown beside
// the grade and never moves it.
type Coverage struct {
	WithSkills int `json:"with_skills"`
	Sessions   int `json:"sessions"`
	// Ratio is WithSkills/Sessions rounded to four decimals; nil with no
	// session.
	Ratio *float64 `json:"ratio"`
}

// Grade computes the scores of a valid report: per rubric the mean label
// score over the sessions not labelled insufficient_evidence, a rubric
// without such a session dropped and the other weights rescaled to sum to 1,
// no curve.
func Grade(r Report) Scores {
	out := Scores{Rubrics: make([]RubricScore, 0, len(Rubrics)), UsedSkills: []UsedSkill{}, UnusedSkills: []string{}}
	means := make([]float64, len(Rubrics))
	total := 0.0
	for i, rb := range Rubrics {
		rs := RubricScore{Name: rb.Name, Labels: map[string]int{}}
		sum := 0.0
		for _, s := range r.Sessions {
			l := labelOf(s.Labels, rb.Name)
			rs.Labels[l]++
			if l == InsufficientEvidence {
				rs.Insufficient++

				continue
			}
			for _, ls := range rb.Labels {
				if ls.Label == l {
					sum += ls.Score
				}
			}
			rs.Judged++
		}
		if rs.Judged > 0 {
			means[i] = sum / float64(rs.Judged)
			rs.Score = ptr(round4(means[i]))
			total += rb.Weight
		}
		out.Rubrics = append(out.Rubrics, rs)
	}
	if total > 0 {
		score := 0.0
		for i, rb := range Rubrics {
			if out.Rubrics[i].Score == nil {
				continue
			}
			w := rb.Weight / total
			out.Rubrics[i].Weight = w
			score += w * means[i]
		}
		// Rounding before the comparison keeps a score of exactly 0.9 an A
		// whatever the float sum came to.
		score = round4(score)
		out.Score = &score
		out.Grade = "F"
		for _, l := range letters {
			if score >= l.min {
				out.Grade = l.letter

				break
			}
		}
	} else {
		out.Grade = NoGrade
	}

	used := skillUses(r.Sessions)
	for _, s := range r.Sessions {
		if len(s.SkillsUsed) > 0 {
			out.Coverage.WithSkills++
		}
	}
	for n, c := range used {
		out.UsedSkills = append(out.UsedSkills, UsedSkill{Name: n, Sessions: Count(c)})
	}
	slices.SortFunc(out.UsedSkills, func(a, b UsedSkill) int { return strings.Compare(a.Name, b.Name) })
	out.Coverage.Sessions = len(r.Sessions)
	if out.Coverage.Sessions > 0 {
		out.Coverage.Ratio = ptr(round4(float64(out.Coverage.WithSkills) / float64(out.Coverage.Sessions)))
	}
	for _, f := range r.Skills.Found {
		if used[f.Name] == 0 && !slices.Contains(out.UnusedSkills, f.Name) {
			out.UnusedSkills = append(out.UnusedSkills, f.Name)
		}
	}
	slices.Sort(out.UnusedSkills)

	return out
}

func labelOf(l Labels, rubric string) string {
	switch rubric {
	case "efficiency":
		return l.Efficiency.Label
	case "code_quality":
		return l.CodeQuality.Label
	case "procedure_compliance":
		return l.ProcedureCompliance.Label
	case "verbosity":
		return l.Verbosity.Label
	}

	return ""
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func ptr(v float64) *float64 { return &v }
