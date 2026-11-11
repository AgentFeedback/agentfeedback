package sessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
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

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// HarnessOMP is the registry name of oh-my-pi.
const HarnessOMP = "omp"

// ompReader reads the persisted journal, including retained branches, not the
// current model context. No parent session, child transcript, blob or reasoning
// payload is followed. Schemas: oh-my-pi session-entries.ts and ai/types.ts.
type ompReader struct{}

func init() { register(ompReader{}) }

func (ompReader) harness() string { return HarnessOMP }
func (ompReader) name() string    { return "omp-jsonl" }
func (ompReader) location(env Env) string {
	if filepath.IsAbs(env.OMPSessionsDir) {
		return filepath.Clean(env.OMPSessionsDir)
	}
	if !filepath.IsAbs(env.Home) {
		return ""
	}
	return filepath.Join(env.Home, ".omp", "agent", "sessions")
}

const (
	ompHeaderCap = 1 << 20
	ompRecordCap = 16 << 20
)

var errOMPChanged = errors.New("the omp session changed since its policy was checked")

type ompMeta struct {
	Type                  string   `json:"type"`
	Version               int      `json:"version"`
	ID                    string   `json:"id"`
	Cwd                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories"`
	Timestamp             string   `json:"timestamp"`
}

func ompID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\#\x00\r\n")
}

// ompHeader consumes only the optional title and metadata, never buffering the
// following transcript. Pretty-printed metadata is accepted within a fixed cap.
func ompHeader(r io.Reader) ([]byte, *ompMeta, error) {
	var prefix []byte
	recordStart := 0
	var one [1]byte
	depth := 0
	quoted, escaped := false, false
	title := false
	for len(prefix) < ompHeaderCap {
		n, err := r.Read(one[:])
		if n > 0 {
			b := one[0]
			prefix = append(prefix, b)
			if quoted {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					quoted = false
				}
			} else {
				switch b {
				case '"':
					quoted = true
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
			if b == '\n' && depth <= 0 && !quoted {
				body := bytes.TrimSpace(prefix[recordStart:])
				if len(body) == 0 {
					recordStart = len(prefix)
					continue
				}
				var typ struct {
					Type      string  `json:"type"`
					V         int     `json:"v"`
					Title     *string `json:"title"`
					UpdatedAt *string `json:"updatedAt"`
					Pad       *string `json:"pad"`
					Source    string  `json:"source"`
				}
				if json.Unmarshal(body, &typ) != nil {
					return prefix, nil, fmt.Errorf("malformed omp header: %w", errUnsupported)
				}
				if typ.Type == "title" && !title && typ.V == 1 && len(prefix)-recordStart <= 256 && typ.Title != nil && typ.UpdatedAt != nil && typ.Pad != nil && (typ.Source == "" || typ.Source == "auto" || typ.Source == "user") {
					title = true
					recordStart = len(prefix)
					continue
				}
				m := ompMeta{Version: 1}
				if typ.Type != "session" || json.Unmarshal(body, &m) != nil || !ompID(m.ID) {
					return prefix, nil, fmt.Errorf("invalid omp session header: %w", errUnsupported)
				}
				return prefix, &m, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return prefix, nil, fmt.Errorf("incomplete omp header: %w", errUnsupported)
			}
			return prefix, nil, err
		}
	}
	return prefix, nil, fmt.Errorf("omp header exceeds byte limit: %w", errUnsupported)
}

// open validates every ancestor and the descriptor before any bytes are read.
// os.Root confines resolution during ancestor replacement; the shared
// nonblocking opener also prevents a raced FIFO from waiting for a writer.
func (r ompReader) open(env Env, path string) (io.ReadCloser, error) {
	root := r.location(env)
	before, err := cursorPathInfo(root, path)
	if err != nil {
		return nil, err
	}
	if env.Open == nil {
		env.Open = func(path string) (io.ReadCloser, error) {
			dir, err := os.OpenRoot(root)
			if err != nil {
				return nil, err
			}
			defer func() { _ = dir.Close() }()
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			return openCursorFile(dir, rel)
		}
	}
	f, err := env.open(path)
	if err != nil {
		return nil, err
	}
	after, pathErr := cursorPathInfo(root, path)
	opened, statErr := cursorOpenedInfo(f)
	if pathErr != nil || statErr != nil || !os.SameFile(before, after) || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, errOMPChanged
	}
	return f, nil
}

func (r ompReader) candidates(env Env) ([]candidate, []problem, error) {
	root := r.location(env)
	if root == "" {
		return nil, nil, fs.ErrNotExist
	}
	// Validate directories too, without following symlinks in their ancestors.
	checkDir := func(path string) error {
		for p := filepath.Clean(path); ; p = filepath.Dir(p) {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("omp session directory contains a symlink or non-directory")
			}
			if filepath.Dir(p) == p {
				return nil
			}
		}
	}
	if err := checkDir(root); err != nil {
		return nil, nil, err
	}
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	var problems []problem
	add := func(dir string, f fs.DirEntry) {
		if !strings.HasSuffix(f.Name(), ".jsonl") || !f.Type().IsRegular() || env.Policy.Disabled {
			return
		}
		path := filepath.Join(dir, f.Name())
		info, err := f.Info()
		if err != nil {
			problems = append(problems, problem{path: path, reason: ioReason(err)})
			return
		}
		opened, err := r.open(env, path)
		if err != nil {
			problems = append(problems, problem{path: path, reason: ioReason(err)})
			return
		}
		_, meta, err := ompHeader(opened)
		_ = opened.Close()
		if err != nil {
			problems = append(problems, problem{path: path, reason: ioReason(err)})
			return
		}
		out = append(out, candidate{sessionID: meta.ID, path: path, cwd: meta.Cwd, mtime: info.ModTime(), omp: meta})
	}
	for _, f := range files {
		if err := env.context().Err(); err != nil {
			return nil, problems, err
		}
		if !f.IsDir() {
			add(root, f)
			continue
		}
		// A root-level custom transcript's sibling directory is child history,
		// not a project store. Nested project transcript directories are never walked.
		if info, err := os.Lstat(filepath.Join(root, f.Name()+".jsonl")); err == nil && !info.IsDir() {
			continue
		}
		dir := filepath.Join(root, f.Name())
		if err := checkDir(dir); err != nil {
			problems = append(problems, problem{path: dir, dir: true, reason: ioReason(err)})
			continue
		}
		children, err := os.ReadDir(dir)
		if err != nil {
			problems = append(problems, problem{path: dir, dir: true, reason: ioReason(err)})
			continue
		}
		for _, child := range children {
			add(dir, child)
		}
	}
	counts := map[string]int{}
	for _, c := range out {
		counts[c.sessionID]++
	}
	kept := out[:0]
	for _, c := range out {
		if counts[c.sessionID] > 1 {
			problems = append(problems, problem{path: c.path, reason: "duplicate omp session id " + c.sessionID})
			continue
		}
		kept = append(kept, c)
	}
	return kept, problems, nil
}

func (ompReader) gate(env Env, c candidate) (string, string) {
	if env.Policy.Disabled {
		return StateDisabled, collect.ReasonDisabled
	}
	if c.omp == nil {
		return StateUnsupportedFormat, "missing omp session header"
	}
	if c.omp.Version < 1 || c.omp.Version > 3 {
		return StateUnsupportedFormat, "unsupported omp session header version"
	}
	if !filepath.IsAbs(c.omp.Cwd) && (len(env.Policy.DenyPaths) > 0 || env.Policy.OptInOnly) {
		return StateDenied, "omp session has no absolute working directory"
	}
	cwds := make([]string, 0, 1+len(c.omp.AdditionalDirectories))
	if filepath.IsAbs(c.omp.Cwd) {
		cwds = append(cwds, c.omp.Cwd)
	}
	for _, cwd := range c.omp.AdditionalDirectories {
		if filepath.IsAbs(cwd) {
			cwds = append(cwds, cwd)
		}
	}
	return cwdPolicy(env, cwds)
}

func (r ompReader) load(env Env, c candidate) (*transcript, error) {
	if state, reason := r.gate(env, c); state != "" {
		return nil, &RefusedError{Ref: Ref(HarnessOMP, c.sessionID), State: state, Reason: reason}
	}
	f, err := r.open(env, c.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	prefix, m, err := ompHeader(f)
	if err != nil {
		return nil, err
	}
	if c.omp == nil || m.ID != c.sessionID || m.ID != c.omp.ID || m.Cwd != c.omp.Cwd || m.Version != c.omp.Version || !slices.Equal(m.AdditionalDirectories, c.omp.AdditionalDirectories) {
		return nil, errOMPChanged
	}
	return r.parse(env.reader(io.MultiReader(bytes.NewReader(prefix), f)), m)
}

type ompRecord struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	ParentID    *string         `json:"parentId"`
	Timestamp   string          `json:"timestamp"`
	Model       string          `json:"model"`
	Role        string          `json:"role"`
	Attribution string          `json:"attribution"`
	Synthetic   bool            `json:"synthetic"`
	Content     json.RawMessage `json:"content"`
	CustomType  string          `json:"customType"`
	Details     json.RawMessage `json:"details"`
	Data        json.RawMessage `json:"data"`
	Message     *ompMessage     `json:"message"`
}

type ompMessage struct {
	Role                  string            `json:"role"`
	Content               json.RawMessage   `json:"content"`
	Attribution           string            `json:"attribution"`
	Synthetic             bool              `json:"synthetic"`
	Model                 string            `json:"model"`
	Timestamp             int64             `json:"timestamp"`
	CompletedAt           int64             `json:"completedAt"`
	StopReason            string            `json:"stopReason"`
	ErrorMessage          string            `json:"errorMessage"`
	ToolCallAbortMessages map[string]string `json:"toolCallAbortMessages"`
	ToolCallID            string            `json:"toolCallId"`
	ToolName              string            `json:"toolName"`
	IsError               bool              `json:"isError"`
	Details               json.RawMessage   `json:"details"`
}

func ompText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return joinTexts(raw, "text")
}

// parse hashes exactly the complete revision, including title bytes and malformed
// records. An incomplete tail contributes neither events nor watermark bytes.
// Any complete change resets the generic watermark offset; entry/call IDs remain
// stable so rescanning a changed journal preserves finding idempotency keys.
func (ompReader) parse(r io.Reader, meta *ompMeta) (*transcript, error) {
	t := &transcript{sessionID: meta.ID}
	t.addCwd(meta.Cwd)
	for _, cwd := range meta.AdditionalDirectories {
		t.addCwd(cwd)
	}
	if at, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
		t.seen(at)
	}
	calls := map[string]*call{}
	seen := map[string]bool{}
	parents := map[string]string{}
	callEntries := map[string]string{}
	callModels := map[string]string{}
	resultEntries := map[string]string{}
	model := ""
	hash := sha256.New()
	br := bufio.NewReaderSize(r, 64<<10)
	var offset int64
	// Reuse the metadata prefix reader, so pretty headers are not classified as
	// malformed individual JSONL lines and cannot leak title text into prompts.
	prefix, actual, err := ompHeader(br)
	if err != nil {
		return nil, err
	}
	if actual.ID != meta.ID {
		return nil, errOMPChanged
	}
	_, _ = hash.Write(prefix)
	offset = int64(len(prefix))
	t.parsed++
	for {
		var line []byte
		for {
			part, readErr := br.ReadSlice('\n')
			if len(line)+len(part) > ompRecordCap {
				return nil, fmt.Errorf("omp record exceeds byte limit: %w", errUnsupported)
			}
			if len(line) == 0 && !errors.Is(readErr, bufio.ErrBufferFull) {
				line = part
			} else {
				// ReadSlice reuses its buffer on the next fragment.
				line = append(line, part...)
			}
			err = readErr
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			_, _ = hash.Write(line)
			var rec ompRecord
			if json.Unmarshal(line, &rec) != nil || rec.Type == "" {
				t.badLines = append(t.badLines, offset)
			} else {
				if rec.ID != "" && seen[rec.ID] {
					return nil, fmt.Errorf("duplicate omp entry id %q: %w", rec.ID, errUnsupported)
				}
				if rec.ID != "" {
					seen[rec.ID] = true
					if rec.ParentID != nil && *rec.ParentID != rec.ID && seen[*rec.ParentID] {
						parents[rec.ID] = *rec.ParentID
					}
				}
				if rec.Type == "session" {
					return nil, fmt.Errorf("duplicate omp session header: %w", errUnsupported)
				}
				t.parsed++
				e := entry{offset: offset, span: rec.ID, cwd: meta.Cwd, kind: kindOther}
				if e.span == "" {
					e.span = "L" + strconv.FormatInt(offset, 10)
				}
				if at, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
					e.at = t.seen(at)
				}
				if rec.Type == "model_change" && (rec.Role == "" || rec.Role == "default") {
					model = rec.Model
				}
				// User-attributed custom notices also carry generated instructions.
				// Only skill-prompt is a source-backed human invocation.
				if rec.Type == "custom_message" && rec.CustomType == "skill-prompt" && rec.Attribution == "user" && !rec.Synthetic {
					var skill struct {
						Name   string `json:"name"`
						Args   string `json:"args"`
						Prompt string `json:"prompt"`
					}
					if json.Unmarshal(rec.Details, &skill) == nil && skill.Name != "" {
						e.kind, e.text = kindPrompt, skill.Prompt
						if e.text == "" {
							e.text = strings.TrimSpace("/skill:" + skill.Name + " " + skill.Args)
						}
						t.addSkillHit(skill.Name, e.span, offset)
					}
				}
				if rec.Type == "message" && rec.Message != nil {
					m := rec.Message
					if m.Timestamp != 0 {
						e.at = t.seen(time.UnixMilli(m.Timestamp))
					}
					if m.CompletedAt != 0 {
						t.seen(time.UnixMilli(m.CompletedAt))
					}
					switch m.Role {
					case "user":
						if m.Attribution == "user" && !m.Synthetic {
							e.kind, e.text = kindPrompt, ompText(m.Content)
						}
					case "assistant":
						e.model = m.Model
						if e.model == "" {
							e.model = model
						} else {
							model = e.model
						}
						e.text = ompText(m.Content)
						if e.text == "" && (m.StopReason == "error" || m.StopReason == "aborted") {
							e.text = m.ErrorMessage
						}
						if e.text != "" {
							e.kind = kindAssistantText
						}
						var blocks []struct {
							Type      string          `json:"type"`
							ID        string          `json:"id"`
							Name      string          `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						}
						_ = json.Unmarshal(m.Content, &blocks)
						for _, b := range blocks {
							if b.Type != "toolCall" {
								continue
							}
							if b.ID == "" || calls[b.ID] != nil {
								return nil, fmt.Errorf("missing or duplicate omp tool call id: %w", errUnsupported)
							}
							c := &call{id: b.ID, tool: b.Name, input: b.Arguments}
							var args struct {
								Command *string `json:"command"`
								Path    string  `json:"path"`
							}
							_ = json.Unmarshal(c.input, &args)
							if args.Command != nil {
								c.command, c.hasCommand = *args.Command, true
							}
							for _, s := range []string{c.command, args.Path} {
								for _, hit := range skillFilePath.FindAllStringSubmatch(s, -1) {
									t.addSkillDir(hit[1], e.span, offset)
								}
							}
							calls[b.ID] = c
							callEntries[b.ID] = rec.ID
							callModels[b.ID] = e.model
							e.calls = append(e.calls, c)
							e.kind = kindToolCall
						}
						if m.StopReason == "aborted" {
							e.kind = kindInterrupt
							// Only explicit call IDs (or calls on this same assistant)
							// establish which retained branch's tools were interrupted.
							for _, c := range calls {
								text, named := m.ToolCallAbortMessages[c.id]
								if !c.result && (named || slices.Contains(e.calls, c)) {
									c.result, c.resultOffset, c.status = true, offset, statusInterrupted
									c.content = text
								}
							}
						}
					case "toolResult":
						if m.ToolCallID == "" {
							return nil, fmt.Errorf("missing omp tool result id: %w", errUnsupported)
						}
						c := calls[m.ToolCallID]
						if rec.ParentID != nil {
							// A replay branches back to the original assistant. A result
							// on a sibling branch is another execution, not a duplicate.
							ancestor := *rec.ParentID
							for ancestor != "" && ancestor != callEntries[m.ToolCallID] {
								if resultEntries[ancestor] == m.ToolCallID {
									return nil, fmt.Errorf("duplicate omp tool result on one branch: %w", errUnsupported)
								}
								ancestor = parents[ancestor]
							}
							if ancestor == "" {
								c = nil
							}
						} else if c != nil && c.result && c.status != statusInterrupted {
							return nil, fmt.Errorf("duplicate unlinked omp tool result: %w", errUnsupported)
						}
						resultEntries[rec.ID] = m.ToolCallID
						if c != nil {
							if c.result && rec.ParentID != nil {
								replay := *c
								c = &replay
								e.calls, e.kind = []*call{c}, kindToolCall
								e.model = callModels[m.ToolCallID]
							}
							var details struct {
								ExitCode    *int   `json:"exitCode"`
								TimedOut    bool   `json:"timedOut"`
								Synthetic   bool   `json:"__synthetic"`
								Interrupted bool   `json:"__interrupted"`
								Source      string `json:"source"`
							}
							// Extension tool details may be any JSON value.
							_ = json.Unmarshal(m.Details, &details)
							c.result, c.resultOffset = true, offset
							c.content, c.exitCode, c.status = ompText(m.Content), details.ExitCode, statusOK
							if m.IsError || details.TimedOut || c.exitCode != nil && *c.exitCode != 0 {
								c.status = statusError
							}
							if details.Interrupted || details.Synthetic && (details.Source == "assistant_stop_aborted" || details.Source == "interrupt_skipped") || m.IsError && (strings.HasPrefix(c.content, "Tool execution was aborted.") || strings.HasPrefix(c.content, "Tool execution was aborted:") || strings.HasPrefix(c.content, "Previous OMP process exited before this tool returned;") || strings.HasPrefix(c.content, "The parent session may still be running this tool;")) {
								c.status = statusInterrupted
							}
						}
					}
				}
				if rec.Type == "custom" && rec.CustomType == "session_exit" {
					var data struct {
						Pending []struct {
							ID string `json:"toolCallId"`
						} `json:"pendingToolCalls"`
					}
					if json.Unmarshal(rec.Data, &data) == nil {
						for _, pending := range data.Pending {
							if c := calls[pending.ID]; c != nil && !c.result {
								c.result, c.resultOffset, c.status = true, offset, statusInterrupted
							}
						}
					}
				}
				if e.kind == kindPrompt && strings.TrimSpace(e.text) == "" {
					e.kind = kindOther
				}
				t.addEntry(e)
			}
			offset += int64(len(line))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	t.size, t.head = offset, hex.EncodeToString(hash.Sum(nil))
	t.summarise(true)
	return t, nil
}
