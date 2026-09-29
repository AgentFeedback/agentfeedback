package envelope

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"

	"github.com/agentfeedback/agentfeedback/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

var versionSpelling = regexp.MustCompile(`^[1-9][0-9]*$`)

// Decode runs the write path on body: parse, the inference table in its
// order, the normalisation order, the kind schema as a guide for an explicit
// kind, and the recommended-member check. It returns the stored envelope and
// its warnings, or a *Rejection for one of the four refused bodies (then the
// envelope and the warnings are nil).
func Decode(body []byte) (*Envelope, []schema.Detail, error) {
	return DecodeWithLimits(body, BodyLimit, MaxDepth)
}

// DecodeWithLimits is Decode with maxBytes in place of BodyLimit and
// maxDepth in place of MaxDepth: a body over maxBytes is 413
// request_too_large and one nested deeper than maxDepth is 400 bad_request,
// each message stating the limit. The import path re-decodes stored
// envelopes with it, which can exceed both request limits: invalid UTF-8
// expands to U+FFFD, and inference nests values up to StoredDepthHeadroom
// levels deeper.
func DecodeWithLimits(body []byte, maxBytes, maxDepth int) (*Envelope, []schema.Detail, error) {
	if len(body) > maxBytes {
		return nil, nil, &Rejection{Status: 413, Code: "request_too_large", Message: fmt.Sprintf("body over %d bytes", maxBytes)}
	}
	det := newDetails()
	value, err := parse(body, det, maxDepth)
	if err != nil {
		return nil, nil, &Rejection{Status: 400, Code: "bad_request", Message: err.Error()}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, &Rejection{Status: 400, Code: "bad_request", Message: "body is a JSON value but not an object"}
	}
	d := &decoder{det: det, placed: map[string]bool{}}
	env, kindInferred := d.infer(object)
	warnings := det.flatten()
	if !kindInferred {
		warnings = append(warnings, schema.Validate(env.Kind, env.SchemaVersion, env.Payload, d.placed)...)
	}
	present := map[string]bool{"kind": true}
	for name, value := range map[string]string{
		"key": env.Key, "summary": env.Summary, "machine": env.Machine, "model": env.Model,
		"harness": env.Harness, "project": env.Project, "occurred_at": env.OccurredAt,
	} {
		present[name] = value != ""
	}
	for _, name := range recommended {
		if !present[name] {
			warnings = append(warnings, schema.Detail{Code: "missing_recommended", Pointer: "/" + name, Message: name + " is recommended"})
		}
	}
	return env, warnings, nil
}

type decoder struct {
	det    *details
	placed map[string]bool // top-level payload members inference put there
}

func (d *decoder) warn(code, message string, path ...string) {
	d.det.add(path, code, message)
}

// infer applies the inference table and the normalisation order to the
// parsed body object and reports whether the kind was inferred.
func (d *decoder) infer(value map[string]any) (*Envelope, bool) {
	env := &Envelope{}

	// A null envelope member is absent, with one coerced warning.
	for _, name := range envelopeMembers {
		if v, ok := value[name]; ok && v == nil {
			d.warn("coerced", name+" is null; treated as absent", name)
			delete(value, name)
		}
	}

	// payload present but not an object.
	var payload map[string]any
	raw, payloadPresent := value["payload"]
	if payloadPresent {
		if object, ok := raw.(map[string]any); ok {
			payload = object
		} else {
			d.det.remap(path("payload"), path("payload", "value"))
			payload = map[string]any{"value": raw}
			d.placed["value"] = true
			d.warn("payload_wrapped", `payload was not an object; wrapped as {"value": ...}`, "payload")
		}
	} else {
		payload = map[string]any{}
	}

	// Unknown top-level members move into payload, in code point order of
	// their names; a flat body becomes the payload.
	var unknown []string
	for name := range value {
		if !isEnvelopeMember[name] {
			unknown = append(unknown, name)
		}
	}
	slices.Sort(unknown)
	for _, name := range unknown {
		hint := ""
		if lower := schema.LowerSimple(name); isEnvelopeMember[lower] {
			hint = "; did you mean " + lower
		}
		d.place(payload, name, value[name], "moved_to_payload",
			name+" is not an envelope member; moved into payload"+hint, path(name))
	}
	if !payloadPresent {
		d.warn("payload_inferred", "payload was absent; built from the members the envelope does not know", "payload")
	}

	// kind: an explicit string, token-normalised, or unknown.
	kindInferred := true
	if s, ok := value["kind"].(string); ok {
		if env.Kind = schema.Token(s); env.Kind != "" {
			kindInferred = false
		}
	}
	if kindInferred {
		d.warn("missing_kind", "kind is missing, empty or not a string; stored as unknown", "kind")
		env.Kind = "unknown"
	}

	// schema_version: a plain positive integer at most 2^53-1, or 1.
	env.SchemaVersion = 1
	if raw, ok := value["schema_version"]; ok {
		if v, ok := parseVersion(raw); ok {
			env.SchemaVersion = v
		} else {
			d.warn("schema_version_defaulted", "schema_version is not a positive integer; defaulted to 1", "schema_version")
		}
	}

	// String members: coerce, then normalise in the fixed order.
	for _, name := range stringMembers {
		raw, ok := value[name]
		if !ok {
			continue
		}
		s, isString := raw.(string)
		if !isString {
			s = string(canonjson.Marshal(raw))
			d.det.collapse(path(name))
			d.warn("coerced", name+" was not a string; encoded as canonical JSON", name)
		}
		rule := rules[name]
		s = schema.Trim(s)
		if name == "summary" {
			s = schema.NewlinesToSpaces(s)
		}
		if rule.token {
			s = schema.Token(s)
		}
		if rule.limit > 0 {
			var cut bool
			if s, cut = schema.TruncateBytes(s, rule.limit); cut {
				d.warn("truncated", fmt.Sprintf("%s cut to %d bytes", name, rule.limit), name)
				if !rule.token {
					// Step 5: a trimmed member is trimmed again after a cut,
					// so its stored form is a fixed point of the write path.
					s = schema.Trim(s)
				}
			}
		}
		if s == "" {
			continue // empty after normalisation: omitted
		}
		switch name {
		case "key":
			env.Key = s
		case "summary":
			env.Summary = s
		case "machine":
			env.Machine = s
		case "model":
			env.Model = s
		case "harness":
			env.Harness = s
		case "project":
			env.Project = s
		}
	}

	// context: a non-object goes to payload.context_raw before occurred_at_raw
	// can be added.
	context := map[string]any{}
	if raw, ok := value["context"]; ok {
		if object, ok := raw.(map[string]any); ok {
			context = maps.Clone(object)
		} else {
			d.place(payload, "context_raw", raw, "moved_to_payload",
				"context was not an object; moved to payload.context_raw", path("context"))
		}
	}

	// occurred_at: parsed and stored, or kept as context.occurred_at_raw.
	if raw, ok := value["occurred_at"]; ok {
		text, isString := raw.(string)
		if !isString {
			text = string(canonjson.Marshal(raw))
			d.det.collapse(path("occurred_at"))
		}
		if stored, ok := schema.NormalizeDateTime(text); ok {
			env.OccurredAt = stored
			if !isString {
				d.warn("coerced", "occurred_at was not a string", "occurred_at")
			}
		} else {
			if _, taken := context["occurred_at_raw"]; taken {
				// The producer's value is discarded, and so are its warnings.
				d.det.detach(path("context", "occurred_at_raw"))
				d.warn("duplicate_key", "context.occurred_at_raw replaced", "context", "occurred_at_raw")
			}
			d.det.remap(path("occurred_at"), path("context", "occurred_at_raw"))
			context["occurred_at_raw"] = text
			if !isString {
				d.warn("coerced", "occurred_at was not a string", "context", "occurred_at_raw")
			}
			d.warn("invalid_format", "occurred_at is not an RFC 3339 date-time; kept as context.occurred_at_raw", "context", "occurred_at_raw")
		}
	}

	// context: cap, then coerce and truncate the kept values.
	if len(context) > contextEntries {
		keys := slices.Sorted(maps.Keys(context))
		overflow := make(map[string]any, len(keys)-contextEntries)
		for _, k := range keys[contextEntries:] {
			overflow[k] = context[k]
			delete(context, k)
		}
		at := d.place(payload, "context_overflow", overflow, "truncated",
			fmt.Sprintf("context has more than %d entries; the rest moved to payload.context_overflow", contextEntries), nil)
		for k := range overflow {
			d.det.remap(path("context", k), append(slices.Clone(at), k))
		}
	}
	if len(context) > 0 {
		env.Context = make(map[string]string, len(context))
		for k, raw := range context {
			s, isString := raw.(string)
			if !isString {
				s = string(canonjson.Marshal(raw))
				d.det.collapse(path("context", k))
				d.warn("coerced", "context values are strings; encoded as canonical JSON", "context", k)
			}
			var cut bool
			if s, cut = schema.TruncateBytes(s, contextValueBytes); cut {
				d.warn("truncated", fmt.Sprintf("context value cut to %d bytes", contextValueBytes), "context", k)
			}
			env.Context[k] = s
		}
	}

	env.Payload = payload
	return env, kindInferred
}

// place puts an inferred member into payload under name, or under
// payload.moved.<name> when the name is taken; a non-object payload.moved is
// wrapped first, and a second value under the same reserved name replaces
// the earlier one with duplicate_key, dropping the earlier value's warnings
// as the parser does for a duplicate member. The warnings under from, when
// given, follow the content. It records the top-level payload member that received
// it in placed and returns the path the content ended up at.
func (d *decoder) place(payload map[string]any, name string, value any, code, message string, from []string) []string {
	var at []string
	if _, taken := payload[name]; !taken {
		at = path("payload", name)
		payload[name] = value
		d.placed[name] = true
	} else {
		moved, exists := payload["moved"]
		object, isObject := moved.(map[string]any)
		if exists && !isObject {
			d.det.remap(path("payload", "moved"), path("payload", "moved", "value"))
			object = map[string]any{"value": moved}
			payload["moved"] = object
			d.warn("payload_wrapped", "payload.moved is reserved and was not an object; wrapped", "payload", "moved")
		}
		if !exists {
			object = map[string]any{}
			payload["moved"] = object
		}
		at = path("payload", "moved", name)
		if _, dup := object[name]; dup {
			// The earlier value is discarded, and so are its warnings.
			d.det.detach(at)
			d.warn("duplicate_key", "payload.moved."+name+" replaced", at...)
		}
		object[name] = value
		d.placed["moved"] = true
	}
	if from != nil {
		d.det.remap(from, at)
	}
	d.warn(code, message, at...)
	return at
}

// parseVersion accepts a number spelled ^[1-9][0-9]*$ that is at most
// schemaVersionMax.
func parseVersion(raw any) (uint64, bool) {
	n, ok := raw.(json.Number)
	if !ok || len(n) > 16 || !versionSpelling.MatchString(string(n)) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(n), 10, 64)
	if err != nil || v > schemaVersionMax {
		return 0, false
	}
	return v, true
}

func path(tokens ...string) []string { return tokens }
