package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// HarnessOpenCode is the registry name of OpenCode V1.
const HarnessOpenCode = "opencode-v1"

// opencodeReader reads OpenCode's SQLite session store, in the schema of
// OpenCode 1.18.35 (tables session, message and part; message and part
// rows carry their content as JSON in data).
//
// The database is Env.OpenCodeDB when set (an absolute path as is, a
// relative one under DataHome/opencode), else every opencode.db and
// opencode-*.db in DataHome/opencode, opencode.db first. Only root
// sessions are listed: a sub-agent session is a child session (parent_id
// set) and is not read.
//
// The database is never written: it is opened with
// "file:<path>?immutable=1", so no lock is taken and no -wal or -shm file
// is opened or created (SQLite opens -shm read-write, creating it, even
// under mode=ro). A read during which the database file or its -wal file
// changed (existence, size or mtime) is discarded and done once more; a
// second change fails the read. Rows still only in OpenCode's write-ahead
// log, not yet checkpointed into the database file, are not seen until
// OpenCode checkpoints them, at the latest when it exits; the session then
// reads as appended.
//
// A session is laid out as a virtual append-only log: one record per
// message (ordered by time_created, id), followed by one per part of it
// (ordered by id), each the compact JSON of its kind, id and the fields of
// its data that are final once it is complete, plus '\n'. A user message
// is complete when it has a part, an assistant message when its
// time.completed is set, a tool part when its state is completed or error,
// any other part always; the first incomplete record and everything after
// it are not counted, like a final line without '\n'.
type opencodeReader struct{}

func init() { register(opencodeReader{}) }

func (opencodeReader) harness() string { return HarnessOpenCode }
func (opencodeReader) name() string    { return "opencode-sqlite" }

func (opencodeReader) location(env Env) string {
	if env.OpenCodeDB != "" {
		return opencodeDBPath(env)
	}

	return filepath.Join(env.DataHome, "opencode")
}

// opencodeDBPath is Env.OpenCodeDB, a relative one joined to
// DataHome/opencode.
func opencodeDBPath(env Env) string {
	if filepath.IsAbs(env.OpenCodeDB) {
		return env.OpenCodeDB
	}

	return filepath.Join(env.DataHome, "opencode", env.OpenCodeDB)
}

// databases lists the database files from directory entries alone. No
// database, or no absolute place to look for one, is fs.ErrNotExist.
func (r opencodeReader) databases(env Env) ([]string, error) {
	if err := env.context().Err(); err != nil {
		return nil, err
	}
	if env.OpenCodeDB != "" {
		p := opencodeDBPath(env)
		if !filepath.IsAbs(p) {
			return nil, fs.ErrNotExist
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fs.ErrNotExist
		}

		return []string{p}, nil
	}
	dir := r.location(env)
	if !filepath.IsAbs(dir) {
		return nil, fs.ErrNotExist
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if err := env.context().Err(); err != nil {
			return out, err
		}
		n := e.Name()
		if !e.Type().IsRegular() || (n != "opencode.db" && !(strings.HasPrefix(n, "opencode-") && strings.HasSuffix(n, ".db"))) {
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	if len(out) == 0 {
		return nil, fs.ErrNotExist
	}
	slices.SortStableFunc(out, func(a, b string) int {
		switch {
		case filepath.Base(a) == "opencode.db":
			return -1
		case filepath.Base(b) == "opencode.db":
			return 1
		}

		return strings.Compare(a, b)
	})

	return out, nil
}

// opencodeColumns are the tables and columns the reader needs; a database
// without one of them is not in a format it understands.
var opencodeColumns = map[string][]string{
	"session": {"id", "parent_id", "directory", "time_created", "time_updated"},
	"message": {"id", "session_id", "time_created", "data"},
	"part":    {"id", "message_id", "session_id", "data"},
}

// checkSchema reports errUnsupported when a needed table or column is
// missing.
func checkSchema(ctx context.Context, db *sql.DB) error {
	for _, table := range []string{"session", "message", "part"} {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			var n string
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()

				return err
			}
			have[n] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, col := range opencodeColumns[table] {
			if !have[col] {
				return fmt.Errorf("%w: no column %s.%s", errUnsupported, table, col)
			}
		}
	}

	return nil
}

// opencodeDSN is the immutable DSN of the database at path, so no lock is
// taken and no -wal or -shm file is opened or created. The path is
// percent-encoded (store.FileURI).
func opencodeDSN(path string) string {
	return store.FileURI(path, url.Values{"immutable": {"1"}})
}

// opencodeStat stats the database file and its -wal file; tests replace it.
var opencodeStat = os.Stat

// ocStamp is what a writer changes in the database file and its -wal file.
type ocStamp struct {
	size     int64
	mtime    time.Time
	wal      bool
	walSize  int64
	walMtime time.Time
}

// ocStampOf stats the database at path and its -wal file. A -wal file that
// does not exist is no error.
func ocStampOf(path string) (ocStamp, error) {
	info, err := opencodeStat(path)
	if err != nil {
		return ocStamp{}, err
	}
	st := ocStamp{size: info.Size(), mtime: info.ModTime()}
	wal, err := opencodeStat(path + "-wal")
	switch {
	case err == nil:
		st.wal, st.walSize, st.walMtime = true, wal.Size(), wal.ModTime()
	case !errors.Is(err, fs.ErrNotExist):
		return ocStamp{}, err
	}

	return st, nil
}

func (a ocStamp) equal(b ocStamp) bool {
	return a.size == b.size && a.mtime.Equal(b.mtime) && a.wal == b.wal && a.walSize == b.walSize && a.walMtime.Equal(b.walMtime)
}

// readOpenCode runs fn over an immutable connection to the database at
// path, closed afterwards. A read during which the database file or its
// -wal file changed is discarded and done once more; when that one changes
// too the read fails. fn may run twice and must start afresh each time.
func readOpenCode(path string, fn func(db *sql.DB) error) error {
	for range 2 {
		before, err := ocStampOf(path)
		if err != nil {
			return err
		}
		err = withOpenCode(opencodeDSN(path), fn)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if after, serr := ocStampOf(path); serr == nil && after.equal(before) {
			return err
		}
	}

	return errors.New("the database changed during the read")
}

func withOpenCode(dsn string, fn func(db *sql.DB) error) error {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	return fn(db)
}

// candidates lists the root sessions of every database from the session
// table alone; no message is read. A database that cannot be read, or a
// session id already listed from an earlier database, is reported in
// problems. When no database could be read the first error is returned.
func (r opencodeReader) candidates(env Env) ([]candidate, []problem, error) {
	ctx := env.context()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dbs, err := r.databases(env)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	var problems []problem
	var first error
	read := 0
	from := map[string]string{}
	for _, p := range dbs {
		if err := ctx.Err(); err != nil {
			return out, problems, err
		}
		var cands []candidate
		err := readOpenCode(p, func(db *sql.DB) error {
			cands = nil
			if err := checkSchema(ctx, db); err != nil {
				return err
			}
			rows, err := db.QueryContext(ctx, "SELECT id, directory, time_created, time_updated FROM session WHERE parent_id IS NULL")
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var id, dir string
				var created, updated int64
				if err := rows.Scan(&id, &dir, &created, &updated); err != nil {
					return err
				}
				cands = append(cands, candidate{sessionID: id, path: p, cwd: dir, mtime: time.UnixMilli(updated)})
			}

			return rows.Err()
		})
		if err != nil {
			if ctx.Err() != nil {
				return out, problems, ctx.Err()
			}
			if first == nil {
				first = err
			}
			problems = append(problems, problem{path: p, reason: ioReason(err)})

			continue
		}
		read++
		for _, c := range cands {
			if prev, ok := from[c.sessionID]; ok {
				problems = append(problems, problem{path: p, reason: "session " + c.sessionID + " is also in " + prev + "; listed from there"})

				continue
			}
			from[c.sessionID] = p
			out = append(out, c)
		}
	}
	if read == 0 {
		return nil, problems, first
	}

	return out, problems, nil
}

// gate applies the user policy to the session row's directory.
func (opencodeReader) gate(env Env, c candidate) (state, reason string) {
	return gateCwd(env, c)
}

// opencodeLoadHook, when set, is called with each candidate before its
// messages are read.
var opencodeLoadHook func(c candidate)

type ocRow struct {
	id   string
	data []byte
}

type ocTime struct {
	Created   *int64 `json:"created"`
	Completed *int64 `json:"completed"`
	Start     *int64 `json:"start"`
}

type ocMessage struct {
	Role  string `json:"role"`
	Time  ocTime `json:"time"`
	Error *struct {
		Name string `json:"name"`
	} `json:"error"`
	ModelID string `json:"modelID"`
	Path    struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
}

type ocPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	State     struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Error    string          `json:"error"`
		Metadata struct {
			Interrupted bool `json:"interrupted"`
			// Exit is the shell tool's exit code: an integer, or null when
			// the command was aborted or timed out.
			Exit json.RawMessage `json:"exit"`
		} `json:"metadata"`
		Time ocTime `json:"time"`
	} `json:"state"`
	Metadata struct {
		Interrupted bool `json:"interrupted"`
	} `json:"metadata"`
}

const (
	ocAborted     = "MessageAbortedError"
	ocToolAborted = "Tool execution aborted"
	ocRejected    = "The user rejected permission to use this specific tool call"
	ocRuleDenied  = "The user has specified a rule which prevents you from using this specific tool call"
	ocDismissed   = "The user dismissed this question"
	// The shell tool's notes in the <shell_metadata> block of its output.
	ocShellMeta    = "<shell_metadata>"
	ocShellAborted = "User aborted the command"
	ocShellTimeout = "shell tool terminated command after exceeding timeout"
)

// load reads the session's messages and parts and lays them out as the
// virtual log.
func (r opencodeReader) load(env Env, c candidate) (*transcript, error) {
	return r.loadCapture(env, c, nil)
}

// loadCapture renders the same virtual records used for digest offsets.
// Message and part cursors share one read transaction, so nested part reads
// do not need another connection from the single-connection database.
func (opencodeReader) loadCapture(env Env, c candidate, raw io.Writer) (*transcript, error) {
	ctx := env.context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opencodeLoadHook != nil {
		opencodeLoadHook(c)
	}
	var t *transcript
	read := func(db *sql.DB) error {
		if err := checkSchema(ctx, db); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var dir string
		if err := tx.QueryRowContext(ctx, "SELECT directory FROM session WHERE id = ?", c.sessionID).Scan(&dir); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("session %s: %w", c.sessionID, fs.ErrNotExist)
			}

			return err
		}
		if dir != c.cwd {
			return errors.New("the session's directory changed since it was listed")
		}
		t = &transcript{sessionID: c.sessionID, captureOnly: env.captureOnly || raw != nil}
		t.addCwd(dir)
		if err := t.layout(ctx, tx, raw); err != nil {
			return err
		}
		t.summarise(true)

		return tx.Commit()
	}
	var err error
	if raw == nil {
		err = readOpenCode(c.path, read)
	} else {
		// A stream cannot retract an earlier attempt. Reject a changed
		// source instead of appending a retry to bytes already emitted.
		var before, after ocStamp
		before, err = ocStampOf(c.path)
		if err == nil {
			err = withOpenCode(opencodeDSN(c.path), read)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			var stampErr error
			after, stampErr = ocStampOf(c.path)
			if stampErr != nil || !after.equal(before) {
				err = ErrCaptureChanged
			}
		}
	}
	if err != nil {
		return nil, err
	}

	return t, nil
}

// The fields of a message and of a part that the virtual log records:
// only those that are final once the row is complete, since OpenCode
// rewrites rows later (a user message gains its summary after the turn, a
// finished tool part a compaction time).
var (
	ocMessageFields = [][]string{
		{"role"}, {"time", "created"}, {"modelID"}, {"providerID"}, {"path", "cwd"}, {"error", "name"},
	}
	ocAssistantFields = [][]string{{"time", "completed"}}
	ocPartFields      = [][]string{
		{"messageID"},
		{"type"},
		{"synthetic"},
		{"ignored"},
		{"tool"},
		{"callID"},
		{"state", "status"},
		{"state", "input"},
		{"state", "output"},
		{"state", "error"},
		{"state", "metadata", "exit"},
		{"state", "metadata", "interrupted"},
	}
	ocTextFields = [][]string{{"text"}}
)

// ocRecord is the canonical record of a message or part row: the compact
// JSON of its kind, id and the projection of its data onto the recorded
// fields, and whether its data parsed as an object. Data that is not an
// object is kept as a string.
func ocRecord(kind, id string, data []byte) ([]byte, bool) {
	rec := map[string]any{"kind": kind, "id": id}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		rec["data"] = string(data)
		b, _ := json.Marshal(rec)

		return append(b, '\n'), false
	}
	fields := ocPartFields
	if kind == "message" {
		fields = ocMessageFields
		var role string
		if json.Unmarshal(obj["role"], &role) == nil && role == "assistant" {
			fields = append(slices.Clip(fields), ocAssistantFields...)
		}
	} else {
		var typ string
		if json.Unmarshal(obj["type"], &typ) == nil && (typ == "text" || typ == "reasoning") {
			fields = append(slices.Clip(fields), ocTextFields...)
		}
	}
	for _, path := range fields {
		if v, ok := ocPick(obj, path); ok {
			ocPut(rec, path, v)
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		rec = map[string]any{"kind": kind, "id": id, "data": string(data)}
		b, _ = json.Marshal(rec)
	}

	return append(b, '\n'), true
}

// ocPick is the value at path in obj.
func ocPick(obj map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	for i, k := range path {
		v, ok := obj[k]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		obj = nil
		if json.Unmarshal(v, &obj) != nil || obj == nil {
			return nil, false
		}
	}

	return nil, false
}

// ocPut sets the value at path in rec, adding the objects on the way.
func ocPut(rec map[string]any, path []string, v json.RawMessage) {
	for _, k := range path[:len(path)-1] {
		next, ok := rec[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			rec[k] = next
		}
		rec = next
	}
	rec[path[len(path)-1]] = v
}

// layout adds complete virtual records and their entry metadata to t.
// Ordered cursors carry IDs only: even without ordering indexes, SQLite
// cannot buffer or spill transcript bodies in its sort. Prepared lookups
// decode only the current message and part in the same read snapshot.
func (t *transcript) layout(ctx context.Context, tx *sql.Tx, raw io.Writer) error {
	messageData, err := tx.PrepareContext(ctx, "SELECT data FROM message WHERE id = ?")
	if err != nil {
		return err
	}
	defer func() { _ = messageData.Close() }()
	partData, err := tx.PrepareContext(ctx, "SELECT data FROM part WHERE id = ?")
	if err != nil {
		return err
	}
	defer func() { _ = partData.Close() }()
	messages, err := tx.QueryContext(ctx, "SELECT id FROM message WHERE session_id = ? ORDER BY time_created, id", t.sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = messages.Close() }()
	for messages.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var m ocRow
		if err := messages.Scan(&m.id); err != nil {
			return err
		}
		if err := messageData.QueryRowContext(ctx, m.id).Scan(&m.data); err != nil {
			return err
		}
		parts, err := tx.QueryContext(ctx, "SELECT id FROM part WHERE session_id = ? AND message_id = ? ORDER BY id", t.sessionID, m.id)
		if err != nil {
			return err
		}
		hasParts := parts.Next()
		complete, err := t.layoutMessage(ctx, m, parts, partData, hasParts, raw)
		_ = parts.Close()
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
	}

	return messages.Err()
}

// ocAdd counts a record only after its bytes have been emitted.
func (t *transcript) ocAdd(raw io.Writer, rec []byte, ok bool) (int64, error) {
	at := t.size
	if raw != nil {
		if _, err := raw.Write(rec); err != nil {
			return at, err
		}
	}
	if at == 0 {
		sum := sha256.Sum256(rec[:len(rec)-1])
		t.head = hex.EncodeToString(sum[:])
	}
	if ok {
		t.parsed++
	} else {
		t.badLines = append(t.badLines, at)
	}
	t.size += int64(len(rec))

	return at, nil
}

// layoutMessage returns false at the first incomplete virtual record.
// An aborted assistant still inspects later parts for an interrupted tool:
// those parts suppress a duplicate message interrupt even beyond the prefix.
func (t *transcript) layoutMessage(ctx context.Context, m ocRow, parts *sql.Rows, partData *sql.Stmt, hasParts bool, raw io.Writer) (bool, error) {
	if err := parts.Err(); err != nil {
		return false, err
	}
	rec, ok := ocRecord("message", m.id, m.data)
	var msg ocMessage
	if ok && json.Unmarshal(m.data, &msg) != nil {
		msg = ocMessage{}
	}
	if ok && (msg.Role == "assistant" && msg.Time.Completed == nil || msg.Role == "user" && !hasParts) {
		return false, nil
	}
	at, err := t.ocAdd(raw, rec, ok)
	if err != nil {
		return false, err
	}
	messageEntry := len(t.entries)
	aborted := ok && msg.Role == "assistant" && msg.Error != nil && msg.Error.Name == ocAborted
	if ok {
		e := entry{offset: at, span: m.id, kind: kindOther, cwd: t.lastCwd}
		if msg.Time.Created != nil {
			e.at = t.seen(time.UnixMilli(*msg.Time.Created))
		}
		if msg.Role == "assistant" {
			e.model = msg.ModelID
			if cwd := t.addCwd(msg.Path.Cwd); cwd != "" {
				e.cwd = cwd
			}
		}
		t.addEntry(e)
	}
	var texts []string
	prompt, interrupted, complete := false, false, true
	for more := hasParts; more; more = parts.Next() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var pr ocRow
		if err := parts.Scan(&pr.id); err != nil {
			return false, err
		}
		if err := partData.QueryRowContext(ctx, pr.id).Scan(&pr.data); err != nil {
			return false, err
		}
		var p ocPart
		if json.Unmarshal(pr.data, &p) != nil {
			p = ocPart{}
		} else if aborted && p.Type == "tool" && ocInterrupted(&p) {
			interrupted = true
		}
		if !complete {
			if interrupted {
				break
			}
			continue
		}
		prec, pok := ocRecord("part", pr.id, pr.data)
		if pok && p.Type == "tool" && p.State.Status != "completed" && p.State.Status != "error" {
			complete = false
			if !aborted || interrupted {
				break
			}
			continue
		}
		pat, err := t.ocAdd(raw, prec, pok)
		if err != nil {
			return false, err
		}
		if !pok {
			continue
		}
		if msg.Role == "user" && p.Type == "text" {
			if !p.Synthetic && !p.Ignored {
				prompt = prompt || strings.TrimSpace(p.Text) != ""
				if !t.captureOnly {
					texts = append(texts, p.Text)
				}
			}
			continue
		}
		e := entry{offset: pat, span: pr.id, kind: kindOther, cwd: t.lastCwd}
		switch p.Type {
		case "tool":
			c := &call{id: p.CallID, tool: p.Tool, result: true, resultOffset: pat}
			c.status, c.exitCode = toolStatus(&p)
			if !t.captureOnly {
				c.input = p.State.Input
				var in struct {
					Command *string `json:"command"`
				}
				if json.Unmarshal(p.State.Input, &in) == nil && in.Command != nil {
					c.command, c.hasCommand = *in.Command, true
				}
				if p.State.Status == "completed" {
					c.content = p.State.Output
				} else {
					c.content = p.State.Error
				}
			}
			if p.State.Time.Start != nil {
				e.at = t.seen(time.UnixMilli(*p.State.Time.Start))
			}
			e.kind, e.calls = kindToolCall, []*call{c}
		case "text":
			if msg.Role == "assistant" && !p.Synthetic && !p.Ignored && strings.TrimSpace(p.Text) != "" {
				e.kind = kindAssistantText
				if !t.captureOnly {
					e.text = p.Text
				}
			}
		}
		t.addEntry(e)
	}
	if err := parts.Err(); err != nil {
		return false, err
	}
	if ok {
		e := &t.entries[messageEntry]
		if prompt {
			e.kind = kindPrompt
			if !t.captureOnly {
				e.text = strings.Join(texts, "\n")
			}
		} else if aborted && !interrupted {
			e.kind = kindInterrupt
		}
	}

	return complete, nil
}

func ocInterrupted(p *ocPart) bool {
	status, _ := toolStatus(p)

	return status == statusInterrupted
}

// toolStatus classifies a complete tool part and returns the exit code it
// records. A completed part is ok unless its metadata carries an exit: a
// non-zero integral number is an error with that exit code; null with the shell
// tool's abort note in the output is interrupted, with its timeout note an
// error without exit code. An error part is interrupted when aborted,
// denied when the user or a rule refused it or a question was dismissed,
// else an error.
func toolStatus(p *ocPart) (string, *int) {
	st := &p.State
	if st.Status == "completed" {
		exit := bytes.TrimSpace(st.Metadata.Exit)
		switch {
		case len(exit) == 0:
			return statusOK, nil
		case string(exit) == "null":
			_, note, _ := strings.Cut(st.Output, ocShellMeta)
			switch {
			case strings.Contains(note, ocShellAborted):
				return statusInterrupted, nil
			case strings.Contains(note, ocShellTimeout):
				return statusError, nil
			}

			return statusOK, nil
		}
		var f float64
		if json.Unmarshal(exit, &f) == nil && f != 0 && f == math.Trunc(f) && math.Abs(f) <= math.MaxInt32 {
			n := int(f)

			return statusError, &n
		}

		return statusOK, nil
	}
	switch {
	case st.Metadata.Interrupted || p.Metadata.Interrupted || strings.HasPrefix(st.Error, ocToolAborted):
		return statusInterrupted, nil
	case strings.Contains(st.Error, ocRejected), strings.Contains(st.Error, ocRuleDenied), strings.Contains(st.Error, ocDismissed):
		return statusDenied, nil
	default:
		return statusError, nil
	}
}
