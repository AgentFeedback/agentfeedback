package detect

import "time"

// Config is the nudge rule's settings: the user config's [detect] table.
type Config struct {
	// Nudge switches the note on; counting happens either way.
	Nudge bool
	// SameTool, SameCommand and SessionFailures are the thresholds, counted
	// since the last note: failures of one tool, of one command, and of any
	// tool call.
	SameTool, SameCommand, SessionFailures int
	// IntervalMinutes is the least time between two notes in a session.
	IntervalMinutes int
	// MaxPerSession caps the notes of a session.
	MaxPerSession int
}

// Defaults is the rule as shipped.
func Defaults() Config {
	return Config{Nudge: true, SameTool: 3, SameCommand: 2, SessionFailures: 5, IntervalMinutes: 20, MaxPerSession: 3}
}

// Normalized replaces every value that is zero or negative by its default.
func (c Config) Normalized() Config {
	d := Defaults()
	fix := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	fix(&c.SameTool, d.SameTool)
	fix(&c.SameCommand, d.SameCommand)
	fix(&c.SessionFailures, d.SessionFailures)
	fix(&c.IntervalMinutes, d.IntervalMinutes)
	fix(&c.MaxPerSession, d.MaxPerSession)

	return c
}

// State is one session's counters. It holds no command text: commands are
// counted under their digest.
type State struct {
	// Tools and Commands count failures per tool name and per command
	// digest since the last note; Failures counts every failure since then.
	Tools    map[string]int `json:"tools"`
	Commands map[string]int `json:"commands"`
	Failures int            `json:"failures"`
	// Total counts every failure of the session.
	Total int `json:"total"`
	// The last counted failure.
	LastTool       string `json:"last_tool,omitempty"`
	LastCommand    string `json:"last_command_digest,omitempty"`
	LastExit       int    `json:"last_exit"`
	LastErrorClass string `json:"last_error_class,omitempty"`
	// Nudges counts the notes given, LastNudge is the time of the last.
	Nudges    int       `json:"nudges"`
	LastNudge time.Time `json:"last_nudge,omitzero"`
	// Pending is a crossed threshold waiting for an event that can deliver
	// the note, within the same turn; PendingFacts are its facts, without
	// the command.
	Pending      bool      `json:"pending"`
	PendingFacts *Facts    `json:"pending_facts,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

// Input is everything Decide needs besides the state.
type Input struct {
	Harness string
	Event   string
	Ev      Event
	Config  Config
	Now     time.Time
	// Filed reports whether the session already filed a friction report;
	// called only when a note is about to be given.
	Filed func() bool
}

// Reasons a threshold was crossed.
const (
	ReasonSameTool    = "same_tool"
	ReasonSameCommand = "same_command"
	ReasonSession     = "session"
)

// Facts are what the note says about the failures.
type Facts struct {
	Reason string `json:"reason"`
	Tool   string `json:"tool,omitempty"`
	// Count is the number of failures behind Reason.
	Count int `json:"count"`
	// Exit is the last exit status, -1 when unknown.
	Exit int `json:"exit"`
	// SinceLast is set when the session had a note before: the counts are
	// since that note.
	SinceLast bool `json:"since_last,omitempty"`
	// Command is the current event's command, or "". It is never stored.
	Command string `json:"-"`
}

// Decision is Decide's answer.
type Decision struct {
	Nudge bool
	Facts Facts
}

// crossed reports which threshold the counters since the last note
// reached, if any.
func (s *State) crossed(c Config) (string, int, bool) {
	switch {
	case s.LastTool != "" && s.Tools[s.LastTool] >= c.SameTool:
		return ReasonSameTool, s.Tools[s.LastTool], true
	case s.LastCommand != "" && s.Commands[s.LastCommand] >= c.SameCommand:
		return ReasonSameCommand, s.Commands[s.LastCommand], true
	case s.Failures >= c.SessionFailures:
		return ReasonSession, s.Failures, true
	}

	return "", 0, false
}

// eligible reports whether a note may be given now, before the filed check.
func (s *State) eligible(in Input, c Config) bool {
	if !c.Nudge || s.Nudges >= c.MaxPerSession {
		return false
	}

	return s.LastNudge.IsZero() || in.Now.Sub(s.LastNudge) >= time.Duration(c.IntervalMinutes)*time.Minute
}

// give records a note and resets the counters since the last one.
func (s *State) give(now time.Time) {
	s.Nudges++
	s.LastNudge = now
	s.Tools, s.Commands, s.Failures = map[string]int{}, map[string]int{}, 0
	s.Pending, s.PendingFacts = false, nil
}

// Decide applies the event to the session's counters and says whether to
// give the note now. A failure with an ignore reason is not counted. A
// crossed threshold gives the note at once when the event can deliver it,
// otherwise it waits, pending, for a deliver event of the same turn; the
// end of a turn drops it.
func Decide(s *State, in Input) Decision {
	c := in.Config.Normalized()
	if s.Tools == nil {
		s.Tools = map[string]int{}
	}
	if s.Commands == nil {
		s.Commands = map[string]int{}
	}
	if s.Created.IsZero() {
		s.Created = in.Now
	}
	s.Updated = in.Now
	ev := in.Ev
	var d Decision
	// give gives the note with f when the session did not file a report.
	give := func(f Facts) {
		if in.Filed != nil && in.Filed() {
			s.Pending, s.PendingFacts = false, nil

			return
		}
		d = Decision{Nudge: true, Facts: f}
		s.give(in.Now)
	}
	try := func(command string) {
		reason, n, ok := s.crossed(c)
		if !ok {
			return
		}
		if !s.eligible(in, c) {
			return
		}
		f := Facts{Reason: reason, Tool: s.LastTool, Count: n, Exit: s.LastExit, SinceLast: s.Nudges > 0}
		if !CanDeliver(in.Harness, in.Event) {
			s.Pending, s.PendingFacts = true, &f

			return
		}
		f.Command = command
		give(f)
	}
	switch ev.Class {
	case ClassToolFailed:
		if ev.Ignore != "" {
			return d
		}
		s.Failures++
		s.Total++
		s.LastTool, s.LastExit, s.LastErrorClass, s.LastCommand = ev.Tool, ev.ExitCode, ev.ErrorClass, ""
		if ev.Tool != "" {
			s.Tools[ev.Tool]++
		}
		if ev.Command != "" {
			s.LastCommand = CommandDigest(ev.Command)
			s.Commands[s.LastCommand]++
		}
		try(ev.Command)
	case ClassDeliver:
		if s.Pending && s.PendingFacts != nil && s.eligible(in, c) {
			give(*s.PendingFacts)
		}
		if ev.EndsTurn {
			s.Pending, s.PendingFacts = false, nil
		}
	case ClassTurnEnd:
		s.Pending, s.PendingFacts = false, nil
	}

	return d
}
