package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// logName is the client log's file name; this is its only definition.
const logName = "client.jsonl"

// logMaxBytes is the size past which client.jsonl is rotated to
// client.jsonl.1; a variable so tests can make it small.
var logMaxBytes int64 = 10 << 20

// LogPath is the owner-only client log: one JSON line per outcome.
func LogPath(cache string) string { return filepath.Join(cache, "log", logName) }

// logLine is what the log keeps of an outcome: never the payload, a message
// body, the URL or the API key.
type logLine struct {
	TS        string `json:"ts"`
	Outcome   string `json:"outcome"`
	Kind      string `json:"kind,omitempty"`
	Key       string `json:"key,omitempty"`
	ID        int64  `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Log appends o to the client log. A failure is one stderr warning; it never
// changes the outcome.
func (c *Client) Log(o Outcome) {
	LogTo(c.cacheDir, o, c.now(), c.stderr)
}

// LogTo appends o to the client log under cache without a configured Client,
// for an outcome decided before one exists (a disabled submission, where the
// URL and the key may be unset). now stamps the line; a nil stderr discards
// the warning a failure prints.
func LogTo(cache string, o Outcome, now time.Time, stderr io.Writer) {
	if stderr == nil {
		stderr = io.Discard
	}
	if err := appendLog(cache, o, now, stderr); err != nil {
		fmt.Fprintf(stderr, "agentfeedback: warning: cannot write the client log %s (%v)\n", LogPath(cache), err)
	}
}

func appendLog(cache string, o Outcome, now time.Time, stderr io.Writer) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(logLine{
		TS:        now.UTC().Format(time.RFC3339Nano),
		Outcome:   o.Outcome,
		Kind:      o.Kind,
		Key:       o.Key,
		ID:        o.ID,
		RequestID: o.RequestID,
		Reason:    o.Reason,
	}); err != nil {
		return err
	}
	line := buf.Bytes()

	path := LogPath(cache)
	if err := ensureDir(filepath.Dir(path), stderr); err != nil {
		return err
	}
	f, err := openLog(path)
	if err != nil {
		return err
	}
	if f, err = rotateIfFull(f, path, int64(len(line))); err != nil {
		return err
	}
	_, werr := f.Write(line)

	return errors.Join(werr, f.Close())
}

// openLog opens the log for appending, owner-only.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()

		return nil, err
	}

	return f, nil
}

// rotateIfFull returns f, or when n more bytes would pass logMaxBytes a
// fresh file at path. It renames path to path.1 only while path is still the
// file f holds: a process that lost the race to another rotation reopens
// without renaming, so it never overwrites the other's .1.
func rotateIfFull(f *os.File, path string, n int64) (*os.File, error) {
	fdInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}
	if fdInfo.Size()+n <= logMaxBytes {
		return f, nil
	}
	if pathInfo, err := os.Stat(path); err == nil && os.SameFile(pathInfo, fdInfo) {
		if err := os.Rename(path, path+".1"); err != nil {
			_ = f.Close()

			return nil, err
		}
	}
	_ = f.Close()

	return openLog(path)
}
