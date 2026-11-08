package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// Tests can stall the launcher to exercise the hook's outer deadline.
var captureSweep = capture.Sweep

// A trigger reads only the existing cache. Policy refresh and transcript access
// belong to the detached child, never the harness's deadline.
func triggerCaptureSweep(getenv func(string) string, mf modeFlags, deadline time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	done := make(chan struct{})
	clock, launch := nowFunc, captureSweep
	go func() {
		defer close(done)
		captureSweepBody(ctx, getenv, mf, clock, launch)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func captureSweepBody(ctx context.Context, getenv func(string) string, mf modeFlags, clock func() time.Time, launch func(string, string, capture.SweepOptions) error) {
	cache, err := cacheDir(getenv)
	if err != nil {
		return
	}
	fail := func() {
		if ctx.Err() == nil {
			client.LogTo(cache, client.Outcome{Outcome: client.OutcomeError, Reason: "session sweep: trigger failed"}, clock(), io.Discard)
		}
	}
	path, err := configPath(getenv)
	if err != nil {
		fail()
		return
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		fail()
		return
	}
	mode, err := resolveModeFrom(mf, getenv, file)
	if err != nil {
		fail()
		return
	}
	if mode.Mode == modeLocal || mode.Settings.APIKey.Value == "" {
		return
	}
	data, err := dataDir(getenv)
	if err != nil {
		fail()
		return
	}
	dbPath, err := localDBPath(getenv)
	if err != nil {
		fail()
		return
	}
	c, err := client.New(client.Config{URL: mode.Settings.URL.Value, APIKey: mode.Settings.APIKey.Value, DataDir: data, CacheDir: cache, Stderr: io.Discard, Now: clock, Version: clientVersion().Version})
	if err != nil {
		fail()
		return
	}
	cfg := file.Sessions.config()
	policy, err := capture.CachedSweepPolicy(ctx, dbPath, c, capture.Options{Server: mode.Settings.URL.Value, Quiet: cfg.Quiet, PolicyTTL: cfg.PolicyTTL, Now: clock(), Clock: clock})
	if ctx.Err() != nil || os.IsNotExist(err) {
		return
	}
	if err != nil {
		fail()
		return
	}
	if ctx.Err() != nil {
		return
	}
	if err := launch(cache, data, capture.SweepOptions{Policy: policy, Config: cfg, Clock: clock, Env: append(os.Environ(), envURL+"="+mode.Settings.URL.Value)}); err != nil {
		fail()
	}
}

func lastCaptureRun(getenv func(string) string) (capture.LastRun, error) {
	cache, err := cacheDir(getenv)
	if err != nil {
		return capture.LastRun{}, err
	}
	return capture.ReadLastRun(cache)
}

func printLastCaptureRun(w io.Writer, run capture.LastRun) {
	if run.Started.IsZero() {
		fmt.Fprintln(w, "last push: never")
		return
	}
	fmt.Fprintf(w, "last push: %s; %d pushed, %d replayed, %d skipped, %d deferred, %d errors\n", shownTime(run.Finished), run.Pushed, run.Replayed, run.Skipped, run.Deferred, run.Errors)
	if run.SuspendedUntil != nil {
		fmt.Fprintf(w, "  suspended until %s\n", shownTime(*run.SuspendedUntil))
	}
}
