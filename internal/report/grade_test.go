package report

import (
	"math"
	"slices"
	"testing"
)

// sessionWith labels every rubric of a session through pick, which gets the
// rubric and returns its label.
func sessionWith(pick func(rb Rubric) string, skills ...string) Session {
	l := func(name string) Label {
		for _, rb := range Rubrics {
			if rb.Name == name {
				return Label{Label: pick(rb), Reason: "because"}
			}
		}

		return Label{}
	}

	return Session{
		Ref: "claude-code:s", RulesSource: "session-log", SkillsUsed: skills,
		Labels: Labels{l("efficiency"), l("code_quality"), l("procedure_compliance"), l("verbosity")},
	}
}

func best(rb Rubric) string  { return rb.Labels[0].Label }
func worst(rb Rubric) string { return rb.Labels[len(rb.Labels)-1].Label }

func reportOf(sessions ...Session) Report { return Report{Version: 1, Sessions: sessions} }

func TestGrade_AllWorstIsF(t *testing.T) {
	s := Grade(reportOf(sessionWith(worst), sessionWith(worst)))
	if s.Grade != "F" || s.Score == nil || *s.Score != 0 {
		t.Fatalf("want F with score 0, got %s %v", s.Grade, s.Score)
	}
}

func TestGrade_AllBestIsA(t *testing.T) {
	s := Grade(reportOf(sessionWith(best), sessionWith(best)))
	if s.Grade != "A" || s.Score == nil || *s.Score != 1 {
		t.Fatalf("want A with score 1, got %s %v", s.Grade, s.Score)
	}
}

func TestGrade_Thresholds(t *testing.T) {
	// efficiency, code_quality and procedure_compliance best, verbosity at
	// the given label: 0.30 + 0.25 + 0.25 + 0.20 * score.
	tests := []struct {
		verbosity string
		want      string
		score     float64
	}{
		{"proportionate", "A", 1},
		{"uneven", "A", 0.9},
		{"disproportionate", "B", 0.8},
	}
	for _, tt := range tests {
		s := Grade(reportOf(sessionWith(func(rb Rubric) string {
			if rb.Name == "verbosity" {
				return tt.verbosity
			}

			return best(rb)
		})))
		if s.Grade != tt.want || *s.Score != tt.score {
			t.Errorf("verbosity %s: want %s %v, got %s %v", tt.verbosity, tt.want, tt.score, s.Grade, *s.Score)
		}
	}
}

func TestGrade_NoCodeQualityRescales(t *testing.T) {
	s := Grade(reportOf(
		sessionWith(func(rb Rubric) string {
			if rb.Name == "code_quality" {
				return InsufficientEvidence
			}

			return best(rb)
		}),
		sessionWith(func(rb Rubric) string {
			if rb.Name == "code_quality" {
				return InsufficientEvidence
			}

			return worst(rb)
		}),
	))
	want := map[string]float64{"efficiency": 0.30 / 0.75, "code_quality": 0, "procedure_compliance": 0.25 / 0.75, "verbosity": 0.20 / 0.75}
	sum := 0.0
	for _, r := range s.Rubrics {
		if math.Abs(r.Weight-want[r.Name]) > 1e-12 {
			t.Errorf("%s: weight %v, want %v", r.Name, r.Weight, want[r.Name])
		}
		sum += r.Weight
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights sum to %v, want 1", sum)
	}
	cq := s.Rubrics[1]
	if cq.Name != "code_quality" || cq.Score != nil || cq.Judged != 0 || cq.Insufficient != 2 {
		t.Fatalf("code_quality: %+v", cq)
	}
	if s.Score == nil || *s.Score != 0.5 || s.Grade != "F" {
		t.Fatalf("want F 0.5, got %s %v", s.Grade, s.Score)
	}
}

func TestGrade_CoverageDoesNotMoveTheGrade(t *testing.T) {
	without := Grade(reportOf(sessionWith(best), sessionWith(worst)))
	r := reportOf(sessionWith(best, "a", "b"), sessionWith(worst, "a"))
	r.Skills.Found = []FoundSkill{{"a", "codex", "project"}, {"b", "codex", "project"}, {"c", "codex", "global"}, {"c", "claude-code", "global"}}
	with := Grade(r)
	if *with.Score != *without.Score || with.Grade != without.Grade {
		t.Fatalf("skills moved the grade: %s %v, without %s %v", with.Grade, *with.Score, without.Grade, *without.Score)
	}
	if with.Coverage.WithSkills != 2 || with.Coverage.Sessions != 2 || *with.Coverage.Ratio != 1 {
		t.Fatalf("coverage %+v", with.Coverage)
	}
	if !slices.Equal(with.UsedSkills, []UsedSkill{{"a", 2}, {"b", 1}}) {
		t.Fatalf("used %v", with.UsedSkills)
	}
	if len(with.UnusedSkills) != 1 || with.UnusedSkills[0] != "c" {
		t.Fatalf("unused %v, want [c]", with.UnusedSkills)
	}
	if without.Coverage.WithSkills != 0 || *without.Coverage.Ratio != 0 {
		t.Fatalf("coverage without skills %+v", without.Coverage)
	}
}

func TestGrade_AllInsufficientIsNoGrade(t *testing.T) {
	insufficient := func(Rubric) string { return InsufficientEvidence }
	for name, r := range map[string]Report{
		"all insufficient": reportOf(sessionWith(insufficient), sessionWith(insufficient)),
		"no sessions":      reportOf(),
	} {
		s := Grade(r)
		if s.Grade != NoGrade || s.Score != nil {
			t.Errorf("%s: want %s with a null score, got %s %v", name, NoGrade, s.Grade, s.Score)
		}
		for _, rs := range s.Rubrics {
			if rs.Weight != 0 || rs.Score != nil {
				t.Errorf("%s: rubric %+v", name, rs)
			}
		}
	}
}

func TestRubricWeightsSumToOne(t *testing.T) {
	sum := 0.0
	for _, rb := range Rubrics {
		sum += rb.Weight
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights sum to %v", sum)
	}
}
