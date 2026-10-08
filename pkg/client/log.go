package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
// body, the URL or the API key. Of the body's context it keeps only
// session_id.
type logLine struct {
	TS        string `json:"ts"`
	Outcome   string `json:"outcome"`
	Kind      string `json:"kind,omitempty"`
	Key       string `json:"key,omitempty"`
	ID        int64  `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// maxSessionID is the longest context.session_id the log keeps.
const maxSessionID = 128

// SessionIDOf is the body's context.session_id when it is a string of at
// most 128 bytes, else "".
func SessionIDOf(body []byte) string {
	var b struct {
		Context map[string]json.RawMessage `json:"context"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	var id string
	if raw, ok := b.Context["session_id"]; !ok || json.Unmarshal(raw, &id) != nil || len(id) > maxSessionID {
		return ""
	}

	return id
}

// FiledDir holds one empty marker file per session that filed a friction
// report.
func FiledDir(cache string) string { return filepath.Join(cache, "filed") }

// FiledMarkerPath is the marker of a session that filed a friction report:
// named by the first 32 hex digits of the SHA-256 of the session id.
func FiledMarkerPath(cache, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))

	return filepath.Join(FiledDir(cache), hex.EncodeToString(sum[:])[:32])
}

// markFiled creates the session's filed marker when o is a friction report
// of a session that was submitted, found a duplicate, spooled or flushed.
// Each filing refreshes the marker's time. A failure is ignored: the marker
// only quiets a hook's note.
func markFiled(cache string, o Outcome) {
	if o.Kind != "friction" || o.SessionID == "" {
		return
	}
	switch o.Outcome {
	case OutcomeSubmitted, OutcomeDuplicate, OutcomeSpooled, OutcomeFlushed:
	default:
		return
	}
	if os.MkdirAll(FiledDir(cache), 0o700) != nil {
		return
	}
	path := FiledMarkerPath(cache, o.SessionID)
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		_ = f.Close()
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
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
	markFiled(cache, o)
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
		SessionID: o.SessionID,
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
