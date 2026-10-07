// Package localmode is the local-mode target of the CLI: the v1 API handler of
// internal/api invoked in process over a core.Service on the data-directory
// database, served to pkg/client through its transport seam. Every command
// keeps its code and its output; only the transport differs from a server.
//
// No /mcp, metrics, health or readiness route is mounted: nothing listens and
// nothing scrapes. MCPServer builds the stdio MCP server over the same
// service instead. The key the client presents never leaves the process, so
// it is a fixed constant: a local write bypasses the server key by
// construction, for the one OS user who can open the database file.
package localmode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// Key is the API key the in-process handler expects and the client sends.
// It guards nothing: the request never crosses a process boundary.
const Key = "local"

// Features is what the local target wires: everything the HTTP transport
// wires except MCP.
var Features = slices.DeleteFunc(slices.Clone(api.Features), func(f string) bool { return f == "mcp" })

// Target is an open local-mode database with its handler.
type Target struct {
	db      *store.DB
	svc     *core.Service
	version string
	handler http.Handler
	path    string
	wg      sync.WaitGroup
}

// Open opens (creating if needed) the database at path, its directory
// included, and builds the handler. version is the service version /meta
// reports, the client's own. stderr takes the warnings meant for a person;
// nil discards them.
//
// What Open creates is owner-only: the data directory 0700, and the
// database 0600, created empty with that mode before SQLite first opens it,
// so that it never exists looser and the -wal and -shm files SQLite creates
// take the database file's mode; the three are tightened once more after
// the open, for a file whose creation was interrupted. A directory or file
// that already exists keeps its mode; doctor reports a looser one with the
// chmod to run. A failed chmod is a warning: the open proceeds and doctor
// says what is left.
func Open(ctx context.Context, path, version string, stderr io.Writer) (*Target, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	dir := filepath.Dir(path)
	_, dirErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory %s: %w", dir, err)
	}
	if errors.Is(dirErr, os.ErrNotExist) {
		tighten(dir, 0o700, stderr)
	}
	_, fileErr := os.Stat(path)
	created := errors.Is(fileErr, os.ErrNotExist)
	if created {
		// An empty file is a new database to SQLite; the mode is set at
		// creation rather than after it.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create the database %s: %w", path, err)
		}
		if err == nil {
			_ = f.Close()
		}
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if created {
		for _, p := range Sidecars(path) {
			tighten(p, 0o600, stderr)
		}
	}
	svc := core.New(db, core.Config{Version: version, Features: Features})
	srv := api.New(api.Config{Service: svc, DB: db, APIKey: Key, NoMCP: true, NoMetrics: true, NoHealth: true})

	return &Target{db: db, svc: svc, version: version, handler: srv.Handler(), path: path}, nil
}

// MCPServer builds the MCP server over the target's service for the stdio
// transport: the tools of /mcp with no project preset, the generated
// instructions with the placeholder base and no operator text, and the
// version Open was given. Every tool call gets its own request id, which a
// tool error's body carries as over HTTP.
func (t *Target) MCPServer() (*sdk.Server, error) {
	return mcp.NewServer(mcp.Config{Service: t.svc, ErrorBody: api.ToolErrorBody, CallContext: api.WithNewRequestID},
		"", "", t.version)
}

// Path is the database file.
func (t *Target) Path() string { return t.path }

// Sidecars lists the database file and the -wal and -shm files SQLite keeps
// beside it, whether or not they exist now.
func Sidecars(path string) []string { return []string{path, path + "-wal", path + "-shm"} }

// tighten sets mode on path when it exists; a failure is one warning.
func tighten(path string, mode os.FileMode, stderr io.Writer) {
	if err := os.Chmod(path, mode); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "agentfeedback: warning: cannot make %s owner-only (%v); run chmod %o %s\n", path, err, mode, path)
	}
}

// Close waits for every in-flight handler and closes the database.
func (t *Target) Close() error {
	t.wg.Wait()

	return t.db.Close()
}

// Transport serves requests with the handler in process. The handler runs
// in its own goroutine and the response is returned as soon as its status
// is known, with the body streamed through a pipe, so a streamed export is
// read as it is written. Cancelling the request's context, or closing the
// body, ends the stream and unblocks the handler; Close waits for every
// handler to return.
func (t *Target) Transport() http.RoundTripper { return &transport{h: t.handler, wg: &t.wg} }

type transport struct {
	h  http.Handler
	wg *sync.WaitGroup
}

func (tr *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &responseWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	done := make(chan struct{})
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		defer close(done)
		if req.Body != nil {
			defer func() { _ = req.Body.Close() }()
		}
		defer func() {
			if rec := recover(); rec != nil {
				w.abort(fmt.Errorf("handler panic: %v", rec))
			}
		}()
		tr.h.ServeHTTP(w, req)
		w.finish()
	}()
	// The reader side follows the request's context: a cancelled or timed
	// out request fails the body with the context's error, which also fails
	// the handler's next write, so neither side waits on the other.
	body := &contextBody{pr: pr, ctx: req.Context(), closed: make(chan struct{})}
	go func() {
		select {
		case <-req.Context().Done():
			_ = pr.CloseWithError(req.Context().Err())
		case <-body.closed:
		case <-done:
		}
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		_ = body.Close()

		return nil, req.Context().Err()
	}
	if w.err != nil {
		_ = body.Close()

		return nil, w.err
	}

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// contextBody is the response body: the pipe's reader, whose Close also
// releases the context watcher. A read after the context ended reports the
// context's error, as a network body would.
type contextBody struct {
	pr     *io.PipeReader
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *contextBody) Read(p []byte) (int, error) {
	n, err := b.pr.Read(p)
	if errors.Is(err, io.ErrClosedPipe) && b.ctx.Err() != nil {
		return n, b.ctx.Err()
	}

	return n, err
}

func (b *contextBody) Close() error {
	b.once.Do(func() { close(b.closed) })

	return b.pr.Close()
}

// responseWriter is the handler's side of a RoundTrip: headers and status
// are published once on the first WriteHeader or Write (ready closes), the
// body goes down the pipe.
type responseWriter struct {
	header http.Header
	// sent is the snapshot of header taken at WriteHeader; later changes to
	// header are ignored, as net/http ignores them.
	sent   http.Header
	status int
	pw     *io.PipeWriter
	ready  chan struct{}
	once   sync.Once
	err    error
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.status = code
		w.sent = w.header.Clone()
		close(w.ready)
	})
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)

	return w.pw.Write(b)
}

// Flush is a no-op: every write already reaches the reader.
func (w *responseWriter) Flush() {}

// finish ends the body after the handler returned; a handler that wrote
// nothing still answers with its status (200 when it set none).
func (w *responseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.pw.Close()
}

// abort fails the round trip when the handler panicked before answering,
// and cuts the body short when it panicked after.
func (w *responseWriter) abort(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.ready)
	})
	_ = w.pw.CloseWithError(err)
}

// ErrNoDatabase is returned by Stat when the local database does not exist.
var ErrNoDatabase = errors.New("no local database")

// Stat reports whether the database file exists without creating it.
func Stat(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoDatabase
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}

	return nil
}
