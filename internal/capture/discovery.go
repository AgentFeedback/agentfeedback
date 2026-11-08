package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

func discover(ctx context.Context, c *client.Client, p Policy, clock func() time.Time) (Policy, error) {
	// Authenticate the policy request first. Public discovery lacking a route
	// must never hide a key that cannot read the policy (401/403).
	resp, err := c.Do(ctx, http.MethodGet, policyPath, nil, nil)
	if err != nil {
		return classifyFailure(p, err, "policy", clock())
	}
	if !jsonContentType(resp.ContentType) {
		p.State = Unsupported

		return p, nil
	}
	decoded, err := decodePolicy(resp.Body, p)
	if err != nil {
		p.State = Error

		return p, err
	}
	resp, err = c.Do(ctx, http.MethodGet, discoveryPath, nil, nil)
	if err != nil {
		return classifyFailure(p, err, "discovery", clock())
	}
	if !jsonContentType(resp.ContentType) {
		p.State = Unsupported

		return p, nil
	}
	doc, err := decodeObject(resp.Body)
	if err != nil {
		return p, errors.New("sessions: malformed discovery JSON")
	}
	paths, err := decodeObject(doc["paths"])
	if err != nil {
		return p, errors.New("sessions: discovery document has invalid paths")
	}
	path, exists := paths[policyPath]
	if !exists {
		p.State = Unsupported

		return p, nil
	}
	operations, err := decodeObject(path)
	if err != nil {
		return p, errors.New("sessions: discovery policy route is malformed")
	}
	get, exists := operations["get"]
	if !exists {
		p.State = Unsupported

		return p, nil
	}
	if _, err := decodeObject(get); err != nil {
		return p, errors.New("sessions: discovery policy GET operation is malformed")
	}

	return decoded, nil
}

func classifyFailure(p Policy, err error, operation string, receivedAt time.Time) (Policy, error) {
	p.State, p.Enabled = Error, false
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			p.State = Unauthorized
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			p.State = Unsupported
		case http.StatusTooManyRequests:
			p.State, p.RetryAt = Suspended, retryTime(apiErr.RetryAfter, receivedAt)
		default:
			// Do not echo the response body, Location, request id or error
			// message: servers and proxies can include credentials there.
			return p, fmt.Errorf("sessions: %s request failed (HTTP %d)", operation, apiErr.Status)
		}

		return p, nil
	}

	return p, fmt.Errorf("sessions: %s request failed (network or response read error)", operation)
}

func retryTime(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if retry, err := http.ParseTime(value); err == nil {
		return retry.UTC()
	}

	return now.Add(time.Minute)
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (mediaType == "application/json" ||
		(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")))
}

// decodeObject rejects non-objects, trailing values and duplicate member names.
// Decoder errors are deliberately not surfaced: JSON syntax errors may quote
// bytes from an untrusted response that contain credentials.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid JSON member")
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid JSON member name")
		}
		if _, exists := out[key]; exists {
			return nil, errors.New("duplicate JSON member")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, errors.New("invalid JSON member value")
		}
		out[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid JSON object")
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON value")
	}

	return out, nil
}

func decodePolicy(raw []byte, p Policy) (Policy, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return p, errors.New("sessions: malformed policy JSON")
	}
	for _, field := range [...]struct {
		name string
		dest any
	}{
		{"org_id", &p.OrgID},
		{"enabled", &p.Enabled},
		{"notice_version", &p.NoticeVersion},
		{"max_part_bytes", &p.MaxPartBytes},
		{"max_session_bytes", &p.MaxSessionBytes},
		{"incomplete_upload_ttl_seconds", &p.IncompleteUploadTTLSeconds},
	} {
		value, exists := fields[field.name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, field.dest) != nil {
			p.Enabled = false

			return p, errors.New("sessions: policy is missing a required field or has an invalid field type")
		}
	}
	if value, exists := fields["org_name"]; exists {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &p.OrgName) != nil {
			p.Enabled = false

			return p, errors.New("sessions: policy org_name must be a string")
		}
	}
	if value, exists := fields["retention"]; exists {
		retention, err := decodeObject(value)
		if err != nil {
			p.Enabled = false

			return p, errors.New("sessions: policy retention must be an object")
		}
		var r Retention
		if json.Unmarshal(retention["mode"], &r.Mode) != nil ||
			(r.Mode != "delete_after_processing" && r.Mode != "retain") {
			p.Enabled = false

			return p, errors.New("sessions: policy retention mode is invalid")
		}
		if days, exists := retention["days"]; exists {
			if json.Unmarshal(days, &r.Days) != nil || r.Days <= 0 || r.Mode != "retain" {
				p.Enabled = false

				return p, errors.New("sessions: policy retention days are invalid")
			}
		} else if r.Mode == "retain" {
			p.Enabled = false

			return p, errors.New("sessions: retained policy requires retention days")
		}
		p.Retention = &r
	}
	if !validPolicy(p) {
		p.Enabled = false

		return p, errors.New("sessions: policy identity, notice version or upload bounds are invalid or unsupported")
	}
	id, _ := uuid.Parse(p.OrgID) // validPolicy checked the canonical UUID form.
	p.OrgID = id.String()
	p.State = Disabled
	if p.Enabled {
		p.State = Enabled
	}

	return p, nil
}
