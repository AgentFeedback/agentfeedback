package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// ExportHeader is the first line of an export. It states what the stream
// contains, so an importer can refuse a partial export instead of silently
// restoring a subset.
type ExportHeader struct {
	ExportFormat int     `json:"export_format"`
	Family       *string `json:"family"`
	Since        *string `json:"since"`
	ExportedAt   string  `json:"exported_at"`
}

// ExportTerminator is the last line of an export. Its absence means the stream
// was truncated. sha256 covers the record lines only (each including its
// trailing newline), not the header.
type ExportTerminator struct {
	ExportComplete bool   `json:"export_complete"`
	Count          int64  `json:"count"`
	SHA256         string `json:"sha256"`
}

type exportEncoder struct {
	w      io.Writer
	digest hash.Hash
	count  int64
}

func newExportEncoder(w io.Writer) *exportEncoder {
	return &exportEncoder{w: w, digest: sha256.New()}
}

func (e *exportEncoder) writeHeader(family string, since *time.Time, exportedAtMicros int64) error {
	h := ExportHeader{ExportFormat: exportFormatVersion, ExportedAt: FormatMicros(exportedAtMicros)}
	if family != "" {
		f := family
		h.Family = &f
	}
	if since != nil {
		s := since.UTC().Format(TimeFormat)
		h.Since = &s
	}
	line, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode export header: %w", err)
	}
	if _, err := e.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write export header: %w", err)
	}

	return nil
}

func (e *exportEncoder) writeRecord(sub store.Submission) error {
	line, err := json.Marshal(ToRecord(sub))
	if err != nil {
		return fmt.Errorf("encode submission %d: %w", sub.ID, err)
	}
	line = append(line, '\n')
	if _, err := e.w.Write(line); err != nil {
		return fmt.Errorf("write submission %d: %w", sub.ID, err)
	}
	e.digest.Write(line)
	e.count++

	return nil
}

func (e *exportEncoder) writeTerminator() error {
	line, err := json.Marshal(ExportTerminator{
		ExportComplete: true,
		Count:          e.count,
		SHA256:         hex.EncodeToString(e.digest.Sum(nil)),
	})
	if err != nil {
		return fmt.Errorf("encode export terminator: %w", err)
	}
	if _, err := e.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write export terminator: %w", err)
	}

	return nil
}
