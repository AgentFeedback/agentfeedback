package detect

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func fail(tool, command string) Event {
	return Event{Class: ClassToolFailed, SessionID: "s", Tool: tool, Command: command, ExitCode: 1, ErrorClass: "exit"}
}

// step is one event in a rule scenario and whether it must give a note.
type step struct {
	harness, event string
	ev             Event
	at             time.Duration
	nudge          bool
}

func runSteps(t *testing.T, name string, cfg Config, filed bool, steps []step) *State {
	t.Helper()
	var st State
	for i, s := range steps {
		d := Decide(&st, Input{
			Harness: s.harness, Event: s.event, Ev: s.ev, Config: cfg, Now: t0.Add(s.at),
			Filed: func() bool { return filed },
		})
		if d.Nudge != s.nudge {
			t.Errorf("%s: step %d (%s %s %+v): nudge %v, want %v; state %+v", name, i, s.harness, s.event, s.ev, d.Nudge, s.nudge, st)
		}
	}

	return &st
}

const cc, ccFail, ccStop = "claude-code", "PostToolUseFailure", "Stop"

func TestDecide_Thresholds(t *testing.T) {
	cfg := Defaults()
	// Same tool, different commands: the third failure of Bash.
	runSteps(t, "same tool", cfg, false, []step{
		{cc, ccFail, fail("Bash", "a"), 0, false},
		{cc, ccFail, fail("Bash", "b"), time.Second, false},
		{cc, ccFail, fail("Bash", "c"), 2 * time.Second, true},
	})
	// Same command twice, through two tools.
	runSteps(t, "same command", cfg, false, []step{
		{cc, ccFail, fail("Bash", "npm  test"), 0, false},
		{cc, ccFail, fail("Shell", "npm test"), time.Second, true},
	})
	// Five failures of five tools.
	runSteps(t, "session", cfg, false, []step{
		{cc, ccFail, fail("A", ""), 0, false},
		{cc, ccFail, fail("B", ""), 0, false},
		{cc, ccFail, fail("C", ""), 0, false},
		{cc, ccFail, fail("D", ""), 0, false},
		{cc, ccFail, fail("E", ""), 0, true},
	})
	// Config overrides.
	runSteps(t, "config", Config{Nudge: true, SameTool: 1}, false, []step{
		{cc, ccFail, fail("Bash", "a"), 0, true},
	})
	runSteps(t, "nudge off", Config{Nudge: false}, false, []step{
		{cc, ccFail, fail("Bash", "a"), 0, false},
		{cc, ccFail, fail("Bash", "a"), 0, false},
	})
}

func TestDecide_ResetIntervalAndCap(t *testing.T) {
	cfg := Defaults()
	st := runSteps(t, "reset and interval", cfg, false, []step{
		{cc, ccFail, fail("Bash", "x"), 0, false},
		{cc, ccFail, fail("Bash", "x"), time.Minute, true},
		// Counters reset after the note: one more is not a crossing.
		{cc, ccFail, fail("Bash", "x"), 2 * time.Minute, false},
		// Crossed again, but within 20 minutes of the note.
		{cc, ccFail, fail("Bash", "x"), 3 * time.Minute, false},
		// Past the interval, still crossed.
		{cc, ccFail, fail("Bash", "x"), 22 * time.Minute, true},
	})
	if st.Nudges != 2 || st.Total != 5 || st.Failures != 0 {
		t.Errorf("state %+v", st)
	}
	var steps []step
	for i := range 8 {
		at := time.Duration(i) * 21 * time.Minute
		steps = append(steps, step{cc, ccFail, fail("Bash", "y"), at, false}, step{cc, ccFail, fail("Bash", "y"), at + time.Second, i < 3})
	}
	if st := runSteps(t, "per-session cap", cfg, false, steps); st.Nudges != 3 {
		t.Errorf("nudges %d", st.Nudges)
	}
}

func TestDecide_Filed(t *testing.T) {
	runSteps(t, "filed", Defaults(), true, []step{
		{cc, ccFail, fail("Bash", "x"), 0, false},
		{cc, ccFail, fail("Bash", "x"), 0, false},
		{cc, ccFail, fail("Bash", "x"), 0, false},
	})
}

func TestDecide_Ignored(t *testing.T) {
	for _, ig := range []string{IgnoreInterrupt, IgnorePermission, IgnoreSelf} {
		ev := fail("Bash", "x")
		ev.Ignore = ig
		st := runSteps(t, ig, Defaults(), false, []step{
			{cc, ccFail, ev, 0, false}, {cc, ccFail, ev, 0, false}, {cc, ccFail, ev, 0, false},
			{cc, ccFail, ev, 0, false}, {cc, ccFail, ev, 0, false}, {cc, ccFail, ev, 0, false},
		})
		if st.Total != 0 || st.Failures != 0 {
			t.Errorf("%s counted: %+v", ig, st)
		}
	}
}

func TestDecide_Successes(t *testing.T) {
	ok := Event{SessionID: "s", ExitCode: 0}
	st := runSteps(t, "successes", Defaults(), false, []step{
		{"opencode", "tool.execute.after", ok, 0, false},
		{"opencode", "tool.execute.after", ok, 0, false},
		{"antigravity", "PostToolUse", ok, 0, false},
		{"omp", "tool_result", ok, 0, false},
		{"omp", "tool_result", ok, 0, false},
		{"omp", "tool_result", ok, 0, false},
	})
	if st.Total != 0 {
		t.Errorf("counted %+v", st)
	}
}

func TestDecide_PendingDelivery(t *testing.T) {
	deliver := Event{Class: ClassDeliver, SessionID: "s", ExitCode: -1}
	end := Event{Class: ClassTurnEnd, SessionID: "s", ExitCode: -1, EndsTurn: true}
	runSteps(t, "antigravity", Defaults(), false, []step{
		{"antigravity", "PostToolUse", fail("run_command", "npm test"), 0, false},
		{"antigravity", "PostToolUse", fail("run_command", "npm test"), 0, false},
		{"antigravity", "PreInvocation", deliver, time.Second, true},
		{"antigravity", "PreInvocation", deliver, 2 * time.Second, false},
	})
	idle := Event{Class: ClassDeliver, SessionID: "s", ExitCode: -1, EndsTurn: true}
	st := runSteps(t, "opencode", Defaults(), false, []step{
		{"opencode", "tool.execute.after", fail("bash", "go test"), 0, false},
		{"opencode", "tool.execute.after", fail("bash", "go test"), 0, false},
		{"opencode", "session.idle", idle, time.Second, true},
	})
	if st.Pending {
		t.Error("pending after idle")
	}
	// The end of a turn drops the pending note.
	runSteps(t, "turn end", Defaults(), false, []step{
		{"antigravity", "PostToolUse", fail("run_command", "npm test"), 0, false},
		{"antigravity", "PostToolUse", fail("run_command", "npm test"), 0, false},
		{"antigravity", "Stop", end, time.Second, false},
		{"antigravity", "PreInvocation", deliver, 2 * time.Second, false},
	})
	// An idle that is not eligible still ends the turn.
	st = runSteps(t, "idle filed", Defaults(), true, []step{
		{"opencode", "tool.execute.after", fail("bash", "go test"), 0, false},
		{"opencode", "tool.execute.after", fail("bash", "go test"), 0, false},
		{"opencode", "session.idle", idle, time.Second, false},
	})
	if st.Pending {
		t.Error("pending after an idle")
	}
}

func TestDecide_StopNeverNudges(t *testing.T) {
	end := Event{Class: ClassTurnEnd, SessionID: "s", ExitCode: -1, EndsTurn: true}
	var st State
	st.Tools = map[string]int{"Bash": 9}
	st.LastTool, st.Failures = "Bash", 9
	for _, ev := range []struct{ harness, event string }{{cc, ccStop}, {"cursor", "stop"}, {"copilot", "agentStop"}, {"codex", "Stop"}, {"omp", "agent_end"}, {"pi", "agent_settled"}} {
		if d := Decide(&st, Input{Harness: ev.harness, Event: ev.event, Ev: end, Config: Defaults(), Now: t0}); d.Nudge {
			t.Errorf("%s %s nudged", ev.harness, ev.event)
		}
	}
}

func TestConfig_Normalized(t *testing.T) {
	got := Config{Nudge: true, SameTool: -1, SameCommand: 4}.Normalized()
	want := Defaults()
	want.SameCommand = 4
	if got != want {
		t.Errorf("%+v", got)
	}
}

// TestDecide_PendingKeepsFacts: the note a deliver event gives is the one
// of the crossing, even when a later failure changed the counters.
func TestDecide_PendingKeepsFacts(t *testing.T) {
	ag := func(tool string) Event {
		ev := fail(tool, "")
		ev.ExitCode = -1

		return ev
	}
	var st State
	var d Decision
	for i, s := range []struct {
		event string
		ev    Event
	}{
		{"PostToolUse", ag("A")}, {"PostToolUse", ag("A")}, {"PostToolUse", ag("A")}, {"PostToolUse", ag("B")},
		{"PreInvocation", Event{Class: ClassDeliver, SessionID: "s", ExitCode: -1}},
	} {
		d = Decide(&st, Input{Harness: "antigravity", Event: s.event, Ev: s.ev, Config: Defaults(), Now: t0.Add(time.Duration(i) * time.Second)})
		if i < 4 && d.Nudge {
			t.Fatalf("step %d nudged", i)
		}
	}
	want := Facts{Reason: ReasonSameTool, Tool: "A", Count: 3, Exit: -1}
	if !d.Nudge || d.Facts != want {
		t.Fatalf("decision %+v", d)
	}
	if st.Pending || st.PendingFacts != nil {
		t.Errorf("pending kept: %+v", st)
	}
}

// TestDecide_SinceLast: a crossing after the session's first note says the
// counts are since that note.
func TestDecide_SinceLast(t *testing.T) {
	var st State
	var notes []Facts
	for i := range 6 {
		d := Decide(&st, Input{Harness: cc, Event: ccFail, Ev: fail("Bash", ""), Config: Defaults(), Now: t0.Add(time.Duration(i) * 30 * time.Minute)})
		if d.Nudge {
			notes = append(notes, d.Facts)
		}
	}
	if len(notes) != 2 || notes[0].SinceLast || !notes[1].SinceLast {
		t.Fatalf("notes %+v", notes)
	}
}
