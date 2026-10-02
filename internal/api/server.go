// Package api is the HTTP transport of the v1 contract (docs/openapi.yaml)
// over internal/core: routing, middleware, the query grammar, request bodies
// and the mapping of core Problems onto responses. It holds no business
// logic.
package api

import (
	"context"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/mcp"
	"github.com/agentfeedback/agentfeedback/internal/store"
)

// Features is the subset of core.Features this transport wires; the caller
// passes it in core.Config.Features.
var Features = []string{"q", "stats", "export.after_id", "export.limit", "import", "mcp", "redaction"}

// Config wires a Server.
type Config struct {
	Service *core.Service
	// DB is the database behind Service, for /ready and the state metrics.
	DB     *store.DB
	APIKey string
	// ShuttingDown flips to true before the graceful shutdown starts, so
	// /ready reports 503 while in-flight requests drain.
	ShuttingDown *atomic.Bool
	Registry     *prometheus.Registry
	// PublicURL is the normalised base URL the rendered guidance names; empty
	// derives it from each request's scheme and Host.
	PublicURL string
	// MCPInstructions is operator text appended to the MCP server
	// instructions.
	MCPInstructions string
}

// Server serves the v1 HTTP API.
type Server struct {
	svc          *core.Service
	db           *store.DB
	apiKey       string
	shuttingDown *atomic.Bool
	registry     *prometheus.Registry
	metrics      *metrics
	publicURL    string
	mcp          *mcp.Handler
}

// New builds a Server and registers its metrics on the configured registry.
func New(cfg Config) *Server {
	registry := cfg.Registry
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	shuttingDown := cfg.ShuttingDown
	if shuttingDown == nil {
		shuttingDown = &atomic.Bool{}
	}
	s := &Server{
		svc:          cfg.Service,
		db:           cfg.DB,
		apiKey:       cfg.APIKey,
		shuttingDown: shuttingDown,
		registry:     registry,
		metrics:      newMetrics(registry, cfg.Service, cfg.DB),
		publicURL:    cfg.PublicURL,
	}
	s.mcp = mcp.New(mcp.Config{Service: cfg.Service, Instructions: cfg.MCPInstructions, ErrorBody: errorJSON,
		ObserveCreate: s.observeCreate, BuildFailed: writeErr})
	return s
}

// Handler returns the fully wired router.
func (s *Server) Handler() http.Handler {
	public := http.NewServeMux()
	public.HandleFunc("GET /api/v1/openapi.json", s.handleOpenAPI)
	public.HandleFunc("GET /api/v1/schemas", s.handleSchemas)
	public.HandleFunc("GET /api/v1/schemas/{kind}/{version}", s.handleSchema)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/submissions", s.handleCreate)
	api.HandleFunc("GET /api/v1/submissions", s.handleList)
	api.HandleFunc("GET /api/v1/submissions/{id}", s.handleGet)
	api.HandleFunc("PATCH /api/v1/submissions/{id}", s.handleMark)
	api.HandleFunc("DELETE /api/v1/submissions/{id}", s.handleRedact)
	api.HandleFunc("POST /api/v1/submissions/processed", s.handleMarkBatch)
	api.HandleFunc("GET /api/v1/stats", s.handleStats)
	api.HandleFunc("GET /api/v1/export", s.handleExport)
	api.HandleFunc("HEAD /api/v1/export", s.handleExportHead)
	api.HandleFunc("POST /api/v1/import", s.handleImport)
	api.HandleFunc("GET /api/v1/meta", s.handleMeta)

	mux := http.NewServeMux()
	// Operational endpoints: unauthenticated, kept inside the deployment
	// boundary.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	mux.Handle("/api/v1/", s.apiV1(public, onlyPostProcessed(jsonRouteErrors(api, nil))))
	// Discovery: unauthenticated, no data.
	mux.HandleFunc("GET /skill", s.handleSkill)
	mux.HandleFunc("GET /.well-known/agentfeedback.json", s.handleDiscovery)
	// MCP: every method reaches the handler, which checks the key first.
	mux.Handle("/mcp", s.handleMCP(false))
	mux.Handle("/mcp/{project}", s.handleMCP(true))
	mux.Handle("/mcp/", keyedNotFound(s.apiKey))

	// A path ServeMux would redirect (not in cleaned form, or bare /api/v1)
	// is a 404; under /api/v1 and /mcp the key is checked first, as for any
	// other unknown path there.
	notCanonical := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := cleanPath(r.URL.EscapedPath()); underKey(p) {
			keyedNotFound(s.apiKey).ServeHTTP(w, r)
			return
		}
		notFound(w, r)
	})

	// recoverPanic sits innermost so observe still sees the 500 it writes (and
	// records it) and the access log line is emitted for the panicking request.
	return requestID(s.observe(recoverPanic(jsonRouteErrors(mux, notCanonical))))
}

// onlyPostProcessed answers any method but POST on
// /api/v1/submissions/processed with 405, instead of letting the
// /api/v1/submissions/{id} routes take the path as an id.
func onlyPostProcessed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/submissions/processed" && r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiV1 dispatches /api/v1/: the public routes answer without a key (a
// wrong method on one is 405 without a key too); everything else needs the
// key before routing, so an unknown path answers 401 to an unauthenticated
// caller rather than mapping the route space for it. Every response carries
// Cache-Control: no-store except a public route's 2xx, whose handler
// removes it.
func (s *Server) apiV1(public *http.ServeMux, private http.Handler) http.Handler {
	auth := requireAPIKey(s.apiKey)(private)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h, pattern := public.Handler(r)
		switch {
		case pattern != "" && !redirected(r, pattern):
			public.ServeHTTP(w, r)
			return
		case pattern == "":
			probe := &discardWriter{}
			h.ServeHTTP(probe, r)
			if probe.status == http.StatusMethodNotAllowed {
				methodNotAllowed(w, r, probe.Header().Get("Allow"))
				return
			}
		}
		auth.ServeHTTP(w, r)
	})
}

// cleanPath is ServeMux's path canonicalisation: rooted, path.Clean, with a
// trailing slash kept.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// redirected reports whether ServeMux answers r with a redirect instead of the
// handler of pattern: the path is not in cleaned form, or pattern is a subtree
// pattern the path matches only once a trailing slash is appended.
func redirected(r *http.Request, pattern string) bool {
	p := r.URL.EscapedPath()
	if p != cleanPath(p) {
		return true
	}
	if pattern == "" {
		return false
	}
	pat := pattern
	if _, after, found := strings.Cut(pattern, " "); found {
		pat = after
	}
	return strings.HasSuffix(pat, "/") && !strings.HasPrefix(p, pat)
}

// underKey reports a cleaned path in a key-protected route space.
func underKey(p string) bool {
	for _, root := range []string{"/api/v1", "/mcp"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// keyedNotFound answers an unknown path in a key-protected route space: 401
// without the key, 404 with it, both no-store.
func keyedNotFound(apiKey string) http.Handler {
	nf := requireAPIKey(apiKey)(http.HandlerFunc(notFound))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		nf.ServeHTTP(w, r)
	})
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, core.CodeNotFound, "no such route", nil)
}

// discardWriter captures the status and headers a handler writes and drops the
// body. It is used to ask a ServeMux's own no-match handler what it would
// have answered without letting its plain-text body reach the client.
type discardWriter struct {
	header http.Header
	status int
}

func (d *discardWriter) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}

func (d *discardWriter) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	return len(b), nil
}

func (d *discardWriter) WriteHeader(code int) {
	if d.status == 0 {
		d.status = code
	}
}

// jsonRouteErrors makes unmatched routes answer in the API's error shape
// instead of ServeMux's plain text, and keeps ServeMux's redirects from ever
// reaching a client: a path it would redirect goes to onRedirect (nil: 404).
// ServeMux reports a no-match by returning an empty pattern from Handler;
// running the handler it returned against a discarding writer tells 404 from
// 405 (and yields the Allow header it computed) without re-deriving the route
// table here.
func jsonRouteErrors(mux *http.ServeMux, onRedirect http.Handler) http.Handler {
	if onRedirect == nil {
		onRedirect = http.HandlerFunc(notFound)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if redirected(r, pattern) {
			onRedirect.ServeHTTP(w, r)
			return
		}
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &discardWriter{}
		h.ServeHTTP(probe, r)
		switch {
		case probe.status == http.StatusMethodNotAllowed:
			methodNotAllowed(w, r, probe.Header().Get("Allow"))
		case probe.status >= 300 && probe.status < 400:
			onRedirect.ServeHTTP(w, r)
		default:
			notFound(w, r)
		}
	})
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	msg := "method not allowed for this route"
	if allow != "" {
		msg += "; allowed: " + allow
	}
	writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, msg, nil)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleReady reports 503 unavailable (the contract's Error body) while
// shutting down and whenever the database is unreachable or its schema is not
// the one this binary writes: a wedged database must fail the probe, not
// report READY while every write fails.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeError(w, r, http.StatusServiceUnavailable, core.CodeUnavailable, "the service is shutting down", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if !s.dbReady(ctx) {
		writeError(w, r, http.StatusServiceUnavailable, core.CodeUnavailable, "the database is unavailable or its schema is not the expected version", nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("READY"))
}

func (s *Server) dbReady(ctx context.Context) bool {
	if s.db == nil || s.db.Ping(ctx) != nil {
		return false
	}
	current, err := s.db.SchemaVersion(ctx)
	if err != nil {
		return false
	}
	known, err := store.KnownSchemaVersion()
	return err == nil && current == known
}
