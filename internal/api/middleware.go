package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

func ctxWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request id attached by the middleware.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

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
// dropped connection. http.ErrAbortHandler is re-panicked: it is the
// deliberate abort the server handles itself.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic serving request",
					"error", rec, "path", r.URL.Path, "request_id", RequestIDFromContext(r.Context()),
					"stack", string(debug.Stack()))
				writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// validRequestID reports whether a caller's X-Request-Id is echoed: 1 to 128
// bytes of [A-Za-z0-9._-].
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// requestID echoes a well-formed caller X-Request-Id or generates one, so a
// client retry can be correlated with the server's log lines.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			var buf [16]byte
			_, _ = rand.Read(buf[:])
			id = hex.EncodeToString(buf[:])
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctxWithRequestID(r.Context(), id)))
	})
}

// observe records metrics (when they are enabled) and writes the access log.
// Operational endpoints are left out of the log: they are polled every few
// seconds and would bury everything else.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &responseRecorder{ResponseWriter: w}

		next.ServeHTTP(rr, r)

		status := rr.status
		if status == 0 {
			status = http.StatusOK
		}
		// ServeMux sets Pattern on the request it routes, so the innermost
		// match wins. A miss under /api/v1/ keeps the outer pattern /api/v1/;
		// any other miss has none and is labelled unmatched. Either way a
		// scanner cannot create unbounded metric series.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		if s.metrics != nil {
			s.metrics.observeRequest(route, r.Method, status, time.Since(start))
		}

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
