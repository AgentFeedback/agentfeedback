package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// maxRequestBytes caps the entire request body, including trailing whitespace.
const maxRequestBytes = 10 << 20

// decodeRequestBody accepts exactly one JSON value, rejects unknown fields, and
// reads through EOF so the byte limit applies to the complete body. A typo in a
// field name is a bug, not something to drop: a dropped field plus an
// idempotent replay would lose data for good.
func decodeRequestBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		writeRequestBodyError(w, err)

		return false
	}

	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 10 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: must contain exactly one JSON value")
		}

		return false
	}

	return true
}

func writeRequestBodyError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 10 MiB")

		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to encode response", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error","message":"internal error"}`))

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, ErrorResponse{Error: errType, Message: message})
}
