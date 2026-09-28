// Package api is the HTTP transport: routing, middleware, request decoding,
// and the mapping from core sentinel errors to status codes. It holds no
// business logic.
package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// Config wires a Server.
type Config struct {
	Service *core.Service
	APIKey  string
	// ShuttingDown flips to true before the graceful shutdown starts, so
	// /ready reports 503 while in-flight requests drain.
	ShuttingDown *atomic.Bool
	Registry     *prometheus.Registry
}

// Server serves the HTTP API.
type Server struct {
	svc          *core.Service
	apiKey       string
	shuttingDown *atomic.Bool
	registry     *prometheus.Registry
	metrics      *metrics
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
		apiKey:       cfg.APIKey,
		shuttingDown: shuttingDown,
		registry:     registry,
		metrics:      newMetrics(registry, cfg.Service),
	}

	return s
}

// Handler returns the fully wired router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints: unauthenticated, kept inside the deployment
	// boundary.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))

	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/frictions", s.handleCreateFriction)
	api.HandleFunc("POST /api/v1/reviews", s.handleCreateReview)
	api.HandleFunc("POST /api/v1/events", s.handleCreateEvent)
	api.HandleFunc("GET /api/v1/submissions", s.handleListSubmissions)
	api.HandleFunc("POST /api/v1/submissions/processed", s.handleSetProcessed)
	api.HandleFunc("GET /api/v1/submissions/{id}", s.handleGetSubmission)
	api.HandleFunc("GET /api/v1/export", s.handleExport)

	// Auth first, so an unknown /api/v1/ path answers 401 to an unauthenticated
	// caller rather than mapping the route space for it.
	mux.Handle("/api/v1/", requireAPIKey(s.apiKey)(jsonRouteErrors(api)))

	// recoverPanic sits innermost so observe still sees the 500 it writes (and
	// records it) and the access log line is emitted for the panicking request.
	return requestID(s.observe(recoverPanic(mux)))
}

// discardWriter captures the status and headers a handler writes and drops the
// body. It is used to ask the ServeMux's own no-match handler what it would
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

// jsonRouteErrors makes unmatched routes under /api/v1/ answer in the API's
// error shape instead of ServeMux's plain text. ServeMux reports a no-match by
// returning an empty pattern from Handler; running the handler it returned
// against a discarding writer tells 404 from 405 (and yields the Allow header
// it computed) without re-deriving the route table here.
func jsonRouteErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)

			return
		}

		probe := &discardWriter{}
		h.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			if allow := probe.Header().Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"method not allowed for this route")

			return
		}

		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleReady reports 503 while shutting down and whenever the database is
// unreachable or its schema is not the one this binary writes: a wedged
// database must fail the probe, not report READY while every write fails.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("SHUTTING_DOWN"))

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.svc.DB().Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("DB_UNAVAILABLE"))

		return
	}
	current, err := s.svc.DB().SchemaVersion(ctx)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("DB_UNAVAILABLE"))

		return
	}
	known, err := store.KnownSchemaVersion()
	if err != nil || current != known {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("DB_UNAVAILABLE"))

		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("READY"))
}
