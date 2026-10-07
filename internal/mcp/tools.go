package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

// Tool names, one per core operation.
const (
	ToolSubmit    = "submit_feedback"
	ToolList      = "list_submissions"
	ToolGet       = "get_submission"
	ToolStats     = "stats"
	ToolMark      = "mark_processed"
	ToolGetSchema = "get_schema"
)

// tools are the tools of one connection; preset is its project ("" none).
type tools struct {
	svc           *core.Service
	preset        string
	errorBody     func(context.Context, error) []byte
	observeCreate func(created bool, err error)
}

func ptr(b bool) *bool { return &b }

// annotations sets every hint explicitly; no tool reaches outside the
// service.
func annotations(readOnly, destructive bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: ptr(destructive), OpenWorldHint: ptr(false)}
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func object(props map[string]any, additional bool) map[string]any {
	return map[string]any{"type": "object", "properties": props, "additionalProperties": additional}
}

// envelopeTypes are the JSON types the input schema states for each
// envelope member; the write path accepts and coerces anything.
var envelopeTypes = map[string]map[string]any{
	"kind":           prop("string", "friction, review, or another kind; stored as unknown with a missing_kind warning when omitted"),
	"schema_version": prop("integer", "version of the kind's payload schema; defaults to 1"),
	"key":            prop("string", "idempotency key: a retry with the same key and content returns the stored row"),
	"summary":        prop("string", "one line saying what happened; required for a useful report"),
	"machine":        prop("string", "host name of the machine the agent runs on"),
	"model":          prop("string", "model id of the agent"),
	"harness":        prop("string", "coding-agent harness, such as claude-code or codex"),
	"project":        prop("string", "project or repository name"),
	"occurred_at":    {"type": "string", "format": "date-time", "description": "RFC 3339 time of the event; defaults to the time of the call"},
	"context":        {"type": "object", "description": "string-valued metadata such as git_branch or git_commit"},
	"payload":        {"type": "object", "description": "kind-specific body; friction: category, details, suggested_fix"},
}

func submitSchema(preset bool) map[string]any {
	props := map[string]any{}
	for _, name := range envelope.Members() {
		if preset && name == "project" {
			continue
		}
		t, ok := envelopeTypes[name]
		if !ok {
			panic(fmt.Sprintf("mcp: no input schema for envelope member %s", name))
		}
		props[name] = t
	}
	return object(props, true)
}

// filterProps are the filter arguments; project is left out under a preset.
func filterProps(preset bool) map[string]any {
	props := map[string]any{
		"kind":           prop("string", "only this kind"),
		"schema_version": prop("integer", "only this payload schema version"),
		"key":            prop("string", "only this idempotency key"),
		"machine":        prop("string", "only this machine"),
		"model":          prop("string", "only this model"),
		"project":        prop("string", "only this project"),
		"harness":        prop("string", "only this harness"),
		"category":       prop("string", "only this payload category"),
		"fix_status":     prop("string", "only this payload fix_status"),
		"exclude_kind":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "leave out these kinds"},
		"verdict":        prop("string", "only this processing verdict"),
		"processed":      prop("boolean", "true: processed only; false: open only"),
		"redacted":       prop("boolean", "true: redacted only; false: not redacted"),
		"content_hash":   prop("string", "only this content hash"),
		"since":          map[string]any{"type": "string", "format": "date-time", "description": "on or after this RFC 3339 time"},
		"until":          map[string]any{"type": "string", "format": "date-time", "description": "before this RFC 3339 time"},
		"on":             prop("string", "time field since and until apply to: created_at, occurred_at or processed_at"),
		"q":              prop("string", "text search over summary and payload"),
	}
	if preset {
		delete(props, "project")
	}
	return props
}

func listSchema(preset bool) map[string]any {
	props := filterProps(preset)
	props["before_id"] = prop("integer", "page cursor: ids below this one, newest first")
	props["after_id"] = prop("integer", "page cursor: ids above this one, oldest first")
	props["limit"] = prop("integer", "rows per page")
	props["include"] = map[string]any{"type": "string", "enum": []string{"payload"}, "description": "payload: include each row's payload"}
	return object(props, false)
}

func statsSchema(preset bool) map[string]any {
	props := filterProps(preset)
	props["by"] = prop("string", "comma-separated group keys, such as kind,project")
	props["top"] = prop("integer", "groups and recurrences to return")
	props["bucket"] = prop("string", "time series bucket: day or week")
	return object(props, false)
}

var (
	getSchemaInput = object(map[string]any{
		"id": prop("integer", "submission id"),
	}, false)
	markSchema = object(map[string]any{
		"ids":          map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "submission ids"},
		"processed":    prop("boolean", "true marks processed, false reopens"),
		"verdict":      prop("string", "outcome such as fixed, duplicate or wontfix"),
		"resolution":   prop("string", "what was done"),
		"ref":          prop("string", "reference to the fix, such as a commit or ticket"),
		"processed_by": prop("string", "who processed it"),
	}, false)
	schemaInput = object(map[string]any{
		"kind":    prop("string", "kind name, or envelope; with version"),
		"version": prop("integer", "schema version; with kind"),
	}, false)
)

const submitDescription = "File feedback at the end of a task when something slowed you down: a missing or wrong doc, " +
	"a tool that behaved unlike its name, stale config, several attempts at something that should have been written down. " +
	`Send kind "friction", a one-line summary (required) and payload {category, details, suggested_fix}. ` +
	"Never include credentials, tokens or private data. Returns the stored submission and any warnings; " +
	"an identical retry returns the existing row."

func (t *tools) register(s *sdk.Server) {
	preset := t.preset != ""
	s.AddTool(&sdk.Tool{Name: ToolSubmit, Description: submitDescription,
		InputSchema: submitSchema(preset), Annotations: annotations(false, false)}, t.submit)
	s.AddTool(&sdk.Tool{Name: ToolList,
		Description: "List submissions newest first, filtered like GET /api/v1/submissions. Use it to read the feedback queue or check for duplicates.",
		InputSchema: listSchema(preset), Annotations: annotations(true, false)}, t.list)
	s.AddTool(&sdk.Tool{Name: ToolGet, Description: "Get one submission by id, with its payload.",
		InputSchema: getSchemaInput, Annotations: annotations(true, false)}, t.get)
	s.AddTool(&sdk.Tool{Name: ToolStats,
		Description: "Counts over the submissions matching the filters: totals, groups by key, recurrences and time buckets.",
		InputSchema: statsSchema(preset), Annotations: annotations(true, false)}, t.stats)
	s.AddTool(&sdk.Tool{Name: ToolMark,
		Description: "Mark submissions processed (or reopen them) with a verdict, resolution and reference. Marks are reversible.",
		InputSchema: markSchema, Annotations: annotations(false, false)}, t.mark)
	s.AddTool(&sdk.Tool{Name: ToolGetSchema,
		Description: "With kind and version: that JSON Schema. With neither: the list of schemas the server ships.",
		InputSchema: schemaInput, Annotations: annotations(true, false)}, t.schema)
}

// result answers with body as structured content and as one text block.
func result(body []byte, isError bool) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(body)}},
		StructuredContent: json.RawMessage(body),
		IsError:           isError,
	}
}

// respond turns a core answer into a tool result: v (raw bytes as they are,
// anything else marshalled as the REST layer does) or the REST error body.
func (t *tools) respond(ctx context.Context, v any, err error) (*sdk.CallToolResult, error) {
	if err == nil {
		var body []byte
		if raw, ok := v.([]byte); ok {
			body = raw
		} else if body, err = core.Marshal(v); err != nil {
			err = fmt.Errorf("encode tool result: %w", err)
		}
		if err == nil {
			return result(body, false), nil
		}
	}
	return result(t.errorBody(ctx, err), true), nil
}

func (t *tools) submit(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	body := req.Params.Arguments
	if isAbsent(body) {
		body = json.RawMessage("{}")
	}
	if t.preset != "" {
		var err error
		if body, err = withPreset(body, t.preset); err != nil {
			return t.respond(ctx, nil, err)
		}
	}
	res, err := t.svc.Create(ctx, body)
	if t.observeCreate != nil {
		t.observeCreate(res.Created, err)
	}
	if n := res.Scrubbed.Total(); err == nil && n > 0 {
		// Counts only: the scrubbed text is never logged.
		slog.InfoContext(ctx, "ingest scrubbed", "id", res.Record.ID, "uid", res.Record.UID,
			"total", n, "counts", res.Scrubbed.String(), "tool", ToolSubmit)
	}
	return t.respond(ctx, res, err)
}

func (t *tools) list(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	allowed := listArgs
	if t.preset != "" {
		allowed = without(listArgs, "project")
	}
	a, err := readArgs(req.Params.Arguments, allowed)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	p, err := a.listParams(t.preset)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	res, err := t.svc.List(ctx, p)
	return t.respond(ctx, res, err)
}

func (t *tools) get(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	a, err := readArgs(req.Params.Arguments, getArgs)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	id, err := a.int64("id")
	if err == nil && id == nil {
		err = invalid(detailRequired, "?id", "id is required: the submission id")
	}
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	rec, err := t.svc.Get(ctx, *id)
	return t.respond(ctx, rec, err)
}

func (t *tools) stats(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	allowed := statsArgs
	if t.preset != "" {
		allowed = without(statsArgs, "project")
	}
	a, err := readArgs(req.Params.Arguments, allowed)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	p, err := a.statsParams(t.preset)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	res, err := t.svc.Stats(ctx, p)
	return t.respond(ctx, res, err)
}

func (t *tools) mark(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	body := req.Params.Arguments
	if isAbsent(body) {
		body = json.RawMessage("{}")
	}
	res, err := t.svc.MarkBatch(ctx, body)
	return t.respond(ctx, res, err)
}

// schemaList is the REST body of GET /api/v1/schemas.
type schemaList struct {
	Schemas any `json:"schemas"`
}

func (t *tools) schema(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	a, err := readArgs(req.Params.Arguments, schemaArgs)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	kind, err := a.str("kind")
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	version, err := a.int64("version")
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	switch {
	case kind == "" && version == nil:
		return t.respond(ctx, schemaList{t.svc.Schemas()}, nil)
	case kind == "":
		return t.respond(ctx, nil, invalid(detailRequired, "?kind", "kind is required with version; send both, or neither to list the schemas"))
	case version == nil:
		return t.respond(ctx, nil, invalid(detailRequired, "?version", "version is required with kind; send both, or neither to list the schemas"))
	}
	doc, err := t.svc.Schema(kind, *version)
	if err != nil {
		return t.respond(ctx, nil, err)
	}
	return t.respond(ctx, doc, nil)
}
