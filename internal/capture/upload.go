package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const uploadsPath = "/api/v1/sessions/uploads"

func (r *pushRun) request(method, path string, body []byte) (*client.Response, error) {
	if r.stopped() {
		return nil, errBudget
	}
	if !r.authorized() {
		return nil, errConsent
	}
	if r.checkSource != nil {
		if err := r.checkSource(); err != nil {
			return nil, err
		}
	}
	if len(body) > maxRequest {
		return nil, errReceipt
	}
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	return r.client.Do(ctx, method, path, nil, body)
}

// The hosted meta response currently has no prefix. Accept its documented
// extension when present; absent/read-forbidden meta falls back to hostname.
func (r *pushRun) installation() (string, error) {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if host == "" {
		return "", errReceipt
	}
	resp, err := r.request(http.MethodGet, "/api/v1/meta", nil)
	if err != nil {
		var api *client.APIError
		if errors.As(err, &api) && (api.Status == 401 || api.Status == 403 || api.Status == 404 || api.Status == 405) {
			return host, nil
		}
		return "", err
	}
	if resp.Status != 200 {
		return "", errReceipt
	}
	fields, err := decodeObject(resp.Body)
	if err != nil {
		return host, nil
	}
	var prefix string
	// No prefix exists in today's hosted shape; future optional key metadata
	// uses key.prefix. Never derive a prefix from the plaintext API key.
	if key, ok := fields["key"]; ok {
		if keyFields, err := decodeObject(key); err == nil {
			_ = json.Unmarshal(keyFields["prefix"], &prefix)
		}
	}
	if prefix == "" {
		_ = json.Unmarshal(fields["key_prefix"], &prefix)
	}
	if validIdentity(prefix) {
		return prefix, nil
	}
	return host, nil
}

func validIdentity(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= 512 && !strings.ContainsRune(s, '\x00')
}

func validUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && len(s) == 36 && strings.EqualFold(id.String(), s)
}

func decimalID(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

type uploadReceipt struct {
	id        string
	received  map[int]bool
	completed bool
}

func receipt(resp *client.Response, count int, now time.Time) (uploadReceipt, error) {
	r := uploadReceipt{received: make(map[int]bool)}
	if (resp.Status != 200 && resp.Status != 201) || !jsonContentType(resp.ContentType) {
		return r, errReceipt
	}
	fields, err := decodeObject(resp.Body)
	if err != nil {
		return r, errReceipt
	}
	var expires string
	var received []int
	var submission *string
	for _, field := range []struct {
		key    string
		target any
	}{{"id", &r.id}, {"expires_at", &expires}, {"received_parts", &received}, {"submission_id", &submission}} {
		v, ok := fields[field.key]
		if !ok || json.Unmarshal(v, field.target) != nil {
			return r, errReceipt
		}
	}
	if !validUUID(r.id) || bytes.Equal(bytes.TrimSpace(fields["received_parts"]), []byte("null")) {
		return r, errReceipt
	}
	if submission != nil {
		if !decimalID(*submission) || resp.Status != 200 {
			return r, errReceipt
		}
		r.completed = true
	}
	at, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || (!r.completed && !at.After(now)) {
		return r, errReceipt
	}
	for _, p := range received {
		if p < 1 || p > count || r.received[p] {
			return r, errReceipt
		}
		r.received[p] = true
	}
	if resp.Status == 201 && len(received) != 0 {
		return r, errReceipt
	}
	if r.completed && len(received) != count {
		return r, errReceipt
	}
	return r, nil
}

func (r *pushRun) upload(c sessions.CaptureCandidate, m sessions.CaptureMeta, envelope map[string]any, parts []string, installation, hash string, partBytes int, result PushResult) (PushResult, error) {
	contentBytes := 0
	for _, p := range parts {
		contentBytes += len(p)
	}
	if !validIdentity(installation) || !validIdentity(c.Ref) || !validIdentity(c.Harness) {
		return result, errReceipt
	}
	manifest := map[string]any{
		"envelope": envelope, "installation": installation, "session_ref": c.Ref,
		"revision": result.Revision, "capture_start": m.Start.UTC().Format(captureTimeLayout), "capture_end": m.End.UTC().Format(captureTimeLayout),
		"part_count": number(int64(len(parts))), "content_sha256": hash, "content_bytes": number(int64(contentBytes)), "envelope_bytes": number(result.Bytes),
	}
	body := canonjson.Marshal(manifest)
	saved := r.manifestKey(c)
	saved.Manifest, saved.PartBytes = body, partBytes
	if err := r.db.Write(r.ctx, func(q store.Querier) error {
		var err error
		saved, err = store.KeepSessionManifest(r.ctx, q, saved)
		return err
	}); err != nil {
		return result, err
	}
	// A concurrent attempt may have selected different mutable metadata.
	// Do not send parts against a manifest this attempt did not reconstruct.
	if !bytes.Equal(saved.Manifest, body) || saved.PartBytes != partBytes {
		return result, errReceipt
	}
	resp, err := r.request(http.MethodPost, uploadsPath, body)
	if err != nil {
		return result, err
	}
	begin, err := receipt(resp, len(parts), r.clock())
	if err != nil {
		return result, err
	}
	if begin.completed {
		result.Status, result.Reason = "replayed", ""
		return result, nil
	}
	base := uploadsPath + "/" + begin.id
	for i, part := range parts {
		index := i + 1
		if begin.received[index] {
			continue
		}
		body := canonjson.Marshal(map[string]any{"content": part, "sha256": shaText(part)})
		resp, err := r.request(http.MethodPut, base+"/parts/"+strconv.Itoa(index), body)
		if err != nil {
			return result, err
		}
		if resp.Status != 200 || !jsonContentType(resp.ContentType) {
			return result, errReceipt
		}
		fields, err := decodeObject(resp.Body)
		var id string
		var got int
		if err != nil || json.Unmarshal(fields["id"], &id) != nil || json.Unmarshal(fields["part"], &got) != nil || id != begin.id || got != index {
			return result, errReceipt
		}
	}
	// Complete has no request entity: not {}, not null, and no JSON framing.
	resp, err = r.request(http.MethodPost, base+"/complete", nil)
	if err != nil {
		return result, err
	}
	if (resp.Status != 200 && resp.Status != 201) || !jsonContentType(resp.ContentType) {
		return result, errReceipt
	}
	fields, err := decodeObject(resp.Body)
	if err != nil {
		return result, errReceipt
	}
	submission, err := decodeObject(fields["submission"])
	if err != nil {
		return result, errReceipt
	}
	var uid string
	if json.Unmarshal(submission["uid"], &uid) != nil || !validUUID(uid) {
		return result, errReceipt
	}
	var id string
	if json.Unmarshal(submission["id"], &id) != nil || !decimalID(id) {
		return result, errReceipt
	}
	if _, ok := fields["warnings"]; !ok {
		return result, errReceipt
	}
	var warnings []json.RawMessage
	if json.Unmarshal(fields["warnings"], &warnings) != nil || warnings == nil {
		return result, errReceipt
	}
	result.Status, result.Reason, result.SubmissionUID = "pushed", "", uid
	if resp.Status == 200 {
		result.Status = "replayed"
	}
	return result, nil
}
