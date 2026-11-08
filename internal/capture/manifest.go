package capture

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

var errManifestChanged = errors.New("session manifest content changed")

type pendingManifest struct {
	Envelope      map[string]any `json:"envelope"`
	Installation  string         `json:"installation"`
	CaptureStart  string         `json:"capture_start"`
	CaptureEnd    string         `json:"capture_end"`
	ContentSHA256 string         `json:"content_sha256"`
	ContentBytes  int64          `json:"content_bytes"`
	EnvelopeBytes int64          `json:"envelope_bytes"`
	PartCount     int            `json:"part_count"`
}

func (r *pushRun) manifestKey(c sessions.CaptureCandidate) store.SessionManifest {
	return store.SessionManifest{PushScope: policyScope(r.policy), Harness: c.Harness, SessionID: c.SessionID, Revision: formatRevision(c.Size)}
}

func (r *pushRun) pending(c sessions.CaptureCandidate) (pendingManifest, int, bool, error) {
	var saved store.SessionManifest
	var found bool
	err := r.db.Read(r.ctx, func(q store.Querier) error {
		var err error
		saved, found, err = store.GetSessionManifest(r.ctx, q, r.manifestKey(c))
		return err
	})
	var m pendingManifest
	if err == nil && found {
		d := json.NewDecoder(bytes.NewReader(saved.Manifest))
		d.UseNumber()
		err = d.Decode(&m)
		if err == nil && (m.Envelope == nil || saved.PartBytes <= 0 || saved.PartBytes > maxRawPart) {
			err = errReceipt
		}
	}
	return m, saved.PartBytes, found, err
}
