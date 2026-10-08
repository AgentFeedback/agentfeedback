package sessions

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// HarnessOpenCode is the registry name of OpenCode.
const HarnessOpenCode = "opencode"

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
func checkSchema(db *sql.DB) error {
	for _, table := range []string{"session", "message", "part"} {
		rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()

				return err
			}
			have[n] = true
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
// percent-encoded.
func opencodeDSN(path string) string {
	p := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()

	return "file:" + p + "?immutable=1"
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
		var cands []candidate
		err := readOpenCode(p, func(db *sql.DB) error {
			cands = nil
			if err := checkSchema(db); err != nil {
				return err
			}
			rows, err := db.Query("SELECT id, directory, time_created, time_updated FROM session WHERE parent_id IS NULL")
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
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
	id, messageID string
	data          []byte
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
func (opencodeReader) load(_ Env, c candidate) (*transcript, error) {
	if opencodeLoadHook != nil {
		opencodeLoadHook(c)
	}
	var dir string
	var messages, parts []ocRow
	err := readOpenCode(c.path, func(db *sql.DB) error {
		messages, parts = nil, nil
		if err := checkSchema(db); err != nil {
			return err
		}
		if err := db.QueryRow("SELECT directory FROM session WHERE id = ?", c.sessionID).Scan(&dir); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("session %s: %w", c.sessionID, fs.ErrNotExist)
			}

			return err
		}
		if dir != c.cwd {
			return errors.New("the session's directory changed since it was listed")
		}
		var err error
		if messages, err = ocRows(db, "SELECT id, '', data FROM message WHERE session_id = ? ORDER BY time_created, id", c.sessionID); err != nil {
			return err
		}
		parts, err = ocRows(db, "SELECT id, message_id, data FROM part WHERE session_id = ? ORDER BY message_id, id", c.sessionID)

		return err
	})
	if err != nil {
		return nil, err
	}
	t := &transcript{sessionID: c.sessionID}
	t.addCwd(dir)
	byMessage := map[string][]ocRow{}
	for _, p := range parts {
		byMessage[p.messageID] = append(byMessage[p.messageID], p)
	}
	t.layout(messages, byMessage)
	t.summarise(true)

	return t, nil
}

func ocRows(db *sql.DB, query, sessionID string) ([]ocRow, error) {
	rows, err := db.Query(query, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ocRow
	for rows.Next() {
		var r ocRow
		var data string
		if err := rows.Scan(&r.id, &r.messageID, &data); err != nil {
			return nil, err
		}
		r.data = []byte(data)
		out = append(out, r)
	}

	return out, rows.Err()
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
		{"messageID"}, {"type"}, {"synthetic"}, {"ignored"}, {"tool"}, {"callID"},
		{"state", "status"}, {"state", "input"}, {"state", "output"}, {"state", "error"},
		{"state", "metadata", "exit"}, {"state", "metadata", "interrupted"},
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

// layout adds the complete records of the virtual log to t: the line
// bookkeeping of scanJSONL, and the entries.
func (t *transcript) layout(messages []ocRow, parts map[string][]ocRow) {
	var offset int64
	// add counts one complete record and reports whether the log goes on.
	add := func(rec []byte, ok bool) int64 {
		at := offset
		if offset == 0 {
			sum := sha256.Sum256(rec[:len(rec)-1])
			t.head = hex.EncodeToString(sum[:])
		}
		if ok {
			t.parsed++
		} else {
			t.badLines = append(t.badLines, at)
		}
		offset += int64(len(rec))

		return at
	}
	defer func() { t.size = offset }()
	for _, m := range messages {
		rec, ok := ocRecord("message", m.id, m.data)
		var msg ocMessage
		if ok && json.Unmarshal(m.data, &msg) != nil {
			msg = ocMessage{}
		}
		mine := parts[m.id]
		if ok && (msg.Role == "assistant" && msg.Time.Completed == nil || msg.Role == "user" && len(mine) == 0) {
			return
		}
		// The parts of the message that are complete, up to the first
		// that is not.
		type parsedPart struct {
			row ocRow
			rec []byte
			ok  bool
			p   ocPart
		}
		var done []parsedPart
		complete := true
		for _, pr := range mine {
			prec, pok := ocRecord("part", pr.id, pr.data)
			var p ocPart
			if pok && json.Unmarshal(pr.data, &p) != nil {
				p = ocPart{}
			}
			if pok && p.Type == "tool" && p.State.Status != "completed" && p.State.Status != "error" {
				complete = false

				break
			}
			done = append(done, parsedPart{pr, prec, pok, p})
		}

		at := add(rec, ok)
		if ok {
			e := entry{offset: at, span: m.id, kind: kindOther, cwd: t.lastCwd}
			if msg.Time.Created != nil {
				e.at = t.seen(time.UnixMilli(*msg.Time.Created))
			}
			switch msg.Role {
			case "user":
				var texts []string
				for _, d := range done {
					if d.ok && d.p.Type == "text" && !d.p.Synthetic && !d.p.Ignored {
						texts = append(texts, d.p.Text)
					}
				}
				if s := strings.Join(texts, "\n"); strings.TrimSpace(s) != "" {
					e.kind, e.text = kindPrompt, s
				}
			case "assistant":
				e.model = msg.ModelID
				if cwd := t.addCwd(msg.Path.Cwd); cwd != "" {
					e.cwd = cwd
				}
				if msg.Error != nil && msg.Error.Name == ocAborted && !slices.ContainsFunc(mine, func(pr ocRow) bool {
					var p ocPart

					return json.Unmarshal(pr.data, &p) == nil && p.Type == "tool" && ocInterrupted(&p)
				}) {
					e.kind = kindInterrupt
				}
			}
			t.entries = append(t.entries, e)
		}
		for _, d := range done {
			pat := add(d.rec, d.ok)
			if !d.ok {
				continue
			}
			if msg.Role == "user" && d.p.Type == "text" {
				continue
			}
			e := entry{offset: pat, span: d.row.id, kind: kindOther, cwd: t.lastCwd}
			switch d.p.Type {
			case "tool":
				c := &call{id: d.p.CallID, tool: d.p.Tool, input: d.p.State.Input, result: true, resultOffset: pat}
				c.status, c.exitCode = toolStatus(&d.p)
				var in struct {
					Command *string `json:"command"`
				}
				if json.Unmarshal(d.p.State.Input, &in) == nil && in.Command != nil {
					c.command, c.hasCommand = *in.Command, true
				}
				if d.p.State.Status == "completed" {
					c.content = d.p.State.Output
				} else {
					c.content = d.p.State.Error
				}
				if d.p.State.Time.Start != nil {
					e.at = t.seen(time.UnixMilli(*d.p.State.Time.Start))
				}
				e.kind, e.calls = kindToolCall, []*call{c}
			case "text":
				if msg.Role == "assistant" && !d.p.Synthetic && !d.p.Ignored && strings.TrimSpace(d.p.Text) != "" {
					e.kind, e.text = kindAssistantText, d.p.Text
				}
			}
			t.entries = append(t.entries, e)
		}
		if !complete {
			return
		}
	}
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
