package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const sessionsPushSynopsis = "sessions push [<harness:id>...] [--due] [--since T] [--dry-run] [--limit N] [--json]"

func runSessionsPush(args []string, stdout, stderr io.Writer) error {
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
	data, err := dataDir(os.Getenv)
	if err != nil {
		return err
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		return err
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
	svc, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		return err
	}
	cfg := file.Sessions.config()
	results, pushErr := capture.Push(context.Background(), svc.db, c, svc.env, capture.PushOptions{
		Policy: capture.Options{Server: mode.Settings.URL.Value, Quiet: cfg.Quiet, PolicyTTL: cfg.PolicyTTL, Now: nowFunc(), Clock: nowFunc},
		Config: cfg, Refs: refs, Since: sinceTime, Limit: *limit, DryRun: *dryRun,
		ClientVersion: clientVersion().Version, WithCwd: file.Context.Cwd, Drop: file.Context.Drop,
		Log: func(message string) { fmt.Fprintln(stderr, "agentfeedback sessions push: "+message) },
	})
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
	if err != nil || pushErr != nil {
		return errors.Join(err, pushErr)
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
