package sessions

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// HarnessCursor is the registry name shared by Cursor IDE and CLI.
const HarnessCursor = "cursor"

// cursorReader reads only transcripts explicitly indexed by lifecycle hooks.
// Their bytes are opaque: neither IDE text nor CLI JSONL establishes semantic
// digest compatibility. No Cursor database or unindexed transcript is searched.
type cursorReader struct{}

type cursorIndex struct {
	path string
	meta detect.CursorSession
}

func init() { register(cursorReader{}) }

func (cursorReader) harness() string { return HarnessCursor }
func (cursorReader) name() string    { return "cursor-agent-transcript" }

func (cursorReader) location(env Env) string {
	if !filepath.IsAbs(env.CacheDir) {
		return ""
	}
	return filepath.Join(detect.SessionsDir(env.CacheDir), HarnessCursor)
}

// readCursorIndex reads counter metadata, never the transcript it names. The
// filename must bind the original conversation id, not its sanitised spelling.
func readCursorIndex(env Env, path string) (*cursorIndex, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("cursor index is not a regular counter file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var state detect.State
	dec := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	if err := dec.Decode(&state); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("cursor index has trailing data")
	}
	if state.Cursor == nil {
		return nil, nil
	}
	meta := *state.Cursor
	if meta.ConversationID == "" || strings.ContainsAny(meta.ConversationID, `/\#`) || meta.ConversationID == "." || meta.ConversationID == ".." || detect.StatePath(env.CacheDir, HarnessCursor, meta.ConversationID) != path {
		return nil, errors.New("cursor index has an invalid conversation id")
	}
	return &cursorIndex{path: path, meta: meta}, nil
}

func (r cursorReader) candidates(env Env) ([]candidate, []problem, error) {
	if err := env.context().Err(); err != nil {
		return nil, nil, err
	}
	root := r.location(env)
	if root == "" {
		return nil, nil, fs.ErrNotExist
	}
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	var problems []problem
	for _, file := range files {
		if err := env.context().Err(); err != nil {
			return nil, problems, err
		}
		if !strings.HasSuffix(file.Name(), ".json") || file.IsDir() {
			continue
		}
		path := filepath.Join(root, file.Name())
		index, err := readCursorIndex(env, path)
		if err != nil {
			problems = append(problems, problem{path: path, reason: ioReason(err)})
			continue
		}
		if index == nil {
			continue
		}
		info, err := file.Info()
		if err != nil {
			problems = append(problems, problem{path: path, reason: ioReason(err)})
			continue
		}
		c := candidate{sessionID: index.meta.ConversationID, path: index.meta.TranscriptPath, mtime: info.ModTime(), cursor: index}
		if len(index.meta.WorkspaceRoots) > 0 {
			c.cwd = index.meta.WorkspaceRoots[0]
		}
		if transcriptInfo, err := os.Lstat(c.path); err == nil {
			c.mtime = transcriptInfo.ModTime()
		}
		out = append(out, c)
	}
	return out, problems, nil
}

func cursorIndexUnchanged(env Env, c candidate) error {
	if c.cursor == nil {
		return ErrCaptureChanged
	}
	fresh, err := readCursorIndex(env, c.cursor.path)
	if err != nil || fresh == nil {
		return ErrCaptureChanged
	}
	project := ""
	if len(fresh.meta.WorkspaceRoots) > 0 {
		project = fresh.meta.WorkspaceRoots[0]
	}
	if fresh.meta.ConversationID != c.cursor.meta.ConversationID ||
		fresh.meta.TranscriptPath != c.cursor.meta.TranscriptPath ||
		fresh.meta.Cwd != c.cursor.meta.Cwd ||
		!slices.Equal(fresh.meta.WorkspaceRoots, c.cursor.meta.WorkspaceRoots) ||
		c.path != fresh.meta.TranscriptPath || c.cwd != project || c.sessionID != fresh.meta.ConversationID {
		return ErrCaptureChanged
	}
	return nil
}

func (cursorReader) gate(env Env, c candidate) (state, reason string) {
	if env.Policy.Disabled {
		return StateDisabled, collect.ReasonDisabled
	}
	if err := cursorIndexUnchanged(env, c); err != nil {
		return StateChanged, "capture_changed"
	}
	meta := c.cursor.meta
	if len(meta.WorkspaceRoots) == 0 {
		return StateUnknownProject, "Cursor index has no workspace roots"
	}
	for _, root := range meta.WorkspaceRoots {
		if !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
			return StateUnknownProject, "Cursor index has an invalid workspace root"
		}
	}
	if meta.Cwd != "" && (!filepath.IsAbs(meta.Cwd) || strings.ContainsRune(meta.Cwd, '\x00')) {
		return StateUnknownProject, "Cursor index has an invalid cwd"
	}
	if state, reason := capturePolicy(env, meta.WorkspaceRoots); state != "" {
		return state, reason
	}
	if meta.Cwd != "" {
		return capturePolicy(env, []string{meta.Cwd})
	}
	return "", ""
}

// cursorPathInfo refuses symlinks in every component, including the home and
// its ancestors. Lexical containment alone must never authorise a transcript.
func cursorPathInfo(home, path string) (os.FileInfo, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(path) {
		return nil, errors.New("cursor transcript requires absolute home and path")
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return nil, errors.New("cursor transcript is outside home")
	}
	var source os.FileInfo
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("cursor transcript path contains a symlink")
		}
		if source == nil {
			if !info.Mode().IsRegular() {
				return nil, errors.New("cursor transcript is not a regular file")
			}
			source = info
		} else if !info.IsDir() {
			return nil, errors.New("cursor transcript ancestor is not a directory")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return source, nil
}

func cursorOpenedInfo(f io.Closer) (os.FileInfo, error) {
	if tee, ok := f.(*captureTee); ok {
		return cursorOpenedInfo(tee.Closer)
	}
	if f, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
		return f.Stat()
	}
	return nil, errors.New("cursor transcript descriptor cannot be validated")
}

// open also serves raw capture reopening. The root confines resolution even
// during an ancestor replacement; descriptor identity and repeated no-symlink
// checks reject replacements before any transcript bytes are read.
func (r cursorReader) open(env Env, c candidate) (io.ReadCloser, error) {
	if state, reason := r.gate(env, c); state != "" {
		return nil, &RefusedError{Ref: Ref(HarnessCursor, c.sessionID), State: state, Reason: reason}
	}
	before, err := cursorPathInfo(env.Home, c.path)
	if err != nil {
		return nil, err
	}
	if env.Open == nil {
		env.Open = func(path string) (io.ReadCloser, error) {
			root, err := os.OpenRoot(env.Home)
			if err != nil {
				return nil, err
			}
			defer func() { _ = root.Close() }()
			rel, err := filepath.Rel(env.Home, path)
			if err != nil {
				return nil, err
			}
			return openCursorFile(root, rel)
		}
	}
	f, err := env.open(c.path)
	if err != nil {
		return nil, err
	}
	after, pathErr := cursorPathInfo(env.Home, c.path)
	opened, statErr := cursorOpenedInfo(f)
	if pathErr != nil || statErr != nil || !os.SameFile(before, after) || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrCaptureChanged
	}
	if err := cursorIndexUnchanged(env, c); err != nil {
		_ = f.Close()
		return nil, err
	}
	if state, reason := r.gate(env, c); state != "" {
		_ = f.Close()
		return nil, &RefusedError{Ref: Ref(HarnessCursor, c.sessionID), State: state, Reason: reason}
	}
	return f, nil
}

func (r cursorReader) load(env Env, c candidate) (*transcript, error) {
	f, err := r.open(env, c)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	t := &transcript{sessionID: c.sessionID, storageOnly: true}
	for _, root := range c.cursor.meta.WorkspaceRoots {
		t.addCwd(root)
	}
	t.addCwd(c.cursor.meta.Cwd)
	// The first workspace root remains the display project.
	t.lastCwd = c.cwd
	if err := scanJSONL(f, t, func(_ int64, _ []byte) bool {
		// Count complete opaque lines, not a recognised prompt/tool schema.
		// No transcript text or per-line semantic entries are retained.
		return true
	}); err != nil {
		return nil, err
	}
	if err := cursorIndexUnchanged(env, c); err != nil {
		return nil, err
	}
	// Neither documented transcript surface guarantees event timestamps.
	t.start, t.end = c.mtime, c.mtime
	return t, nil
}
