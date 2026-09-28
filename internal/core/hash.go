package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/agentfeedback/agentfeedback/internal/canonjson"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// reviewCanonical / frictionCanonical are the exact shapes hashed into
// payload_hash. Struct field order fixes the JSON encoding, so identical
// content always produces the same digest. created_at is deliberately
// excluded — the hash identifies content, not the moment of receipt.
//
// These two shapes are byte-compatible with API 1.0: rows imported from the
// PostgreSQL service keep their original hash and still dedupe against new
// submissions. Do not reorder the fields.
type reviewCanonical struct {
	MachineName      string        `json:"machine_name"`
	CoordinatorModel string        `json:"coordinator_model"`
	Payload          reviewPayload `json:"payload"`
}

type frictionCanonical struct {
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	Payload          frictionPayload `json:"payload"`
}

// canonicalHash returns the sha256 hex digest of v's JSON encoding.
func canonicalHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}

// eventHash digests {coordinator_model, machine_name, payload} canonically:
// keys sorted, numbers verbatim. Event payloads are free-form JSON, so the
// digest cannot come from a fixed Go struct the way reviews' and frictions' do.
// The three top-level keys are written in sorted order by construction.
func eventHash(machineName, coordinatorModel string, payload []byte) (string, error) {
	canonicalPayload, err := canonjson.Marshal(payload)
	if err != nil {
		return "", err
	}
	machine, err := json.Marshal(machineName)
	if err != nil {
		return "", fmt.Errorf("encode machine_name: %w", err)
	}
	model, err := json.Marshal(coordinatorModel)
	if err != nil {
		return "", fmt.Errorf("encode coordinator_model: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(`{"coordinator_model":`)
	buf.Write(model)
	buf.WriteString(`,"machine_name":`)
	buf.Write(machine)
	buf.WriteString(`,"payload":`)
	buf.Write(canonicalPayload)
	buf.WriteByte('}')

	sum := sha256.Sum256(buf.Bytes())

	return hex.EncodeToString(sum[:]), nil
}

// HashForFamily recomputes a record's content hash from its stored payload,
// using the rule of its family. The importer needs it for rows exported
// without a hash; the result is byte-identical to the hash the create path
// would have produced for the same content.
func HashForFamily(family, machineName, coordinatorModel string, payload []byte) (string, error) {
	switch family {
	case store.FamilyFriction:
		var p frictionPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return "", fmt.Errorf("decode friction payload: %w", err)
		}
		// Context is auto-collected metadata and never part of the identity.
		p.Context = nil

		return canonicalHash(frictionCanonical{
			MachineName:      machineName,
			CoordinatorModel: coordinatorModel,
			Payload:          p,
		})
	case store.FamilyReview:
		var p reviewPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return "", fmt.Errorf("decode review payload: %w", err)
		}

		return canonicalHash(reviewCanonical{
			MachineName:      machineName,
			CoordinatorModel: coordinatorModel,
			Payload:          p,
		})
	case store.FamilyEvent:
		return eventHash(machineName, coordinatorModel, payload)
	default:
		return "", fmt.Errorf("unknown family %q", family)
	}
}
