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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// HarnessPi is the registry name of pi.
const HarnessPi = "pi"

type piReader struct{}

func init()                      { register(piReader{}) }
func (piReader) harness() string { return HarnessPi }
func (piReader) name() string    { return "pi-jsonl" }
func (piReader) location(env Env) string {
	if filepath.IsAbs(env.PiSessionsDir) {
		return filepath.Clean(env.PiSessionsDir)
	}
	if !filepath.IsAbs(env.Home) {
		return ""
	}
	return filepath.Join(env.Home, ".pi", "agent", "sessions")
}
func (r piReader) fileEnv(env Env) Env {
	env.OMPSessionsDir = r.location(env)
	return env
}
func (r piReader) open(env Env, path string) (io.ReadCloser, error) {
	return (ompReader{}).open(r.fileEnv(env), path)
}
func piHeader(r io.Reader) ([]byte, *ompMeta, error) {
	var line []byte
	var one [1]byte
	for len(line) < ompHeaderCap {
		n, err := r.Read(one[:])
		if n > 0 {
			line = append(line, one[0])
			if one[0] == '\n' {
				var m struct {
					Type      string `json:"type"`
					Version   int    `json:"version"`
					ID        string `json:"id"`
					Cwd       string `json:"cwd"`
					Timestamp string `json:"timestamp"`
				}
				if json.Unmarshal(line, &m) != nil || m.Type != "session" || !ompID(m.ID) {
					return nil, nil, fmt.Errorf("invalid pi session header: %w", errUnsupported)
				}
				return line, &ompMeta{Type: m.Type, Version: m.Version, ID: m.ID, Cwd: m.Cwd, Timestamp: m.Timestamp}, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil, fmt.Errorf("incomplete pi session header: %w", errUnsupported)
			}
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("pi header exceeds byte limit: %w", errUnsupported)
}
func (r piReader) candidates(env Env) ([]candidate, []problem, error) {
	cs, problems, err := (ompReader{}).candidates(r.fileEnv(env))
	if err != nil {
		return nil, problems, err
	}
	out := cs[:0]
	for _, c := range cs {
		f, err := r.open(env, c.path)
		if err != nil {
			problems = append(problems, problem{path: c.path, reason: ioReason(err)})
			continue
		}
		_, m, err := piHeader(f)
		_ = f.Close()
		if err != nil {
			problems = append(problems, problem{path: c.path, reason: ioReason(err)})
			continue
		}
		c.omp, c.cwd = m, m.Cwd
		out = append(out, c)
	}
	return out, problems, nil
}
func (piReader) gate(env Env, c candidate) (string, string) {
	if env.Policy.Disabled {
		return StateDisabled, collect.ReasonDisabled
	}
	if c.omp == nil || c.omp.Version != 3 {
		return StateUnsupportedFormat, "unsupported pi session header version"
	}
	if !filepath.IsAbs(c.omp.Cwd) && (len(env.Policy.DenyPaths) > 0 || env.Policy.OptInOnly) {
		return StateDenied, "pi session has no absolute working directory"
	}
	if filepath.IsAbs(c.omp.Cwd) {
		return cwdPolicy(env, []string{c.omp.Cwd})
	}
	return "", ""
}
func (r piReader) load(env Env, c candidate) (*transcript, error) {
	if state, reason := r.gate(env, c); state != "" {
		return nil, &RefusedError{Ref: Ref(HarnessPi, c.sessionID), State: state, Reason: reason}
	}
	f, err := r.open(env, c.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	prefix, m, err := piHeader(f)
	if err != nil {
		return nil, err
	}
	if c.omp == nil || m.ID != c.sessionID || m.Version != c.omp.Version || m.Cwd != c.omp.Cwd || m.Timestamp != c.omp.Timestamp {
		return nil, errOMPChanged
	}
	return r.parse(env.reader(io.MultiReader(bytes.NewReader(prefix), f)), m)
}

type piRecord struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	ParentID  *string    `json:"parentId"`
	Timestamp string     `json:"timestamp"`
	ModelID   string     `json:"modelId"`
	Message   *piMessage `json:"message"`
}
type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Model        string          `json:"model"`
	Timestamp    int64           `json:"timestamp"`
	DurationMs   int64           `json:"durationMs"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	ToolCallID   string          `json:"toolCallId"`
	IsError      bool            `json:"isError"`
}

func (piReader) parse(r io.Reader, meta *ompMeta) (*transcript, error) {
	t := &transcript{sessionID: meta.ID}
	t.addCwd(meta.Cwd)
	if at, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
		t.seen(at)
	}
	br := bufio.NewReaderSize(r, 64<<10)
	prefix, actual, err := piHeader(br)
	if err != nil {
		return nil, err
	}
	if actual.ID != meta.ID || actual.Version != 3 {
		return nil, errUnsupported
	}
	h := sha256.New()
	_, _ = h.Write(prefix)
	offset := int64(len(prefix))
	t.parsed++
	seen := map[string]bool{}
	parents := map[string]string{}
	models := map[string]string{}
	calls := map[string]*call{}
	owners := map[string]string{}
	results := map[string]string{}
	for {
		var line []byte
		for {
			part, readErr := br.ReadSlice('\n')
			if len(line)+len(part) > ompRecordCap {
				return nil, fmt.Errorf("pi record exceeds byte limit: %w", errUnsupported)
			}
			if len(line) == 0 && !errors.Is(readErr, bufio.ErrBufferFull) {
				line = part
			} else {
				line = append(line, part...)
			}
			err = readErr
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			_, _ = h.Write(line)
			var rec piRecord
			if json.Unmarshal(line, &rec) != nil || rec.Type == "" || rec.Type == "message" && rec.Message == nil {
				t.badLines = append(t.badLines, offset)
			} else {
				if rec.Type == "session" || rec.ID != "" && seen[rec.ID] {
					return nil, fmt.Errorf("duplicate pi entry or session header: %w", errUnsupported)
				}
				if rec.ID != "" {
					seen[rec.ID] = true
				}
				model := ""
				if rec.ParentID != nil && *rec.ParentID != rec.ID && seen[*rec.ParentID] {
					parents[rec.ID] = *rec.ParentID
					model = models[*rec.ParentID]
				}
				t.parsed++
				e := entry{offset: offset, span: rec.ID, cwd: meta.Cwd, kind: kindOther}
				if e.span == "" {
					e.span = "L" + strconv.FormatInt(offset, 10)
				}
				if at, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
					e.at = t.seen(at)
				}
				if rec.Type == "model_change" {
					model = rec.ModelID
				}
				if rec.Type == "message" && rec.Message != nil {
					m := rec.Message
					if m.Timestamp != 0 {
						e.at = t.seen(time.UnixMilli(m.Timestamp))
						if m.DurationMs > 0 {
							t.seen(time.UnixMilli(m.Timestamp + m.DurationMs))
						}
					}
					switch m.Role {
					case "user":
						e.kind, e.text = kindPrompt, ompText(m.Content)
					case "assistant":
						if m.Model != "" {
							model = m.Model
						}
						e.model = model
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
								return nil, fmt.Errorf("missing or duplicate pi tool call id: %w", errUnsupported)
							}
							c := &call{id: b.ID, tool: b.Name, input: b.Arguments}
							var args struct {
								Command *string `json:"command"`
							}
							_ = json.Unmarshal(b.Arguments, &args)
							if args.Command != nil {
								c.command, c.hasCommand = *args.Command, true
							}
							calls[b.ID], owners[b.ID] = c, rec.ID
							e.calls = append(e.calls, c)
							e.kind = kindToolCall
						}
						if m.StopReason == "aborted" {
							e.kind = kindInterrupt
							for _, c := range e.calls {
								c.result, c.resultOffset, c.status = true, offset, statusInterrupted
							}
						}
					case "toolResult":
						if m.ToolCallID == "" {
							return nil, fmt.Errorf("missing pi tool result id: %w", errUnsupported)
						}
						c := calls[m.ToolCallID]
						ancestor := ""
						if rec.ParentID != nil {
							ancestor = *rec.ParentID
						}
						for ancestor != "" && ancestor != owners[m.ToolCallID] {
							if results[ancestor] == m.ToolCallID {
								return nil, fmt.Errorf("duplicate pi tool result on branch: %w", errUnsupported)
							}
							ancestor = parents[ancestor]
						}
						if ancestor == "" {
							c = nil
						}
						results[rec.ID] = m.ToolCallID
						if c != nil {
							if c.result {
								replay := *c
								c = &replay
								e.calls, e.kind = []*call{c}, kindToolCall
								e.model = models[owners[m.ToolCallID]]
							}
							c.result, c.resultOffset, c.content, c.status = true, offset, ompText(m.Content), statusOK
							if m.IsError {
								c.status = statusError
							}
						}
					}
				}
				models[rec.ID] = model
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
	t.size, t.head = offset, hex.EncodeToString(h.Sum(nil))
	t.summarise(true)
	return t, nil
}
