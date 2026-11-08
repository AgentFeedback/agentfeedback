package main

import (
	"context"
	"fmt"
	"io"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// capturePolicy never uses the submission client or its start-up delivery pass.
// Only the local policy cache is written; no session content is read here.
func capturePolicy(getenv func(string) string, mf modeFlags, file fileConfig) (capture.Policy, error) {
	return resolveCapturePolicy(getenv, mf, file, false)
}

// resolveCapturePolicy keeps sessions list offline with cacheOnly; all consent
// decisions use Refresh instead.
func resolveCapturePolicy(getenv func(string) string, mf modeFlags, file fileConfig, cacheOnly bool) (capture.Policy, error) {
	m, err := resolveModeFrom(mf, getenv, file)
	if err != nil {
		return capture.Policy{}, err
	}
	cfg := file.Sessions.config()
	opts := capture.Options{
		Server: m.Settings.URL.Value, Local: m.Mode == modeLocal,
		Quiet: cfg.Quiet, PolicyTTL: cfg.PolicyTTL, Now: nowFunc(), Clock: nowFunc,
	}
	ctx := context.Background()
	if opts.Local {
		return capture.Refresh(ctx, nil, nil, opts)
	}
	if m.Settings.APIKey.Value == "" {
		return capture.Policy{State: "unauthorized", Server: redactURL(opts.Server), Quiet: cfg.Quiet.String()}, nil
	}
	data, err := dataDir(getenv)
	if err != nil {
		return capture.Policy{}, err
	}
	cache, err := cacheDir(getenv)
	if err != nil {
		return capture.Policy{}, err
	}
	c, err := client.New(client.Config{
		URL: opts.Server, APIKey: m.Settings.APIKey.Value,
		DataDir: data, CacheDir: cache,
		Now: nowFunc, Stderr: io.Discard, Version: clientVersion().Version,
	})
	if err != nil {
		return capture.Policy{}, errClientSetup(err)
	}
	path, err := localDBPath(getenv)
	if err != nil {
		return capture.Policy{}, err
	}
	target, err := localmode.Open(ctx, path, clientVersion().Version, io.Discard)
	if err != nil {
		return capture.Policy{}, errLocalOpen(path, err)
	}
	defer func() { _ = target.Close() }()
	if cacheOnly {
		return capture.CachedPolicy(ctx, target.DB(), c, opts)
	}
	return capture.Refresh(ctx, target.DB(), c, opts)
}

// writeCaptureOutput marks a notice only after the complete human-readable
// output is written. JSON callers must not use this helper.
func writeCaptureOutput(getenv func(string) string, w io.Writer, text string, p capture.Policy) error {
	n, err := io.WriteString(w, text)
	if err != nil {
		return err
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	return markCaptureNotice(getenv, p)
}

func markCaptureNotice(getenv func(string) string, p capture.Policy) error {
	if p.State != capture.Enabled || !p.Enabled || p.NoticeVersion == p.LastNoticeVersion {
		return nil
	}
	path, err := localDBPath(getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	target, err := localmode.Open(ctx, path, clientVersion().Version, io.Discard)
	if err != nil {
		return errLocalOpen(path, err)
	}
	defer func() { _ = target.Close() }()
	return capture.MarkNotice(ctx, target.DB(), p)
}

func captureProblem(p capture.Policy) error {
	switch p.State {
	case "unauthorized":
		return fmt.Errorf("an authorized installation key is required to read the session policy; check the configured server and installation key, then run agentfeedback doctor")
	case "suspended":
		return fmt.Errorf("session policy requests are suspended; wait for the server's Retry-After period, then run agentfeedback doctor")
	}
	return nil
}
