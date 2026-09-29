package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// responseRecorder captures the status code and byte count for logging and
// metrics while staying transparent to handlers that need flushing (the
// export streams).
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.status == 0 {
		rr.status = code
		rr.ResponseWriter.WriteHeader(code)
	}
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.written += int64(n)

	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (Flush, deadlines).
func (rr *responseRecorder) Unwrap() http.ResponseWriter { return rr.ResponseWriter }

// recoverPanic turns a panicking handler into a 500 JSON response instead of a
// dropped connection.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "panic serving request",
					"error", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// requestID echoes a caller-supplied X-Request-Id or generates one, so a client
// retry can be correlated with the server's log lines.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			var buf [16]byte
			if _, err := rand.Read(buf[:]); err == nil {
				id = hex.EncodeToString(buf[:])
			}
		}
		w.Header().Set("X-Request-Id", id)

		ctx := r.Context()
		next.ServeHTTP(w, r.WithContext(ctxWithRequestID(ctx, id)))
	})
}

// observe records metrics and writes the access log. Operational endpoints are
// left out of the log: they are polled every few seconds and would bury
// everything else.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &responseRecorder{ResponseWriter: w}

		next.ServeHTTP(rr, r)

		status := rr.status
		if status == 0 {
			status = http.StatusOK
		}
		// ServeMux sets Pattern on the request it routes; unmatched paths keep
		// it empty and are collapsed into one label so a scanner cannot create
		// unbounded metric series.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.metrics.observeRequest(route, r.Method, status, time.Since(start))

		switch r.URL.Path {
		case "/health", "/ready", "/metrics":
			return
		}

		level := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		case status >= http.StatusBadRequest:
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "http request",
			"method", r.Method, "path", r.URL.Path, "route", route, "status", status,
			"bytes", rr.written, "duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()))
	})
}
