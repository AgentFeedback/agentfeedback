package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// entryPrefix marks the files this package owns; every other name in
	// spool/ and rejected/ (the bash client's friction-*.json, *.rejected,
	// .tmp.*) is left alone.
	entryPrefix    = "af1-"
	entrySuffix    = ".json"
	inflightMarker = ".json.inflight-"
	tempPrefix     = ".tmp-"
	tempMaxAge     = 24 * time.Hour

	inflightStale  = time.Minute
	frictionMaxAge = 20 * time.Hour
	entryMaxAge    = 30 * 24 * time.Hour
	rejectedMaxAge = 30 * 24 * time.Hour
	responseKeep   = 64 << 10
	summaryShown   = 120
)

// SpoolDir holds the submissions waiting to be sent, under the data
// directory.
func SpoolDir(data string) string { return filepath.Join(data, "spool") }

// RejectedDir holds the submissions the server refused; it sits beside
// spool/, not inside it, and is never read for sending.
func RejectedDir(data string) string { return filepath.Join(data, "rejected") }

// entry is one spool or rejected/ file. Body and Response are written as
// raw bytes (see encode), never re-encoded. Destination is the base URL or
// LocalDestination the submission was meant for; a Flush delivers an entry
// only to that destination, so a configured URL that changes never carries
// pending work to the new server, and local work never reaches a server.
type entry struct {
	V           int             `json:"v"`
	Kind        string          `json:"kind"`
	Key         string          `json:"key"`
	Destination string          `json:"destination"`
	CreatedAt   time.Time       `json:"created_at"`
	Attempts    int             `json:"attempts"`
	NotBefore   time.Time       `json:"not_before"`
	LastError   string          `json:"last_error"`
	RejectedAt  *time.Time      `json:"rejected_at,omitempty"`
	Status      int             `json:"status,omitempty"`
	Response    json.RawMessage `json:"response,omitempty"`
	Body        json.RawMessage `json:"body"`
}

// newEntry is an entry bound to this client's destination.
func (c *Client) newEntry(kind, key string, body []byte, now time.Time) entry {
	return entry{V: 1, Kind: kind, Key: key, Destination: c.destination, CreatedAt: now.UTC(), NotBefore: now.UTC(), Body: body}
}

// encode writes the wrapper with Response and Body spliced in as given:
// json.Marshal would compact them and escape HTML characters.
func (e entry) encode() ([]byte, error) {
	head := e
	head.Response, head.Body = nil, nil
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		V           int        `json:"v"`
		Kind        string     `json:"kind"`
		Key         string     `json:"key"`
		Destination string     `json:"destination"`
		CreatedAt   time.Time  `json:"created_at"`
		Attempts    int        `json:"attempts"`
		NotBefore   time.Time  `json:"not_before"`
		LastError   string     `json:"last_error"`
		RejectedAt  *time.Time `json:"rejected_at,omitempty"`
		Status      int        `json:"status,omitempty"`
	}{head.V, head.Kind, head.Key, head.Destination, head.CreatedAt, head.Attempts, head.NotBefore, head.LastError, head.RejectedAt, head.Status}); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("}"))
	if len(e.Response) > 0 {
		out = append(out, `,"response":`...)
		out = append(out, e.Response...)
	}
	out = append(out, `,"body":`...)
	out = append(out, e.Body...)
	out = append(out, "}\n"...)

	return out, nil
}

// decodeEntry reads a wrapper; a file that is not one is an error. An entry
// without a destination (written before entries recorded one) is an error
// too: there is no way to know which server it was for.
func decodeEntry(data []byte) (entry, error) {
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		return entry{}, err
	}
	if e.V != 1 || strings.TrimSpace(e.Kind) == "" || strings.TrimSpace(e.Key) == "" || len(e.Body) == 0 || e.CreatedAt.IsZero() {
		return entry{}, errors.New("not a version 1 spool entry")
	}
	if strings.TrimSpace(e.Destination) == "" {
		return entry{}, errors.New("the spool entry names no destination")
	}

	return e, nil
}

// newEntryName is af1-<UTC time>-<8 hex>.json; names sort oldest first.
func newEntryName(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])

	return entryPrefix + now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:]) + entrySuffix
}

// ensureDir creates dir owner-only and tightens it every time; a failed
// chmod is a warning, not a failure.
func (c *Client) ensureDir(dir string) error { return ensureDir(dir, c.stderr) }

func ensureDir(dir string, stderr io.Writer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "agentfeedback: warning: cannot make %s owner-only (%v); run chmod 700 %s\n", dir, err, dir)
	}

	return nil
}

// writeAtomic writes data to dir/name through a fresh 0600 temporary file
// (.tmp-<name>.<random>) in the same directory: fsync, close, rename. The
// temporary file is removed on any failure.
func (c *Client) writeAtomic(dir, name string, data []byte) error {
	if err := c.ensureDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, tempPrefix+name+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)

		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return nil
}

// writeEntry writes e as dir/name.
func (c *Client) writeEntry(dir, name string, e entry) error {
	data, err := e.encode()
	if err != nil {
		return err
	}

	return c.writeAtomic(dir, name, data)
}

// writeRejected writes e to rejected/ with the refusal: when, the status,
// and the server's body (at most 64 KiB; a JSON string when not JSON).
func (c *Client) writeRejected(name string, e entry, res result, now time.Time) error {
	at := now.UTC()
	e.RejectedAt = &at
	e.Status = res.status
	resp := res.response
	if len(resp) > responseKeep {
		resp = resp[:responseKeep]
	}
	if len(bytes.TrimSpace(resp)) > 0 && json.Valid(resp) {
		e.Response = resp
	} else if len(resp) > 0 {
		s, err := json.Marshal(string(resp))
		if err != nil {
			return err
		}
		e.Response = s
	}

	return c.writeEntry(RejectedDir(c.dataDir), name, e)
}

// FlushReport counts what one Flush pass did. Flushed counts 201 and 200;
// Duplicates is the 200 subset. OtherDestination counts the entries bound
// to another destination than this client's, left in place and named on
// stderr. Stopped is "" or why the pass ended early.
type FlushReport struct {
	Flushed          int
	Duplicates       int
	Pending          int
	Rejected         int
	Mismatched       int
	Expired          int
	Deferred         int
	OtherDestination int
	Stopped          string
}

// candidate is one af1- spool file a pass may handle.
type candidate struct {
	name string // current file name
	base string // af1-<...>.json
}

// Flush prunes the spool by retention, then sends every due entry bound to
// this client's destination, oldest first, by the same retry table as
// Submit. An entry bound to another destination is left in place untouched,
// its retention included, and named: only a flush bound to its destination
// expires it. rejected/ is only pruned, never sent. No request is made when
// nothing is due. Over the local database the pass stops at the first entry
// the database cannot take: the rest would wait out the same busy timeout.
func (c *Client) Flush(ctx context.Context) FlushReport {
	var rep FlushReport
	now := c.now()
	spool := SpoolDir(c.dataDir)
	c.pruneTemps(spool, now)
	c.pruneTemps(RejectedDir(c.dataDir), now)
	c.pruneRejected(now)

	var due []candidate
	for _, cand := range c.candidates(spool, now) {
		path := filepath.Join(spool, cand.name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		e, err := decodeEntry(data)
		if err != nil {
			c.quarantine(cand.name, err)

			continue
		}
		if e.Destination != c.destination {
			rep.OtherDestination++
			fmt.Fprintf(c.stderr, "agentfeedback: spool entry %s is for %s, not %s; left in place; %s\n", cand.base, e.Destination, c.destination, flushHint(e.Destination))

			continue
		}
		if c.expire(path, e, now) {
			rep.Expired++

			continue
		}
		if e.NotBefore.After(now) {
			rep.Deferred++

			continue
		}
		due = append(due, cand)
	}

	for _, cand := range due {
		if ctx.Err() != nil {
			rep.Stopped = "canceled"

			break
		}
		if stop := c.flushOne(ctx, cand, &rep); stop != "" {
			rep.Stopped = stop

			break
		}
	}

	return rep
}

// flushHint is the command that delivers an entry bound to dest.
func flushHint(dest string) string {
	if dest == LocalDestination {
		return "run agentfeedback flush --local"
	}

	return "run agentfeedback flush --server " + dest
}

// candidates lists the af1- entries in spool, oldest first: pending ones and
// in-flight ones whose claim is older than a minute.
func (c *Client) candidates(spool string, now time.Time) []candidate {
	entries, err := os.ReadDir(spool)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot read the spool %s (%v)\n", spool, err)
		}

		return nil
	}
	var out []candidate
	for _, de := range entries {
		name := de.Name()
		if !de.Type().IsRegular() || !strings.HasPrefix(name, entryPrefix) {
			continue
		}
		if strings.HasSuffix(name, entrySuffix) {
			out = append(out, candidate{name: name, base: name})

			continue
		}
		i := strings.Index(name, inflightMarker)
		if i < 0 {
			continue
		}
		ns, err := strconv.ParseInt(name[i+len(inflightMarker):], 10, 64)
		if err != nil || now.Sub(time.Unix(0, ns)) > inflightStale {
			out = append(out, candidate{name: name, base: name[:i] + entrySuffix})
		}
	}
	slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(a.base, b.base) })

	return out
}

// quarantine moves an unreadable af1- spool file to rejected/ unchanged; it
// is never retried. The move is one error line in the client log too, so a
// pass whose stderr nobody reads still leaves a record.
func (c *Client) quarantine(name string, why error) {
	c.Log(Outcome{Outcome: OutcomeError, Key: name, Reason: "quarantined"})
	from := filepath.Join(SpoolDir(c.dataDir), name)
	to := filepath.Join(RejectedDir(c.dataDir), name)
	if err := c.ensureDir(RejectedDir(c.dataDir)); err == nil {
		err = os.Rename(from, to)
		if err == nil {
			// Its rejected_at is unreadable, so it ages by mtime from now.
			now := c.now()
			if err := os.Chtimes(to, now, now); err != nil {
				fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot set the time of %s (%v); it may be pruned early\n", to, err)
			}
			fmt.Fprintf(c.stderr, "agentfeedback: warning: spool file %s is not a readable entry (%v); moved to %s and not retried\n", name, why, to)

			return
		}
	}
	fmt.Fprintf(c.stderr, "agentfeedback: warning: spool file %s is not a readable entry (%v) and could not be moved to %s\n", name, why, RejectedDir(c.dataDir))
}

// expire deletes an entry past retention (frictions 20 h, other kinds 30
// days since created_at), warns, and logs it.
func (c *Client) expire(path string, e entry, now time.Time) bool {
	age := now.Sub(e.CreatedAt)
	friction := strings.EqualFold(strings.TrimSpace(e.Kind), "friction")
	if (friction && age <= frictionMaxAge) || (!friction && age <= entryMaxAge) {
		return false
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot delete expired spool file %s (%v)\n", path, err)

		return false
	}
	note := ""
	if friction {
		var s struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(e.Body, &s) == nil && s.Summary != "" {
			sum := s.Summary
			if len(sum) > summaryShown {
				sum = strings.ToValidUTF8(sum[:summaryShown], "")
			}
			note = fmt.Sprintf(" (summary: %q)", sum)
		}
	}
	fmt.Fprintf(c.stderr, "agentfeedback: warning: dropped spooled %s %s after %s unsent%s\n", e.Kind, e.Key, age.Round(time.Minute), note)
	c.Log(Outcome{Outcome: OutcomeError, Kind: e.Kind, Key: e.Key, Reason: "expired"})

	return true
}

// pruneTemps removes this package's temporary files (.tmp-, never the bash
// client's .tmp.) older than 24 hours by mtime from dir, with a warning each.
func (c *Client) pruneTemps(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if !de.Type().IsRegular() || !strings.HasPrefix(de.Name(), tempPrefix) {
			continue
		}
		info, err := de.Info()
		if err != nil || now.Sub(info.ModTime()) <= tempMaxAge {
			continue
		}
		path := filepath.Join(dir, de.Name())
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot delete stale temporary file %s (%v)\n", path, err)

			continue
		}
		fmt.Fprintf(c.stderr, "agentfeedback: warning: deleted stale temporary file %s\n", path)
	}
}

// pruneRejected deletes af1- rejected/ entries older than 30 days since
// rejected_at, with a warning and an error line in the client log each. A
// file whose rejected_at is missing or unreadable (a quarantined file) ages
// by its mtime.
func (c *Client) pruneRejected(now time.Time) {
	dir := RejectedDir(c.dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range entries {
		name := de.Name()
		if !de.Type().IsRegular() || !strings.HasPrefix(name, entryPrefix) {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r struct {
			Kind       string `json:"kind"`
			Key        string `json:"key"`
			RejectedAt string `json:"rejected_at"`
		}
		_ = json.Unmarshal(data, &r)
		at, err := time.Parse(time.RFC3339Nano, r.RejectedAt)
		if err != nil {
			info, ierr := de.Info()
			if ierr != nil {
				continue
			}
			at = info.ModTime()
		}
		if now.Sub(at) <= rejectedMaxAge {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot delete old rejected file %s (%v)\n", path, err)

			continue
		}
		fmt.Fprintf(c.stderr, "agentfeedback: warning: deleted rejected file %s (%s %s) after 30 days\n", path, r.Kind, r.Key)
		c.Log(Outcome{Outcome: OutcomeError, Kind: r.Kind, Key: r.Key, Reason: "rejected_expired"})
	}
}

// flushOne claims one due entry (stamped with the time of the claim),
// re-checks that it is still due, sends it, and files the answer with a
// fresh clock reading. It returns a reason when the pass must stop.
func (c *Client) flushOne(ctx context.Context, cand candidate, rep *FlushReport) string {
	spool := SpoolDir(c.dataDir)
	claimed := filepath.Join(spool, cand.base[:len(cand.base)-len(entrySuffix)]+inflightMarker+strconv.FormatInt(c.now().UnixNano(), 10))
	if err := os.Rename(filepath.Join(spool, cand.name), claimed); err != nil {
		// Another process took it.
		return ""
	}
	data, err := os.ReadFile(claimed)
	if err != nil {
		return ""
	}
	e, err := decodeEntry(data)
	if err != nil {
		c.quarantine(filepath.Base(claimed), err)

		return ""
	}
	if other, later := e.Destination != c.destination, e.NotBefore.After(c.now()); other || later {
		// Rescheduled by another process since the listing, or not this
		// client's (the listing checked the destination; a claim is read
		// again rather than trusted).
		if err := os.Rename(claimed, filepath.Join(spool, cand.base)); err != nil {
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot release %s (%v); it is retried once the claim is stale\n", claimed, err)
		}
		if other {
			rep.OtherDestination++
		} else {
			rep.Deferred++
		}

		return ""
	}

	res := c.send(ctx, e.Body, e.Kind, e.Key)
	now := c.now()
	e.Attempts++
	sid := SessionIDOf(e.Body)
	switch res.action {
	case actAccept:
		if err := os.Remove(claimed); err != nil {
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot delete the delivered spool file %s (%v); it may be sent again, which the key makes harmless\n", claimed, err)
		}
		rep.Flushed++
		if res.duplicate {
			rep.Duplicates++
		}
		c.Log(Outcome{Outcome: OutcomeFlushed, Kind: e.Kind, Key: e.Key, ID: res.id, RequestID: res.requestID, SessionID: sid})

		return ""
	case actRetry, actHold:
		e.LastError = res.reason
		e.NotBefore = c.nextAttempt(now, e.Attempts, res)
		if err := c.writeEntry(spool, cand.base, e); err != nil {
			// The claimed file still holds the entry; it is retried once
			// the claim goes stale.
			fmt.Fprintf(c.stderr, "agentfeedback: warning: cannot reschedule %s in %s (%v)\n", cand.base, spool, err)
		} else {
			_ = os.Remove(claimed)
		}
		rep.Pending++
		c.Log(Outcome{Outcome: OutcomeSpooled, Kind: e.Kind, Key: e.Key, RequestID: res.requestID, Reason: res.reason, SessionID: sid})
		if res.transport || res.action == actHold || c.local {
			return res.reason
		}

		return ""
	}

	o := Outcome{Outcome: OutcomeRejected, Kind: e.Kind, Key: e.Key, RequestID: res.requestID, Reason: res.reason, SessionID: sid}
	if res.action == actMismatch {
		o.Outcome = OutcomeMismatch
		rep.Mismatched++
	} else {
		rep.Rejected++
	}
	e.LastError = res.reason
	if err := c.writeRejected(cand.base, e, res, now); err != nil {
		// The claim stays in spool/: a flush may run where nobody reads
		// stderr, so deleting it would lose the payload. It is sent again
		// once the claim is stale and ages out under the spool's retention.
		o.Reason = "rejected_unwritable"
		c.echoBody(fmt.Sprintf("the rejected directory %s is not writable (%v)", RejectedDir(c.dataDir), err), e.Body)
		c.Log(o)

		return ""
	}
	_ = os.Remove(claimed)
	c.Log(o)

	return ""
}

// HasEntries reports whether the spool under data holds any af1- entry at
// all, due or not, for any destination. It changes nothing.
func HasEntries(data string) bool {
	c := &Client{dataDir: data, stderr: io.Discard, now: time.Now}

	return len(c.candidates(SpoolDir(data), c.now())) > 0
}

// HasDue reports whether the spool under data holds an entry a Flush would
// act on now: one within retention and past its not_before, or one that
// cannot be decoded, which Flush quarantines. One that cannot be read is
// not due: Flush skips it too. It reads no configuration, so it cannot tell
// an entry's destination from the caller's; a Flush then leaves a
// mismatching one in place without a request. It changes nothing.
func HasDue(data string, now time.Time) bool {
	c := &Client{dataDir: data, stderr: io.Discard, now: func() time.Time { return now }}
	spool := SpoolDir(data)
	for _, cand := range c.candidates(spool, now) {
		data, err := os.ReadFile(filepath.Join(spool, cand.name))
		if err != nil {
			continue
		}
		e, err := decodeEntry(data)
		if err != nil {
			return true
		}
		age := now.Sub(e.CreatedAt)
		friction := strings.EqualFold(strings.TrimSpace(e.Kind), "friction")
		if (friction && age > frictionMaxAge) || (!friction && age > entryMaxAge) {
			continue
		}
		if !e.NotBefore.After(now) {
			return true
		}
	}

	return false
}
