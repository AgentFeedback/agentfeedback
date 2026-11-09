package sessions

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	harnessreg "github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// ErrCaptureChanged means the source no longer matches the selected snapshot.
var ErrCaptureChanged = errors.New("session changed during capture")

// CaptureCandidate is a complete session revision, independent of digest
// watermarks. Cwd and Cwds are filesystem paths, not scrubbed transport metadata.
// Refused candidates carry no transcript metadata.
type CaptureCandidate struct {
	Ref, Harness, SessionID, Reader string
	State, Reason, Cwd, Head        string
	Cwds                            []string
	Size                            int64
	Mtime, Start, End               time.Time
	Model                           string
	Counts                          Counts
	source                          *captureSource
}

// CaptureMeta describes the complete raw transcript returned by OpenCapture.
type CaptureMeta struct {
	Format     string
	Start, End time.Time
	Model      string
	Counts     Counts
	LineCount  int64
}

type captureSource struct {
	r     reader
	c     candidate
	stamp captureStamp
	meta  CaptureMeta
	cwds  []string
}

type captureStamp struct {
	file os.FileInfo
	oc   ocStamp
}

// ocCapturer is an OpenCode reader: its capture is the virtual log it
// renders from the database, not the bytes of a file.
type ocCapturer interface {
	loadCapture(env Env, c candidate, raw io.Writer) (*transcript, error)
}

func captureSupported(r reader) bool {
	switch r.(type) {
	case claudeReader, codexReader, opencodeReader, opencodeV2Reader:
		return true
	default:
		return false
	}
}

func captureStampOf(r reader, c candidate) (captureStamp, error) {
	info, err := os.Lstat(c.path)
	if err != nil {
		return captureStamp{}, err
	}
	if !info.Mode().IsRegular() {
		return captureStamp{}, errors.New("session source is not a regular file")
	}
	s := captureStamp{file: info}
	if _, ok := r.(ocCapturer); ok {
		s.oc, err = ocStampOf(c.path)
	}
	return s, err
}

func (s captureStamp) equal(other captureStamp) bool {
	return os.SameFile(s.file, other.file) && s.file.Size() == other.file.Size() &&
		s.file.ModTime().Equal(other.file.ModTime()) && s.oc.equal(other.oc)
}

func capturePolicy(env Env, cwds []string) (string, string) {
	if env.Policy.Disabled {
		return StateDisabled, collect.ReasonDisabled
	}
	if state, reason := cwdPolicy(env, cwds); state != "" {
		return state, reason
	}
	for _, cwd := range cwds {
		if allowed, _ := collect.SessionPush(cwd); !allowed {
			return StateDisabled, "repo_sessions_push"
		}
	}
	return "", ""
}

func captureBase(r reader, c candidate) CaptureCandidate {
	return CaptureCandidate{
		Ref: Ref(r.harness(), c.sessionID), Harness: r.harness(),
		SessionID: c.sessionID, Reader: r.name(), Mtime: c.mtime,
	}
}

// CaptureCandidates enumerates only supported capture sources when refs is
// empty. Named unsupported or missing sessions remain explicit outcomes. It
// never writes sessions_seen or retains raw transcript bytes.
func CaptureCandidates(ctx context.Context, db *store.DB, env Env, refs []string) ([]CaptureCandidate, error) {
	out := []CaptureCandidate{}
	env.ctx = ctx
	listed := map[string]listing{}
	list := func(r reader) listing {
		if l, ok := listed[r.harness()]; ok {
			return l
		}
		var l listing
		l.cands, l.problems, l.err = r.candidates(env)
		listed[r.harness()] = l
		return l
	}
	if len(refs) == 0 {
		for _, r := range readers {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !captureSupported(r) {
				continue
			}
			l := list(r)
			for _, c := range l.cands {
				if err := ctx.Err(); err != nil {
					return out, err
				}
				fresh := captureEvaluate(env, r, c, nil)
				if err := ctx.Err(); err != nil {
					return out, err
				}
				out = append(out, fresh)
			}
			if err := ctx.Err(); err != nil {
				return out, err
			}
		}
		return out, ctx.Err()
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		h, id, valid := strings.Cut(ref, ":")
		h = harnessreg.CanonicalName(h)
		c := CaptureCandidate{Ref: Ref(h, id), Harness: h, SessionID: id, State: StateUnsupportedFormat, Reason: "no capture source"}
		r, ok := readerFor(h)
		if !valid || id == "" || strings.ContainsAny(id, `/\#`) || id == "." || id == ".." || !ok || !captureSupported(r) {
			out = append(out, c)
			continue
		}
		c.Reader = r.name()
		l := list(r)
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if state := refusedStore(l.err); state != "" {
			c.State, c.Reason = state, "session store could not be read"
		} else if src, found := find(l.cands, id); found {
			c = captureEvaluate(env, r, src, nil)
			if err := ctx.Err(); err != nil {
				return out, err
			}
		} else {
			c.State, c.Reason = StateAbsent, "session not found"
			if db != nil {
				err := db.Read(ctx, func(q store.Querier) error {
					row, found, err := store.GetSession(ctx, q, h, id)
					if err == nil && found {
						c.State, _ = unlisted(l.problems, row.Path)
					}
					return err
				})
				if err != nil {
					return out, err
				}
			}
		}
		out = append(out, c)
	}
	return out, ctx.Err()
}

// captureEvaluate uses the digest's gate and metadata validation, then checks
// every recorded cwd before exposing any bytes. known paths are rechecked before
// reopening a previously selected source.
func captureEvaluate(env Env, r reader, c candidate, known []string) CaptureCandidate {
	out := captureBase(r, c)
	refuse := func(state, reason string) CaptureCandidate {
		out.State, out.Reason = state, reason
		return out
	}
	if c.gated != nil {
		c.gated = &gatedMeta{}
	}
	if state, reason := r.gate(env, c); state != "" {
		return refuse(state, reason)
	}
	pre := append([]string(nil), known...)
	if c.cwd != "" {
		pre = append(pre, c.cwd)
	}
	if c.gated != nil && c.gated.cwd != "" {
		pre = append(pre, c.gated.cwd)
	}
	if state, reason := capturePolicy(env, pre); state != "" {
		return refuse(state, reason)
	}
	before, err := captureStampOf(r, c)
	if err != nil {
		return refuse(StateUnreadable, "session source could not be read")
	}
	if _, ok := r.(ocCapturer); !ok && !before.file.ModTime().Equal(c.mtime) {
		return refuse(StateChanged, "capture_changed")
	}
	env.captureOnly = true
	var tr *transcript
	if oc, ok := r.(ocCapturer); ok {
		tr, err = oc.loadCapture(env, c, nil)
	} else {
		tr, err = r.load(env, c)
	}
	after, stampErr := captureStampOf(r, c)
	if stampErr != nil || !before.equal(after) || errors.Is(err, errCodexChanged) {
		return refuse(StateChanged, "capture_changed")
	}
	if errors.Is(err, errUnsupported) {
		return refuse(StateUnsupportedFormat, "unsupported session format")
	}
	if err != nil {
		return refuse(StateUnreadable, "session could not be read")
	}
	if tr.parsed == 0 {
		return refuse(StateUnsupportedFormat, "unsupported session format")
	}
	if state, reason := capturePolicy(env, tr.cwds); state != "" {
		return refuse(state, reason)
	}
	if len(tr.cwds) == 0 {
		return refuse(StateUnknownProject, "session has no working directory")
	}
	tr.summarise(false)
	out.State, out.Cwd, out.Cwds = StateNew, tr.lastCwd, append([]string(nil), tr.cwds...)
	out.Size, out.Head = tr.size, tr.head
	out.Start, out.End, out.Model, out.Counts = tr.start, tr.end, tr.lastModel, tr.total
	if _, ok := r.(codexReader); ok {
		out.Model = tr.captureModel
	}
	if out.Start.IsZero() {
		out.Start = c.mtime
	}
	if out.End.IsZero() {
		out.End = c.mtime
	}
	format := r.name()
	if _, ok := r.(ocCapturer); ok {
		format = "opencode-virtual-log"
	}
	meta := CaptureMeta{
		Format: format, Start: out.Start, End: out.End, Model: out.Model,
		Counts: out.Counts, LineCount: int64(tr.parsed + tr.total.Unparsed),
	}
	out.source = &captureSource{r: r, c: c, stamp: after, meta: meta, cwds: tr.cwds}
	return out
}

type captureTee struct {
	io.Reader
	io.Closer
}

func (r *captureTee) Context() context.Context {
	if r, ok := r.Reader.(interface{ Context() context.Context }); ok {
		return r.Context()
	}
	return context.Background()
}

func (r *captureTee) CaptureOnly() bool { return readerCaptureOnly(r.Reader) }

// captureContextReader checks cancellation even when a parser buffers ahead.
type captureContextReader struct {
	io.Reader
	ctx         context.Context
	captureOnly bool
}

func (r *captureContextReader) Context() context.Context { return r.ctx }
func (r *captureContextReader) CaptureOnly() bool        { return r.captureOnly }

func (r *captureContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func captureRefusal(c CaptureCandidate) error {
	if c.Reason == "capture_changed" {
		return ErrCaptureChanged
	}
	return &RefusedError{Ref: scrubString(c.Ref), State: c.State, Reason: scrubString(c.Reason)}
}

// OpenCapture rechecks eligibility and streams the selected complete prefix.
// Callers must read to EOF and close successfully before sending: both
// boundaries reject a source changed while the caller consumed its bytes.
func OpenCapture(ctx context.Context, env Env, c CaptureCandidate) (io.ReadCloser, CaptureMeta, error) {
	env.ctx = ctx
	if err := CheckCapture(ctx, env, c); err != nil {
		return nil, CaptureMeta{}, err
	}
	var raw io.ReadCloser
	var cancel context.CancelFunc
	if oc, ok := c.source.r.(ocCapturer); ok {
		pr, pw := io.Pipe()
		raw = pr
		producerCtx, stopProducer := context.WithCancel(ctx)
		cancel = stopProducer
		producerEnv := env
		producerEnv.ctx = producerCtx
		go func() {
			stop := context.AfterFunc(producerCtx, func() { _ = pw.CloseWithError(producerCtx.Err()) })
			defer stop()
			_, err := oc.loadCapture(producerEnv, c.source.c, pw)
			_ = pw.CloseWithError(err)
		}()
	} else {
		f, err := env.open(c.source.c.path)
		if err != nil {
			return nil, CaptureMeta{}, err
		}
		raw = f
	}
	prefix := &io.LimitedReader{R: raw, N: c.Size}
	return &captureReadCloser{
		reader: prefix, closer: raw, ctx: ctx, cancel: cancel,
		check: func() error { return CheckCapture(ctx, env, c) },
	}, c.source.meta, nil
}

// CheckCapture rechecks the selected source snapshot and every recorded
// repository's current permission without retaining raw transcript bytes.
func CheckCapture(ctx context.Context, env Env, c CaptureCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	env.ctx = ctx
	if c.source == nil {
		return captureRefusal(c)
	}
	s := c.source
	if state, reason := capturePolicy(env, s.cwds); state != "" {
		return &RefusedError{Ref: scrubString(c.Ref), State: state, Reason: reason}
	}
	stamp, err := captureStampOf(s.r, s.c)
	if err != nil || !stamp.equal(s.stamp) {
		return ErrCaptureChanged
	}
	fresh := captureEvaluate(env, s.r, s.c, s.cwds)
	if err := ctx.Err(); err != nil {
		return err
	}
	if fresh.source == nil {
		return captureRefusal(fresh)
	}
	if fresh.Size != c.Size || fresh.Head != c.Head || !fresh.Mtime.Equal(c.Mtime) || !fresh.source.stamp.equal(s.stamp) {
		return ErrCaptureChanged
	}
	return nil
}

type captureReadCloser struct {
	reader *io.LimitedReader
	closer io.Closer
	ctx    context.Context
	cancel context.CancelFunc
	check  func() error
	closed bool
	err    error
}

func (r *captureReadCloser) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.err = r.ctx.Err(); r.err != nil {
		return 0, r.err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		r.err = ctxErr
		return n, r.err
	}
	if errors.Is(err, io.EOF) {
		if r.reader.N != 0 {
			r.err = ErrCaptureChanged
			return n, r.err
		}
		if r.err = r.check(); r.err != nil {
			return n, r.err
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

func (r *captureReadCloser) Close() error {
	if !r.closed {
		r.closed = true
		if r.cancel != nil {
			r.cancel()
		}
		if err := r.closer.Close(); r.err == nil {
			r.err = err
		}
		if err := r.check(); err != nil {
			r.err = err
		}
	}
	return r.err
}
