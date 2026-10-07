package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

// ToolErrorBody is the Error body of err as a tool result carries it, over
// either MCP transport: a *core.Problem 1:1, anything else a logged internal
// error. The request id is the one attached to ctx.
func ToolErrorBody(ctx context.Context, err error) []byte {
	body := errorBody{Error: codeInternal, Message: "internal error", RequestID: RequestIDFromContext(ctx)}
	var p *core.Problem
	if errors.As(err, &p) {
		body = errorBody{Error: p.Code, Message: p.Message, RequestID: RequestIDFromContext(ctx), Details: p.Details}
	} else {
		slog.ErrorContext(ctx, "internal error", "error", err, "request_id", RequestIDFromContext(ctx))
	}
	b, mErr := core.Marshal(body)
	if mErr != nil {
		b, _ = core.Marshal(errorBody{Error: codeInternal, Message: "internal error", RequestID: RequestIDFromContext(ctx)})
	}
	return b
}

// handleMCP serves /mcp (preset false) and /mcp/{project} (preset true).
// The key is checked before anything else, so an unauthenticated caller
// learns nothing about methods or the transport. Then the preset is
// normalised, the body is read under the create body limit, and the MCP
// transport answers; its plain-text errors are rewritten into the Error
// shape, while its JSON-RPC errors pass through for MCP clients to parse.
func (s *Server) handleMCP(preset bool) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project := ""
		if preset {
			if project = mcp.Preset(r.PathValue("project")); project == "" {
				writeErr(w, r, invalid(detailEmpty, "/project",
					"project path parameter is empty once normalised like the project member; give a project name"))
				return
			}
		}
		body, ok := readBody(w, r, mcp.RequestLimit)
		if !ok {
			return
		}
		if len(body) > mcp.RequestLimit {
			mcpTooLarge(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		base, _ := s.baseURL(r)
		tw := &mcpErrorWriter{rw: w}
		s.mcp.For(base, project).ServeHTTP(tw, r)
		tw.finish(r)
	})
	auth := requireAPIKey(s.apiKey)(inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth.ServeHTTP(w, r)
	})
}

func mcpTooLarge(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusRequestEntityTooLarge, core.CodeRequestTooLarge,
		"MCP request body over "+strconv.Itoa(mcp.RequestLimit)+" bytes (the create body limit plus 65536 bytes for the JSON-RPC message)", nil)
}

// maxDerivedBase is the longest request-derived base URL used; a longer
// Host renders the placeholder, so client input cannot grow the server
// cache or the instructions without bound.
const maxDerivedBase = 256

// mcpErrorWriter holds back a response of status 400 or more whose
// Content-Type is not application/json (the transport's plain-text errors),
// so finish can answer it in the Error shape instead.
type mcpErrorWriter struct {
	rw          http.ResponseWriter
	wroteHeader bool
	held        int
	msg         bytes.Buffer
}

func (m *mcpErrorWriter) Header() http.Header { return m.rw.Header() }

func (m *mcpErrorWriter) WriteHeader(code int) {
	if m.wroteHeader {
		return
	}
	m.wroteHeader = true
	ct, _, _ := mime.ParseMediaType(m.rw.Header().Get("Content-Type"))
	if code >= http.StatusBadRequest && ct != "application/json" {
		m.held = code
		return
	}
	m.rw.Header().Set("Cache-Control", "no-store")
	m.rw.WriteHeader(code)
}

func (m *mcpErrorWriter) Write(b []byte) (int, error) {
	if !m.wroteHeader {
		m.WriteHeader(http.StatusOK)
	}
	if m.held != 0 {
		if room := 512 - m.msg.Len(); room > 0 {
			m.msg.Write(b[:min(len(b), room)])
		}
		return len(b), nil
	}
	return m.rw.Write(b)
}

// Flush lets the transport stream server-sent events; a held error is
// never flushed.
func (m *mcpErrorWriter) Flush() {
	if m.held == 0 {
		_ = http.NewResponseController(m.rw).Flush()
	}
}

// finish answers a held transport error in the Error shape.
func (m *mcpErrorWriter) finish(r *http.Request) {
	if m.held == 0 {
		return
	}
	h := m.rw.Header()
	h.Del("Content-Type")
	h.Del("Content-Length")
	msg := strings.TrimSpace(m.msg.String())
	switch status := m.held; {
	case status == http.StatusMethodNotAllowed:
		methodNotAllowed(m.rw, r, http.MethodPost)
	case status == http.StatusNotFound:
		notFound(m.rw, r)
	case status == http.StatusRequestEntityTooLarge:
		mcpTooLarge(m.rw, r)
	case status >= http.StatusInternalServerError:
		slog.ErrorContext(r.Context(), "mcp transport error", "status", status, "message", msg,
			"request_id", RequestIDFromContext(r.Context()))
		writeError(m.rw, r, http.StatusInternalServerError, codeInternal, "internal error", nil)
	default:
		if msg == "" {
			msg = http.StatusText(status)
		}
		writeError(m.rw, r, status, core.CodeBadRequest, msg, nil)
	}
}

// baseURL is the server's base URL for rendered guidance: PUBLIC_URL when
// configured, else the request's scheme and Host; derived reports the
// latter. A Host that does not normalise yields "", which renders the
// placeholder.
func (s *Server) baseURL(r *http.Request) (base string, derived bool) {
	if s.publicURL != "" {
		return s.publicURL, false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	base, err := skillgen.NormalizeServer(scheme + "://" + r.Host)
	if err != nil || len(base) > maxDerivedBase {
		return "", true
	}
	return base, true
}

// skillFormats are the forms GET /skill serves.
var skillFormats = []string{skillgen.FormSkillMD, skillgen.FormAgentsMD, skillgen.FormPrompt}

func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, []string{"format"})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	format := skillgen.FormSkillMD
	if v, ok := q["format"]; ok {
		format = v[0]
		if !slices.Contains(skillFormats, format) {
			writeErr(w, r, invalid(detailRange, "?format",
				"format must be one of "+strings.Join(skillFormats, ", ")+", got "+short(format)))
			return
		}
	}
	base, derived := s.baseURL(r)
	body, err := skillgen.Render(format, base)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if derived {
		w.Header().Set("Vary", "Host, X-Forwarded-Proto")
	}
	writeRaw(w, http.StatusOK, "text/markdown; charset=utf-8", body)
}

// discovery is the document GET /.well-known/agentfeedback.json serves.
type discovery struct {
	Service    string        `json:"service"`
	Version    string        `json:"version"`
	APIVersion string        `json:"api_version"`
	OpenAPI    string        `json:"openapi"`
	Schemas    string        `json:"schemas"`
	MCP        string        `json:"mcp"`
	Skill      string        `json:"skill"`
	Auth       discoveryAuth `json:"auth"`
	Docs       string        `json:"docs"`
}

type discoveryAuth struct {
	Modes []string `json:"modes"`
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	// The contract lists no 400 here, so the query string is ignored.
	writeJSON(w, r, http.StatusOK, discovery{
		Service:    "agentfeedback",
		Version:    s.svc.Meta().ServiceVersion,
		APIVersion: core.APIVersion,
		OpenAPI:    "/api/v1/openapi.json",
		Schemas:    "/api/v1/schemas",
		MCP:        "/mcp",
		Skill:      "/skill",
		Auth:       discoveryAuth{Modes: []string{"bearer", "x-api-key"}},
		Docs:       "https://agentfeedback.dev/docs",
	})
}
