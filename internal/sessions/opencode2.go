package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// HarnessOpenCodeV2 is the registry name of OpenCode 2.
const HarnessOpenCodeV2 = "opencode-v2"

// opencodeV2Reader reads OpenCode 2's SQLite session store, in the schema
// of OpenCode 2.2.6 (tables session_v2, session_message and, when present,
// session_part; message and part rows carry their content as JSON in
// data, the message's kind in type).
//
// The databases, their read-only immutable opening and the retry of a read
// during which a database changed are those of opencodeReader. Only a
// database with a session_v2 table is read, and opencodeReader skips it:
// OpenCode 2 migrates a V1 database in place, keeping V1's tables and
// adding session_v2 rows with the same session ids, so the database is
// listed under one harness only. Only root sessions (parent_id NULL) are
// listed.
//
// A session is laid out as a virtual append-only log: one record per
// session_message row (ordered by seq), followed by one per session_part
// row of it (ordered by ordinal), each the compact JSON of its kind, id,
// type and the fields of its data that are final once it is complete, plus
// '\n'. A part's id is "<message id>/<ordinal>". An assistant message is
// complete when its time.completed is set and none of its tool contents is
// streaming or running, a part when it is not a streaming or running tool,
// any other message always; the first incomplete record and everything
// after it are not counted, like a final line without '\n'.
type opencodeV2Reader struct{}

func init() { register(opencodeV2Reader{}) }

func (opencodeV2Reader) harness() string { return HarnessOpenCodeV2 }
func (opencodeV2Reader) name() string    { return "opencode-v2-sqlite" }

func (opencodeV2Reader) location(env Env) string { return opencodeReader{}.location(env) }

// opencodeV2Columns are the tables and columns the reader needs; a
// database without one of them is not in a format it understands.
// session_part is read only when it exists (the 2.2.6 samples leave it
// empty).
var opencodeV2Columns = map[string][]string{
	"session_v2":      {"id", "parent_id", "directory", "time_created", "time_updated"},
	"session_message": {"id", "session_id", "type", "seq", "time_created", "data"},
	"session_part":    {"message_id", "ordinal", "type", "data"},
}

// checkSchemaV2 reports errUnsupported when a needed table or column is
// missing, and whether the database has a session_part table.
func checkSchemaV2(ctx context.Context, db *sql.DB) (bool, error) {
	parts, err := ocHasTable(ctx, db, "session_part")
	if err != nil {
		return false, err
	}
	tables := []string{"session_v2", "session_message"}
	if parts {
		tables = append(tables, "session_part")
	}

	return parts, checkColumns(ctx, db, tables, opencodeV2Columns)
}

// candidates lists the root sessions of every database with a session_v2
// table; a database without one is skipped silently (errOtherVariant).
func (opencodeV2Reader) candidates(env Env) ([]candidate, []problem, error) {
	return ocCandidates(env, opencodeReader{}.databases, func(ctx context.Context, db *sql.DB, p string) ([]candidate, error) {
		if v2, err := ocHasTable(ctx, db, "session_v2"); err != nil {
			return nil, err
		} else if !v2 {
			return nil, errOtherVariant
		}
		if _, err := checkSchemaV2(ctx, db); err != nil {
			return nil, err
		}

		return ocRootSessions(ctx, db, p, "SELECT id, directory, time_created, time_updated FROM session_v2 WHERE parent_id IS NULL")
	})
}

// gate applies the user policy to the session row's directory.
func (opencodeV2Reader) gate(env Env, c candidate) (state, reason string) {
	return gateCwd(env, c)
}

func (r opencodeV2Reader) load(env Env, c candidate) (*transcript, error) {
	return r.loadCapture(env, c, nil)
}

// loadCapture reads the session and writes its virtual records to raw when
// raw is not nil, as opencodeReader.loadCapture does.
func (opencodeV2Reader) loadCapture(env Env, c candidate, raw io.Writer) (*transcript, error) {
	ctx := env.context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opencodeLoadHook != nil {
		opencodeLoadHook(c)
	}
	var t *transcript
	read := func(db *sql.DB) error {
		parts, err := checkSchemaV2(ctx, db)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var dir string
		if err := tx.QueryRowContext(ctx, "SELECT directory FROM session_v2 WHERE id = ?", c.sessionID).Scan(&dir); err != nil {
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
		if err := t.locationsV2(ctx, tx); err != nil {
			return err
		}
		t.lastCwd = dir
		if err := t.layoutV2(ctx, tx, parts, raw); err != nil {
			return err
		}
		t.summarise(true)

		return tx.Commit()
	}
	if err := readOpenCodeCapture(c.path, raw != nil, read); err != nil {
		return nil, err
	}

	return t, nil
}

type oc2Item struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status string `json:"status"`
		// Input is an object, a string while the call streams.
		Input   json.RawMessage `json:"input"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Metadata struct {
			// Exit is the shell tool's exit code.
			Exit json.RawMessage `json:"exit"`
		} `json:"metadata"`
	} `json:"state"`
	Time struct {
		Created *int64 `json:"created"`
	} `json:"time"`
}

type oc2Message struct {
	Time struct {
		Created   *int64 `json:"created"`
		Completed *int64 `json:"completed"`
	} `json:"time"`
	Text  string `json:"text"`
	Model struct {
		ID string `json:"id"`
	} `json:"model"`
	Outcome string    `json:"outcome"`
	Content []oc2Item `json:"content"`
	// Location and Previous are a location-switched message's directories.
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Previous struct {
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
	} `json:"previous"`
}

// The fields the virtual log records: only those that are final once the
// row is complete. tokens, cost, snapshot and time.streamed are left out:
// OpenCode may rewrite them.
var (
	oc2MessageFields = [][]string{{"time", "created"}}
	oc2TypeFields    = map[string][][]string{
		"user":              {{"text"}},
		"system":            {{"text"}},
		"synthetic":         {{"text"}},
		"assistant":         {{"time", "completed"}, {"model", "id"}, {"model", "providerID"}, {"finish"}},
		"idle":              {{"outcome"}},
		"location-switched": {{"location", "directory"}, {"previous", "location", "directory"}},
	}
	oc2ItemFields = [][]string{
		{"type"}, {"text"}, {"id"}, {"name"},
		{"state", "status"}, {"state", "input"}, {"state", "content"}, {"state", "error"}, {"state", "metadata", "exit"},
	}
)

// oc2Project is the projection of obj onto fields; a tool state's content
// array is projected item by item (oc2Content).
func oc2Project(obj map[string]json.RawMessage, fields [][]string) map[string]any {
	out := map[string]any{}
	for _, path := range fields {
		if v, ok := ocPick(obj, path); ok {
			if len(path) == 2 && path[0] == "state" && path[1] == "content" {
				v = oc2Content(v)
			}
			ocPut(out, path, v)
		}
	}

	return out
}

// oc2CutDataURI cuts a data: URI (in any case) to its header, so a string
// content item carries no inline payload.
func oc2CutDataURI(s string) string {
	if len(s) < 5 || !strings.EqualFold(s[:5], "data:") {
		return s
	}
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i+1]
	}

	return s[:5]
}

// oc2Content projects a tool content array: a text item keeps its type and
// text, a file item its type, mime, name and uri, with a data: URI (in any
// case) cut to "data:<mime>;base64," as OpenCode's own omitMessageFileData
// does, so no file payload reaches the log; any other object keeps its
// type. A string item keeps its text with a data: URI cut to its header;
// any other item that is not an object is kept as it is, as is a value
// that is not an array.
func oc2Content(v json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(v, &items) != nil || items == nil {
		return v
	}
	out := make([]json.RawMessage, len(items))
	for i, raw := range items {
		var it map[string]json.RawMessage
		if json.Unmarshal(raw, &it) != nil || it == nil {
			out[i] = raw
			var s string
			if json.Unmarshal(raw, &s) == nil {
				out[i], _ = json.Marshal(oc2CutDataURI(s))
			}

			continue
		}
		var typ string
		_ = json.Unmarshal(it["type"], &typ)
		keep := []string{"type"}
		switch typ {
		case "text":
			keep = append(keep, "text")
		case "file":
			keep = append(keep, "mime", "name", "uri")
		}
		o := map[string]json.RawMessage{}
		for _, k := range keep {
			if x, ok := it[k]; ok {
				o[k] = x
			}
		}
		var uri, mime string
		if typ == "file" && json.Unmarshal(o["uri"], &uri) == nil && len(uri) >= 5 && strings.EqualFold(uri[:5], "data:") {
			_ = json.Unmarshal(o["mime"], &mime)
			o["uri"], _ = json.Marshal("data:" + mime + ";base64,")
		}
		b, err := json.Marshal(o)
		if err != nil {
			return v
		}
		out[i] = b
	}
	b, err := json.Marshal(out)
	if err != nil {
		return v
	}

	return b
}

// oc2Record is the canonical record of a message or part row and whether
// its data parsed as an object; data that is not an object is kept as a
// string. A message's content items are projected one by one; a part's
// data is projected like a content item.
func oc2Record(kind, id, typ string, data []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		b, _ := json.Marshal(map[string]any{"kind": kind, "id": id, "type": typ, "data": string(data)})

		return append(b, '\n'), false
	}
	var rec map[string]any
	if kind == "part" {
		rec = oc2Project(obj, oc2ItemFields)
	} else {
		rec = oc2Project(obj, append(append([][]string(nil), oc2MessageFields...), oc2TypeFields[typ]...))
		if typ == "assistant" {
			var items []map[string]json.RawMessage
			if json.Unmarshal(obj["content"], &items) == nil && items != nil {
				content := make([]map[string]any, len(items))
				for i, it := range items {
					content[i] = oc2Project(it, oc2ItemFields)
				}
				rec["content"] = content
			}
		}
	}
	rec["kind"], rec["id"], rec["type"] = kind, id, typ
	b, err := json.Marshal(rec)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"kind": kind, "id": id, "type": typ, "data": string(data)})
	}

	return append(b, '\n'), true
}

// oc2Pending reports whether a tool item is not final yet.
func oc2Pending(it *oc2Item) bool {
	return it.Type == "tool" && (it.State.Status == "streaming" || it.State.Status == "running")
}

// locationsV2 adds the previous and new directory of every
// location-switched row of the session, before the layout: the policy sees
// them all even when the log is cut before a switch.
func (t *transcript) locationsV2(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT data FROM session_message WHERE session_id = ? AND type = 'location-switched' ORDER BY seq", t.sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return err
		}
		var msg oc2Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		t.addCwd(msg.Previous.Location.Directory)
		t.addCwd(msg.Location.Directory)
	}

	return rows.Err()
}

// layoutV2 adds complete virtual records and their entries to t. As in
// layout, ordered cursors carry keys only and prepared lookups fetch the
// current row's data in the same read snapshot.
func (t *transcript) layoutV2(ctx context.Context, tx *sql.Tx, withParts bool, raw io.Writer) error {
	messageData, err := tx.PrepareContext(ctx, "SELECT type, data FROM session_message WHERE id = ?")
	if err != nil {
		return err
	}
	defer func() { _ = messageData.Close() }()
	var partData *sql.Stmt
	if withParts {
		if partData, err = tx.PrepareContext(ctx, "SELECT type, data FROM session_part WHERE message_id = ? AND ordinal = ?"); err != nil {
			return err
		}
		defer func() { _ = partData.Close() }()
	}
	var partsOf *sql.Stmt
	if withParts {
		if partsOf, err = tx.PrepareContext(ctx, "SELECT EXISTS (SELECT 1 FROM session_part WHERE message_id = ?)"); err != nil {
			return err
		}
		defer func() { _ = partsOf.Close() }()
	}
	messages, err := tx.QueryContext(ctx, "SELECT id FROM session_message WHERE session_id = ? ORDER BY seq", t.sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = messages.Close() }()
	for messages.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var id, typ string
		var data []byte
		if err := messages.Scan(&id); err != nil {
			return err
		}
		if err := messageData.QueryRowContext(ctx, id).Scan(&typ, &data); err != nil {
			return err
		}
		hasParts := false
		if withParts {
			if err := partsOf.QueryRowContext(ctx, id).Scan(&hasParts); err != nil {
				return err
			}
		}
		complete, err := t.layoutMessageV2(id, typ, data, hasParts, raw)
		if err != nil || !complete {
			return err
		}
		if !withParts {
			continue
		}
		if complete, err = t.layoutPartsV2(ctx, tx, partData, id, raw); err != nil || !complete {
			return err
		}
	}

	return messages.Err()
}

// layoutMessageV2 returns false when the message is incomplete. A message
// with session_part rows (hasParts) takes its items from them: under
// experimental.part_rows OpenCode writes part rows instead of rewriting the
// message, so its embedded content is ignored and no call counts twice. A
// row whose data is an object that does not decode as a message counts as
// complete with no content, as layout treats an undecodable V1 row.
func (t *transcript) layoutMessageV2(id, typ string, data []byte, hasParts bool, raw io.Writer) (bool, error) {
	rec, ok := oc2Record("message", id, typ, data)
	var msg oc2Message
	decoded := ok && json.Unmarshal(data, &msg) == nil
	if !decoded || hasParts {
		msg.Content = nil
	}
	if !decoded {
		msg = oc2Message{}
	}
	if decoded && typ == "assistant" {
		if msg.Time.Completed == nil {
			return false, nil
		}
		for i := range msg.Content {
			if oc2Pending(&msg.Content[i]) {
				return false, nil
			}
		}
	}
	at, err := t.ocAdd(raw, rec, ok)
	if err != nil || !ok {
		return true, err
	}
	e := entry{offset: at, span: id, kind: kindOther, cwd: t.lastCwd}
	if msg.Time.Created != nil {
		e.at = t.seen(time.UnixMilli(*msg.Time.Created))
	}
	switch typ {
	case "user":
		if strings.TrimSpace(msg.Text) != "" {
			e.kind = kindPrompt
			if !t.captureOnly {
				e.text = msg.Text
			}
		}
	case "idle":
		if msg.Outcome == "interrupted" {
			e.kind = kindInterrupt
		}
	case "assistant":
		e.model = msg.Model.ID
	case "skill":
		// A skill the user invoked: the row names it, the tool is not run.
		var sk struct {
			Name  string          `json:"name"`
			Skill json.RawMessage `json:"skill"`
		}
		if json.Unmarshal(data, &sk) == nil {
			name := sk.Name
			if name == "" {
				_ = json.Unmarshal(sk.Skill, &name)
			}
			t.addSkillHit(name, id, at)
		}
	case "location-switched":
		// locationsV2 has added both directories already.
		if cwd := t.addCwd(msg.Location.Directory); cwd != "" {
			e.cwd = cwd
		}
	}
	t.addEntry(e)
	if typ == "assistant" {
		for i := range msg.Content {
			t.addItemV2(&msg.Content[i], at, id+"/"+strconv.Itoa(i), e.at)
		}
	}

	return true, nil
}

// layoutPartsV2 lays out the message's session_part rows; false at the
// first incomplete one.
func (t *transcript) layoutPartsV2(ctx context.Context, tx *sql.Tx, partData *sql.Stmt, messageID string, raw io.Writer) (bool, error) {
	parts, err := tx.QueryContext(ctx, "SELECT ordinal FROM session_part WHERE message_id = ? ORDER BY ordinal", messageID)
	if err != nil {
		return false, err
	}
	defer func() { _ = parts.Close() }()
	for parts.Next() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var ordinal int64
		if err := parts.Scan(&ordinal); err != nil {
			return false, err
		}
		var typ string
		var data []byte
		if err := partData.QueryRowContext(ctx, messageID, ordinal).Scan(&typ, &data); err != nil {
			return false, err
		}
		pid := messageID + "/" + strconv.FormatInt(ordinal, 10)
		rec, ok := oc2Record("part", pid, typ, data)
		var it oc2Item
		if ok && json.Unmarshal(data, &it) != nil {
			it = oc2Item{}
		}
		if ok && oc2Pending(&it) {
			return false, nil
		}
		at, err := t.ocAdd(raw, rec, ok)
		if err != nil {
			return false, err
		}
		if ok {
			t.addItemV2(&it, at, pid, time.Time{})
		}
	}

	return true, parts.Err()
}

// addItemV2 adds the entry of a complete content item: a tool call, or
// assistant text.
func (t *transcript) addItemV2(it *oc2Item, at int64, span string, when time.Time) {
	e := entry{offset: at, span: span, at: when, kind: kindOther, cwd: t.lastCwd}
	switch it.Type {
	case "tool":
		c := &call{id: it.ID, tool: it.Name, result: true, resultOffset: at}
		c.status, c.exitCode = oc2ToolStatus(it)
		if it.Name == ocSkillTool {
			var in struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(it.State.Input, &in) == nil {
				t.addSkillHit(in.ID, span, at)
			}
		}
		if !t.captureOnly {
			c.input = it.State.Input
			var in struct {
				Command *string `json:"command"`
			}
			if json.Unmarshal(it.State.Input, &in) == nil && in.Command != nil {
				c.command, c.hasCommand = *in.Command, true
			}
			var texts []string
			for _, ct := range it.State.Content {
				if ct.Type == "text" {
					texts = append(texts, ct.Text)
				}
			}
			c.content = strings.Join(texts, "")
			if it.State.Error != nil && c.content == "" {
				c.content = it.State.Error.Message
			}
		}
		if it.Time.Created != nil {
			e.at = t.seen(time.UnixMilli(*it.Time.Created))
		}
		e.kind, e.calls = kindToolCall, []*call{c}
	case "text":
		if strings.TrimSpace(it.Text) != "" {
			e.kind = kindAssistantText
			if !t.captureOnly {
				e.text = it.Text
			}
		}
	}
	t.addEntry(e)
}

// oc2ToolStatus classifies a complete tool item and returns the exit code
// it records. A completed item is classified by its metadata exit as V1's
// completed part is (a non-zero integral exit is an error with that code).
// An error item is an error: OpenCode 2's error type and message for an
// aborted, denied or dismissed call were not observed (none in the 2.2.6
// samples), so neither interrupted nor denied is told apart.
func oc2ToolStatus(it *oc2Item) (string, *int) {
	if it.State.Status != "completed" {
		return statusError, nil
	}
	var p ocPart
	p.State.Status = "completed"
	p.State.Metadata.Exit = it.State.Metadata.Exit

	return toolStatus(&p)
}
