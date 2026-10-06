package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

// readBody reads at most limit+1 bytes of the body, so core can tell an
// over-limit body (and answer 413) without reading it all. ok is false when a
// response was already written.
func readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)+1))
	var tooBig *http.MaxBytesError
	if err != nil && !errors.As(err, &tooBig) {
		writeError(w, r, http.StatusBadRequest, core.CodeBadRequest, "the request body could not be read", nil)
		return nil, false
	}
	return body, true
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, envelope.BodyLimit)
	if !ok {
		return
	}
	res, err := s.svc.Create(r.Context(), body)
	s.observeCreate(res.Created, err)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/api/v1/submissions/"+strconv.FormatInt(res.Record.ID, 10))
	writeJSON(w, r, status, res)
}

// observeCreate records a create attempt on submissions_created_total, for
// REST and MCP alike.
func (s *Server) observeCreate(created bool, err error) {
	switch {
	case err != nil:
		s.metrics.observeCreateError(err)
	case created:
		s.metrics.observeSubmission(outcomeCreated)
	default:
		s.metrics.observeSubmission(outcomeExisting)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p, err := parseListParams(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := s.svc.List(r.Context(), p)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := s.idAndNoQuery(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	rec, err := s.svc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rec)
}

func (s *Server) handleMark(w http.ResponseWriter, r *http.Request) {
	id, err := s.idAndNoQuery(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	body, ok := readBody(w, r, envelope.BodyLimit)
	if !ok {
		return
	}
	rec, err := s.svc.Mark(r.Context(), id, body)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rec)
}

func (s *Server) handleRedact(w http.ResponseWriter, r *http.Request) {
	id, err := s.idAndNoQuery(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	rec, err := s.svc.Redact(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rec)
}

func (s *Server) handleMarkBatch(w http.ResponseWriter, r *http.Request) {
	if err := noQuery(r); err != nil {
		writeErr(w, r, err)
		return
	}
	body, ok := readBody(w, r, envelope.BodyLimit)
	if !ok {
		return
	}
	res, err := s.svc.MarkBatch(r.Context(), body)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	p, err := parseStatsParams(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := s.svc.Stats(r.Context(), p)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

// exportWriter sends the export headers and 200 on the first write, so a
// Problem core returns before writing still becomes a JSON error.
type exportWriter struct {
	w       http.ResponseWriter
	started bool
}

func setExportHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson")
}

func (e *exportWriter) Write(b []byte) (int, error) {
	if !e.started {
		e.started = true
		setExportHeaders(e.w)
		e.w.WriteHeader(http.StatusOK)
	}
	return e.w.Write(b)
}

// errHeadProbe stops an export at its first write: HEAD only needs to know
// the parameters pass core's validation.
var errHeadProbe = errors.New("head probe: parameters valid")

type probeWriter struct{}

func (probeWriter) Write([]byte) (int, error) { return 0, errHeadProbe }

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	p, err := parseExportParams(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ew := &exportWriter{w: w}
	err = s.svc.Export(r.Context(), p, ew)
	if err == nil {
		return
	}
	if !ew.started {
		writeErr(w, r, err)
		return
	}
	// The status is sent; the stream ends without its trailer, which is how
	// a reader tells a truncated export.
	if r.Context().Err() != nil {
		slog.WarnContext(r.Context(), "export client disconnected", "error", err,
			"request_id", RequestIDFromContext(r.Context()))
		return
	}
	slog.ErrorContext(r.Context(), "export aborted mid-stream", "error", err,
		"request_id", RequestIDFromContext(r.Context()))
}

func (s *Server) handleExportHead(w http.ResponseWriter, r *http.Request) {
	p, err := parseExportParams(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := s.svc.Export(r.Context(), p, probeWriter{}); err != nil && !errors.Is(err, errHeadProbe) {
		writeErr(w, r, err)
		return
	}
	setExportHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if err := noQuery(r); err != nil {
		writeErr(w, r, err)
		return
	}
	body, ok := readBody(w, r, core.ImportLimit)
	if !ok {
		return
	}
	res, err := s.svc.Import(r.Context(), body)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	// The contract lists no 400 here, so the query string is ignored.
	writeJSON(w, r, http.StatusOK, s.svc.Meta())
}

func (s *Server) handleSchemas(w http.ResponseWriter, r *http.Request) {
	// The contract lists no 400 here, so the query string is ignored.
	w.Header().Del("Cache-Control")
	writeJSON(w, r, http.StatusOK, struct {
		Schemas any `json:"schemas"`
	}{s.svc.Schemas()})
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	version, err := pathID(r, "version")
	if err == nil {
		err = noQuery(r)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	doc, err := s.svc.Schema(r.PathValue("kind"), version)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.Header().Del("Cache-Control")
	writeRaw(w, http.StatusOK, "application/schema+json", doc)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	// The contract lists no 400 here, so the query string is ignored.
	w.Header().Del("Cache-Control")
	writeRaw(w, http.StatusOK, "application/json", openAPIDoc)
}

// idAndNoQuery parses {id} and rejects any query parameter.
func (s *Server) idAndNoQuery(r *http.Request) (int64, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return 0, err
	}
	return id, noQuery(r)
}
