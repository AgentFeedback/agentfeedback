package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// member is one member of a strictly read JSON object, its value as the
// exact source bytes.
type member struct {
	name string
	raw  json.RawMessage
}

// errNotObject and errTrailing classify readObject's failures besides a
// *duplicateError; any other error means the bytes are not one JSON value.
var (
	errNotObject = errors.New("not a JSON object")
	errTrailing  = errors.New("data after the JSON object")
)

type duplicateError struct{ name string }

func (e *duplicateError) Error() string {
	return fmt.Sprintf("member %q appears more than once", e.name)
}

// readObject reads b as exactly one JSON object and returns its members in
// source order with their raw values. Duplicate member names, trailing data
// and anything but an object are errors. Numbers are never converted.
func readObject(b []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var members []member
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("member name is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, &duplicateError{name}
		}
		seen[name] = true
		members = append(members, member{name, raw})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errTrailing
	}
	return members, nil
}

// bodyProblem turns a readObject error on a request body into a Problem:
// malformed JSON, a non-object or trailing data is 400 bad_request, a
// duplicate member 400 validation_error.
func bodyProblem(err error) *Problem {
	var dup *duplicateError
	if errors.As(err, &dup) {
		return invalid("duplicate_key", "/"+schema.EscapeToken(dup.name), "body member %s appears more than once; send it once", short(dup.name))
	}
	switch {
	case errors.Is(err, errNotObject):
		return &Problem{Status: 400, Code: CodeBadRequest, Message: "body must be a JSON object"}
	case errors.Is(err, errTrailing):
		return &Problem{Status: 400, Code: CodeBadRequest, Message: "body must be exactly one JSON object; data follows it"}
	default:
		return &Problem{Status: 400, Code: CodeBadRequest, Message: "body is not valid JSON: " + err.Error()}
	}
}

// jsonType names the JSON type of a raw value for messages.
func jsonType(raw json.RawMessage) string {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	if len(raw) == 0 {
		return "nothing"
	}
	switch raw[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	default:
		return "a number"
	}
}
