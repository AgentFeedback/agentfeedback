package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const sessionsPushSynopsis = "sessions push [<harness:id>...] [--due] [--since T] [--dry-run] [--limit N] [--json]"

func runSessionsPush(args []string, stdout, stderr io.Writer) (retErr error) {
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		return err
	}
	data, err := dataDir(os.Getenv)
	if err != nil {
		return err
	}
	run, err := capture.AcceptSweep(cache, data)
	if err != nil {
		client.LogTo(cache, client.Outcome{Outcome: client.OutcomeError, Reason: "session sweep: handoff rejected"}, nowFunc(), io.Discard)
		return err
	}
	var results []capture.PushResult
	var suspendedUntil *time.Time
	finished := false
	summary := func() capture.LastRun {
		last := capture.LastRun{Started: run.Started(), Finished: nowFunc(), SuspendedUntil: suspendedUntil}
		for _, result := range results {
			switch result.Status {
			case "pushed":
				last.Pushed++
			case "replayed":
				last.Replayed++
			case "deferred", "suspended":
				last.Deferred++
			case "error":
				last.Errors++
			default:
				last.Skipped++
			}
		}
		if retErr != nil && last.Errors == 0 {
			last.Errors++
		}
		return last
	}
	if run != nil {
		defer func() {
			last := summary()
			if !finished {
				if err := run.Finish(last); err != nil {
					retErr = errors.Join(retErr, err)
				}
			}
			outcome := client.Outcome{Outcome: client.OutcomeSubmitted, Reason: "session sweep completed"}
			if retErr != nil || last.Errors > 0 {
				outcome = client.Outcome{Outcome: client.OutcomeError, Reason: "session sweep failed"}
			}
			client.LogTo(cache, outcome, nowFunc(), io.Discard)
		}()
	}
	fs := newFlagSet("sessions push")
	due := fs.Bool("due", false, "push eligible sessions, oldest first")
	since := fs.String("since", "", "explicitly replace the consent cutoff with this RFC 3339 or relative time")
	dryRun := fs.Bool("dry-run", false, "show scrubbed metadata and at most 2 KiB of preview; do not upload or mark pushed")
	limit := fs.Int("limit", 0, "maximum eligible sessions (0: sessions.push_batch)")
	asJSON := fs.Bool("json", false, "print one result per session as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", sessionsPushSynopsis)
		fs.PrintDefaults()
	}
	refs, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("sessions push", err)
	}
	if (len(refs) == 0 && !*due) || (len(refs) > 0 && *due) {
		return usageErr("name sessions or pass --due, not both", "use agentfeedback "+sessionsPushSynopsis)
	}
	if *limit < 0 {
		return usageErr("--limit cannot be negative", "pass a positive limit or 0 for sessions.push_batch")
	}
	if err := checkRefs(refs); err != nil {
		return usageErr(err.Error(), "name a session as <harness>:<session id>")
	}
	if visited(fs)["since"] && strings.TrimSpace(*since) == "" {
		return usageErr("--since cannot be empty", "pass an RFC 3339 or relative time")
	}
	sinceTime, err := sinceTime(*since)
	if err != nil {
		return err
	}
	var pushErr error
	var deadline time.Time
	for {
		if run != nil {
			if err := run.Begin(); err != nil {
				return err
			}
		}
		path, err := configPath(os.Getenv)
		if err != nil {
			return err
		}
		file, _, err := loadFileConfig(path)
		if err != nil {
			return err
		}
		mode, err := resolveModeFrom(modeFlags{}, os.Getenv, file)
		if err != nil {
			return err
		}
		if mode.Mode == modeLocal {
			return failErr("session push requires a remote server with session capture enabled", "configure the Cloud server and installation key")
		}
		if mode.Settings.APIKey.Value == "" {
			return failErr("session push requires an installation key", "configure the key, then run agentfeedback doctor")
		}
		// Do not use apiClient: its start-up inbox/spool pass is unrelated to capture.
		c, err := client.New(client.Config{
			URL: mode.Settings.URL.Value, APIKey: mode.Settings.APIKey.Value,
			DataDir: data, CacheDir: cache, Now: nowFunc, Stderr: io.Discard,
			Version: clientVersion().Version,
		})
		if err != nil {
			return errClientSetup(err)
		}
		svc, err := newSessionsService(context.Background(), os.Getenv, stderr)
		if err != nil {
			return err
		}
		cfg := file.Sessions.config()
		ctx := context.Background()
		cancel := func() {}
		if run != nil {
			quiet, budget := cfg.Quiet, cfg.PushBudget
			if quiet <= 0 {
				quiet = 5 * time.Minute
			}
			if budget <= 0 {
				budget = 90 * time.Second
			}
			if deadline.IsZero() {
				deadline = run.Started().Add(2*quiet + budget)
			}
			ctx, cancel = context.WithDeadline(ctx, deadline)
		}
		pass, passErr := capture.Push(ctx, svc.db, c, svc.env, capture.PushOptions{
			Policy: capture.Options{Server: mode.Settings.URL.Value, Quiet: cfg.Quiet, PolicyTTL: cfg.PolicyTTL, Now: nowFunc(), Clock: nowFunc},
			Config: cfg, Refs: refs, Since: sinceTime, Limit: *limit, DryRun: *dryRun,
			ClientVersion: clientVersion().Version, WithCwd: file.Context.Cwd, Drop: file.Context.Drop,
			Log: func(message string) {
				if run != nil {
					client.LogTo(cache, client.Outcome{Outcome: client.OutcomeSubmitted, Reason: "session sweep: " + message}, nowFunc(), io.Discard)
				} else {
					fmt.Fprintln(stderr, "agentfeedback sessions push: "+message)
				}
			},
		})
		cancel()
		pushErr = passErr
		for _, result := range pass {
			replaced := false
			for i := range results {
				if results[i].Ref == result.Ref {
					results[i] = result
					replaced = true
					break
				}
			}
			if !replaced {
				results = append(results, result)
			}
		}
		if run != nil {
			suspendedUntil = nil
			policy, policyErr := capture.CachedPolicy(context.Background(), svc.db, c, capture.Options{Server: mode.Settings.URL.Value, Quiet: cfg.Quiet, PolicyTTL: cfg.PolicyTTL, Now: nowFunc(), Clock: nowFunc})
			if policyErr == nil && !policy.RetryAt.IsZero() {
				suspendedUntil = &policy.RetryAt
			}
		}
		if run == nil || pushErr != nil {
			break
		}
		var retryAt time.Time
		for _, result := range pass {
			if result.Status != "deferred" {
				continue
			}
			switch result.Reason {
			case "batch_limit":
				if retryAt.IsZero() {
					retryAt = nowFunc()
				}
			case "not_quiet", "capture_changed":
				quiet := cfg.Quiet
				if quiet <= 0 {
					quiet = 5 * time.Minute
				}
				retryAt = nowFunc().Add(quiet)
			}
		}
		if suspendedUntil != nil || !retryAt.Before(deadline) || !nowFunc().Before(deadline) {
			if err := run.Finish(summary()); err != nil {
				return err
			}
			finished = true
			break
		}
		again, err := run.Continue(summary(), retryAt)
		if !again {
			finished = true
		}
		if err != nil {
			return err
		}
		if !again {
			break
		}
	}
	if results == nil {
		results = []capture.PushResult{}
	}
	if *asJSON {
		err = writeJSON(stdout, results)
	} else {
		for _, result := range results {
			if _, err = fmt.Fprintf(stdout, "%s: %s (revision %s, %d bytes, %d parts)%s\n", result.Ref, result.Status, dash(result.Revision), result.Bytes, result.Parts, pushReason(result.Reason)); err != nil {
				break
			}
			if result.Preview != "" {
				if _, err = fmt.Fprintln(stdout, safe(result.Preview)); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return err
	}
	if pushErr != nil {
		return pushErr
	}
	if len(refs) > 0 {
		for _, result := range results {
			if result.Status != "pushed" && result.Status != "replayed" && result.Status != "dry-run" {
				return failErr("a named session could not be pushed", "check the per-session outcomes and agentfeedback sessions status")
			}
		}
	}
	return nil
}

func pushReason(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}
