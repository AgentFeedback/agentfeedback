package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

const hookSynopsis = "hook <harness> <event>"

// hookStdinMax is the most of a hook payload read; the rest is discarded.
const hookStdinMax = 1 << 20

// The hook's own deadlines, measured from its start: reading stdin, and
// the detect part (config, prune, the session file). The remote flush at
// the end of a turn keeps its own, also measured from the start. Variables
// so tests can shorten them.
var (
	hookStdinWait  = time.Second
	hookDetectWait = 1500 * time.Millisecond
)

// hookWriteMargin is how much earlier than the detect deadline the session
// file may last be written, so the detect part never writes it after the
// hook stopped waiting for it.
const hookWriteMargin = 100 * time.Millisecond

// exitStatus is a command's exit status when it printed what it had to and
// the status is not a failure: run prints nothing for it.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) Unwrap() error { return errReported }

// lastRun is the file a hook writes on every run, for doctor.
type lastRun struct {
	TS    string `json:"ts"`
	Event string `json:"event"`
}

// hookLastRunPath is <cache>/hooks/<harness>.json.
func hookLastRunPath(cache, name string) string {
	return filepath.Join(cache, "hooks", name+".json")
}

// hookSpawnErrorPath is the file an OpenCode, omp or pi plugin writes when
// it cannot start the hook.
func hookSpawnErrorPath(cache, name string) string {
	return filepath.Join(cache, "hooks", name+".spawn-error.json")
}

// runHook is the harness hook: it reads the event's payload from stdin,
// counts tool failures per session and, when a threshold is crossed and the
// harness can show it, prints a short note in the harness's output format.
// At the end of a turn in remote mode it then sends the spool's due
// entries, as flush --hook does. It never prints to stderr and always exits
// 0, except for Copilot's note, which exits 2 as Copilot requires; any
// failure is one error line in the client log. Stdin that does not end
// within a second counts as no payload, and the detect part that does not
// finish within 1.5 s of the start gives no note; the end of a turn still
// flushes.
func runHook(args []string, stdin io.Reader, stdout, _ io.Writer) error {
	start := time.Now()
	skipStartupPass = true
	if len(args) != 2 || !harness.Known(args[0]) {
		drain(stdin)

		return nil
	}
	name, event := args[0], args[1]
	cache, cerr := cacheDir(os.Getenv)
	fail := func(err error) {
		if cerr != nil {
			return
		}
		if !logSchemaTooNew(cache, err) {
			client.LogTo(cache, client.Outcome{Outcome: client.OutcomeError, Reason: "hook " + name + " " + oneLine(event) + ": " + oneLine(err.Error())}, nowFunc(), io.Discard)
		}
	}
	read := make(chan []byte, 1)
	go func() {
		payload, _ := io.ReadAll(io.LimitReader(stdin, hookStdinMax+1))
		read <- payload
		_, _ = io.Copy(io.Discard, stdin)
	}()
	var payload []byte
	stdinTimer := time.NewTimer(time.Until(start.Add(hookStdinWait)))
	select {
	case payload = <-read:
		stdinTimer.Stop()
	case <-stdinTimer.C:
		fail(errors.New("stdin did not end within the hook's deadline; the payload was not read"))
	}
	if cerr != nil || !detect.KnownEvent(name, event) {
		return nil
	}
	if err := writeLastRun(cache, name, event, nowFunc()); err != nil {
		fail(err)
	}
	ev, ok := detect.Parse(name, event, payload)
	if !ok || len(payload) > hookStdinMax {
		return nil
	}

	flush := func() {
		if !ev.EndsTurn {
			return
		}
		mf := modeFlags{}
		if m, err := resolveMode(mf, os.Getenv); err != nil {
			fail(err)
		} else if m.Mode == modeRemote {
			boundedHook(start, "hook "+name+" "+oneLine(event), mf)
		}
	}
	type result struct {
		out    []byte
		status int
	}
	deadline := start.Add(hookDetectWait)
	// Once the hook stops waiting, the detect part logs nothing more.
	var abandoned atomic.Bool
	detectFail := func(err error) {
		if !abandoned.Load() {
			fail(err)
		}
	}
	done := make(chan result, 1)
	go func() {
		out, status := hookDetect(cache, name, event, ev, deadline.Add(-hookWriteMargin), detectFail)
		done <- result{out, status}
	}()
	detectTimer := time.NewTimer(time.Until(deadline))
	defer detectTimer.Stop()
	var res result
	select {
	case res = <-done:
	case <-detectTimer.C:
		abandoned.Store(true)
		fail(errors.New("the detect part ran past its deadline; no note"))
		flush()

		return nil
	}
	if len(res.out) > 0 {
		if _, err := stdout.Write(res.out); err != nil {
			fail(err)
		}
	}
	flush()
	if res.status != 0 {
		return exitStatus(res.status)
	}

	return nil
}

// drain discards what is left on stdin without waiting for it.
func drain(stdin io.Reader) {
	go func() { _, _ = io.Copy(io.Discard, stdin) }()
}

// hookDetect applies the event to the session's counters and returns the
// hook's output and exit status. The session file's lock is tried until
// deadline, which also bounds writing it. A user config that cannot be
// read gives no note; counting goes on.
func hookDetect(cache, name, event string, ev detect.Event, deadline time.Time, fail func(error)) ([]byte, int) {
	now := nowFunc()
	if err := detect.Prune(cache, now, client.FiledDir(cache)); err != nil {
		fail(err)
	}
	if ev.SessionID == "" || ev.Class == "" {
		return nil, 0
	}
	cfg := detect.Defaults()
	if path, err := configPath(os.Getenv); err != nil {
		fail(err)
		cfg.Nudge = false
	} else if file, _, err := loadFileConfig(path); err != nil {
		fail(err)
		cfg.Nudge = false
	} else {
		cfg = file.Detect.config()
		if ev.Cwd != "" && filepath.IsAbs(ev.Cwd) {
			d := collect.Check(ev.Cwd, "", file.Collect)
			if d.Disabled || d.NudgeOff {
				cfg.Nudge = false
			}
		} else if file.Collect.Disabled || file.Collect.OptInOnly {
			cfg.Nudge = false
		}
	}
	var dec detect.Decision
	err := detect.Update(cache, name, ev.SessionID, deadline, func(st *detect.State) {
		dec = detect.Decide(st, detect.Input{
			Harness: name, Event: event, Ev: ev, Config: cfg, Now: now,
			Filed: func() bool {
				return filedInSession(cache, ev.SessionID) || filedInSession(cache, detect.CleanSessionID(ev.SessionID))
			},
		})
	})
	if err != nil {
		fail(err)

		return nil, 0
	}
	if !dec.Nudge {
		return nil, 0
	}

	return detect.Render(name, event, detect.Text(name, ev.SessionID, dec.Facts))
}

// writeLastRun records the hook's run for doctor: owner-only, written by
// rename.
func writeLastRun(cache, name, event string, now time.Time) error {
	if len(event) > 64 {
		event = event[:64]
	}
	data, err := json.Marshal(lastRun{TS: now.UTC().Format(time.RFC3339), Event: event})
	if err != nil {
		return err
	}
	path := hookLastRunPath(cache, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())

		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())

		return err
	}

	return nil
}

// filedInSession reports whether the session filed a friction report: the
// client leaves a marker when it logs one submitted, found a duplicate,
// spooled or flushed. The hook checks both the raw id and the id its note
// shows.
func filedInSession(cache, sessionID string) bool {
	_, err := os.Stat(client.FiledMarkerPath(cache, sessionID))

	return err == nil
}
