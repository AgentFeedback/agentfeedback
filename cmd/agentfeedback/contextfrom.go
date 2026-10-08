package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// sessionContext is submit --context-from: one entry (span) of a session
// log is the source of the submission's key, occurred_at, harness, model,
// project and context. The process's working directory and environment are
// never read for it.
type sessionContext struct {
	ref          string // <harness>:<session id>
	span         string
	ordinal      int
	allowUnknown bool
	facts        sessions.SpanFacts
	project      string
}

// contextFromOwned are the flags --context-from replaces.
var contextFromOwned = []string{"key", "project", "harness", "model"}

// locateContext checks the --context-from command line; resolve reads the
// session once the config file is loaded.
func locateContext(value string, ordinal int, allowUnknown bool, given map[string]bool) (*sessionContext, error) {
	for _, f := range contextFromOwned {
		if given[f] {
			return nil, usageErr(fmt.Sprintf("--context-from sets key, occurred_at, harness, model and project, and --%s is given too", f),
				"drop --"+f)
		}
	}
	if ordinal < 1 {
		return nil, usageErr(fmt.Sprintf("--ordinal %d is below 1", ordinal), "number the findings of one span from 1")
	}
	i := strings.LastIndexByte(value, '#')
	if i < 0 || i == len(value)-1 {
		return nil, usageErr(fmt.Sprintf("--context-from %q names no span", value), "pass <harness>:<session id>#<span>, the ref of a digest event")
	}
	ref, span := value[:i], value[i+1:]
	if _, _, err := sessions.ParseRef(ref); err != nil {
		return nil, usageErr(oneLine(err.Error()), "pass <harness>:<session id>#<span>, the ref of a digest event")
	}

	return &sessionContext{ref: ref, span: span, ordinal: ordinal, allowUnknown: allowUnknown}, nil
}

// resolve re-reads the session under the user's policy and points the
// submitter's narrowing decision at the session's working directory.
func (c *sessionContext) resolve(s *submitter) error {
	env, err := sessionsEnvFrom(os.Getenv, s.file)
	if err != nil {
		return err
	}
	facts, err := sessions.Locate(context.Background(), env, c.ref, c.span)
	var refused *sessions.RefusedError
	switch {
	case errors.As(err, &refused):
		return failErr(fmt.Sprintf("session %s is %s, so nothing is filed from it", c.ref, refusedState(refused)),
			"file without --context-from, or change the [collect] rules that exclude it")
	case errors.Is(err, sessions.ErrSessionNotFound), errors.Is(err, sessions.ErrSpanNotFound):
		return failErr(oneLine(err.Error()), "pass the ref of an event of agentfeedback sessions digest")
	case err != nil:
		return errSessions(err)
	}
	if facts.At.IsZero() {
		return failErr(fmt.Sprintf("the span %s of session %s records no time; a replay would not match", c.span, c.ref),
			"pass the ref of an event that has an at time")
	}
	if facts.State == sessions.StateUnknownProject && !c.allowUnknown {
		return failErr(fmt.Sprintf("session %s records no working directory, so no project policy can be checked", c.ref),
			"pass --allow-unknown-project to file from it anyway")
	}
	c.facts = facts
	s.dir, s.decision = "", collect.Decision{}
	if facts.CwdExists {
		d := collect.Check(facts.Cwd, env.Home, env.Policy)
		if d.Disabled {
			return failErr(fmt.Sprintf("the [collect] rules now exclude the session's working directory (%s)", d.Reason),
				"file without --context-from, or change the rules")
		}
		for _, w := range d.Warnings {
			s.warn(w)
		}
		s.dir, s.decision = facts.Cwd, d
	}

	return nil
}

func refusedState(e *sessions.RefusedError) string {
	if e.Reason != "" {
		return e.State + " (" + e.Reason + ")"
	}

	return e.State
}

// collect gathers the project and machine groups in the session's working
// directory when it still exists, with an empty environment: nothing of the
// process filing the submission enters it. A working directory that is gone
// gives its base name as the project and nothing else.
func (c *sessionContext) collect(s *submitter, nonCode map[string]string) collect.Result {
	drop := append(slices.Clone(s.file.Context.Drop), s.decision.Drop...)
	if c.facts.CwdExists {
		r := collect.Collect(collect.Options{
			Dir: c.facts.Cwd, Getenv: func(string) string { return "" }, ClientVersion: clientVersion().Version,
			WithCwd: s.file.Context.Cwd, Context: nonCode, Drop: drop,
		})
		c.project = r.Project

		return r
	}
	r := collect.Result{Context: map[string]string{}}
	for k, v := range nonCode {
		if v != "" && !slices.Contains(drop, k) {
			r.Context[k] = v
		}
	}
	if h, err := os.Hostname(); err == nil {
		r.Machine, _, _ = strings.Cut(h, ".")
	}
	if c.facts.Cwd != "" {
		c.project = filepath.Base(c.facts.Cwd)
	}
	r.Project = c.project

	return r
}

// apply sets the members the session owns over what stdin gave; model comes
// from the session alone and is omitted when it records none.
func (c *sessionContext) apply(obj rawObject) rawObject {
	h, id, _ := sessions.ParseRef(c.ref)
	obj = obj.setString("key", sessions.Key(h, id, c.span, c.ordinal, sessions.DetectorVersion))
	obj = obj.setString("harness", c.facts.Harness)
	obj = obj.setString("occurred_at", c.facts.At.UTC().Format(occurredLayout))
	if c.facts.Model != "" {
		obj = obj.setString("model", c.facts.Model)
	} else {
		obj = obj.remove("model")
	}
	if c.project != "" {
		obj = obj.setString("project", c.project)
	}

	return obj
}

// context is the provenance --context-from adds over the collected and the
// stdin context; --context flags still go over it.
func (c *sessionContext) context() map[string]string {
	return map[string]string{
		"origin": "session-scan", "detector": c.facts.Detector,
		"session_id": c.facts.SessionID, "session_harness": c.facts.Harness,
	}
}
