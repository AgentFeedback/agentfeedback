package mcp_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/pelletier/go-toml/v2"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
)

// coverageFile is coverage.toml: one entry per OpenAPI operation.
type coverageFile struct {
	Operations map[string]*opCoverage `toml:"operations"`
}

// opCoverage is one operation: the tool that calls it or the reason none
// does, and an entry per parameter and body property.
type opCoverage struct {
	Tool       string                    `toml:"tool"`
	Reason     string                    `toml:"reason"`
	Parameters map[string]*fieldCoverage `toml:"parameters"`
	Body       map[string]*fieldCoverage `toml:"body"`
}

// fieldCoverage maps one parameter or body property to a tool argument or a
// reason.
type fieldCoverage struct {
	Argument string `toml:"argument"`
	Reason   string `toml:"reason"`
}

func loadContract(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	return doc
}

func loadCoverage(t *testing.T) *coverageFile {
	t.Helper()
	data, err := os.ReadFile("coverage.toml")
	if err != nil {
		t.Fatal(err)
	}
	var cov coverageFile
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cov); err != nil {
		t.Fatalf("decode coverage.toml: %v", err)
	}
	return &cov
}

type fields struct{ params, body map[string]bool }

// contractFields is every parameter of every location (path-item parameters
// included) and every request-body property of every operation, by
// operationId, plus every problem that keeps an operation from being checked.
func contractFields(doc *openapi3.T) (map[string]fields, []string) {
	out := map[string]fields{}
	where := map[string]string{}
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			at := method + " " + path
			if op.OperationID == "" {
				add("operation %s: no operationId", at)
				continue
			}
			if prev, ok := where[op.OperationID]; ok {
				add("operation %s: duplicate operationId on %s and %s", op.OperationID, prev, at)
				continue
			}
			where[op.OperationID] = at
			f := fields{map[string]bool{}, map[string]bool{}}
			for _, list := range []openapi3.Parameters{item.Parameters, op.Parameters} {
				for _, p := range list {
					if p.Value != nil {
						f.params[p.Value.Name] = true
					}
				}
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				for ct, mt := range op.RequestBody.Value.Content {
					if mt == nil || mt.Schema == nil || mt.Schema.Value == nil {
						add("operation %s: body %s: no schema", op.OperationID, ct)
						continue
					}
					v := mt.Schema.Value
					if len(v.AllOf)+len(v.OneOf)+len(v.AnyOf) > 0 {
						add("operation %s: body %s: composed schema; teach the check to walk it", op.OperationID, ct)
					}
					for name := range v.Properties {
						f.body[name] = true
					}
				}
			}
			out[op.OperationID] = f
		}
	}
	return out, probs
}

// coverageProblems returns every discrepancy between the contract and the
// coverage file, in both directions, sorted.
func coverageProblems(doc *openapi3.T, cov *coverageFile) []string {
	contract, probs := contractFields(doc)
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	set := func(v string) bool { return strings.TrimSpace(v) != "" }
	for id, f := range contract {
		oc, ok := cov.Operations[id]
		if !ok || oc == nil {
			add("operation %s: missing from coverage.toml", id)
			continue
		}
		if set(oc.Tool) == set(oc.Reason) {
			add("operation %s: needs exactly one of tool or reason", id)
		}
		for _, part := range []struct {
			name     string
			contract map[string]bool
			entries  map[string]*fieldCoverage
		}{{"parameter", f.params, oc.Parameters}, {"body property", f.body, oc.Body}} {
			for name := range part.contract {
				if _, ok := part.entries[name]; !ok {
					add("operation %s: %s %s: missing from coverage.toml", id, part.name, name)
				}
			}
			for name, fc := range part.entries {
				if !part.contract[name] {
					add("operation %s: %s %s: not in openapi.yaml", id, part.name, name)
					continue
				}
				if fc == nil || set(fc.Argument) == set(fc.Reason) {
					add("operation %s: %s %s: needs exactly one non-empty argument or reason", id, part.name, name)
				}
			}
		}
	}
	for id := range cov.Operations {
		if _, ok := contract[id]; !ok {
			add("operation %s: not in openapi.yaml", id)
		}
	}
	sort.Strings(probs)
	return probs
}

// registered is the input-schema properties of every tool a server lists.
type registered map[string]map[string]any

func listTools(t *testing.T, e *env, path string) registered {
	t.Helper()
	res, err := e.connect(t, path, "").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := registered{}
	for _, tool := range res.Tools {
		out[tool.Name] = properties(t, tool)
	}
	return out
}

// toolProblems returns every tool or argument of the coverage file the
// server lacks, sorted.
func toolProblems(cov *coverageFile, tools registered) []string {
	var probs []string
	for id, oc := range cov.Operations {
		if oc == nil || oc.Tool == "" {
			continue
		}
		props, ok := tools[oc.Tool]
		if !ok {
			probs = append(probs, fmt.Sprintf("operation %s: tool %q is not registered", id, oc.Tool))
			continue
		}
		for _, entries := range []map[string]*fieldCoverage{oc.Parameters, oc.Body} {
			for name, fc := range entries {
				if fc == nil || fc.Argument == "" {
					continue
				}
				if _, ok := props[fc.Argument]; !ok {
					probs = append(probs, fmt.Sprintf("operation %s: %s maps to argument %s, which %s does not take", id, name, fc.Argument, oc.Tool))
				}
			}
		}
	}
	sort.Strings(probs)
	return probs
}

func TestCoverage_EveryOperationMapped(t *testing.T) {
	if probs := coverageProblems(loadContract(t), loadCoverage(t)); len(probs) > 0 {
		t.Fatalf("coverage.toml disagrees with docs/openapi.yaml:\n%s", strings.Join(probs, "\n"))
	}
}

func TestCoverage_ToolsAndArgumentsExist(t *testing.T) {
	e := newEnv(t, api.Config{})
	if probs := toolProblems(loadCoverage(t), listTools(t, e, "/mcp")); len(probs) > 0 {
		t.Fatalf("coverage.toml names what the tools lack:\n%s", strings.Join(probs, "\n"))
	}
	preset := listTools(t, e, "/mcp/p")
	for _, name := range []string{mcp.ToolSubmit, mcp.ToolList, mcp.ToolStats} {
		if _, ok := preset[name]["project"]; ok {
			t.Errorf("%s takes project on /mcp/{project}", name)
		}
	}
}

func TestCoverage_DetectsDrift(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(doc *openapi3.T, cov *coverageFile)
	}{
		{"operation entry deleted", "operation getStats: missing", func(_ *openapi3.T, cov *coverageFile) {
			delete(cov.Operations, "getStats")
		}},
		{"parameter entry deleted", "parameter limit: missing", func(_ *openapi3.T, cov *coverageFile) {
			delete(cov.Operations["listSubmissions"].Parameters, "limit")
		}},
		{"body entry deleted", "body property summary: missing", func(_ *openapi3.T, cov *coverageFile) {
			delete(cov.Operations["createSubmission"].Body, "summary")
		}},
		{"query parameter added", "parameter brand_new: missing", func(doc *openapi3.T, _ *coverageFile) {
			op := doc.Paths.Find("/api/v1/stats").Get
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewQueryParameter("brand_new")})
		}},
		{"body property added", "body property brand_new: missing", func(doc *openapi3.T, _ *coverageFile) {
			s := doc.Paths.Find("/api/v1/submissions/processed").Post.RequestBody.Value.Content["application/json"].Schema.Value
			s.Properties["brand_new"] = openapi3.NewStringSchema().NewRef()
		}},
		{"stale operation entry", "operation gone: not in openapi.yaml", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["gone"] = &opCoverage{Tool: "stats"}
		}},
		{"stale parameter entry", "parameter gone: not in openapi.yaml", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getStats"].Parameters["gone"] = &fieldCoverage{Argument: "gone"}
		}},
		{"tool and reason both set", "needs exactly one of tool or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getStats"].Reason = "x"
		}},
		{"whitespace reason", "operation getMetrics: needs exactly one of tool or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getMetrics"].Reason = "  "
		}},
		{"field with argument and reason", "needs exactly one non-empty argument or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getStats"].Parameters["top"].Reason = "x"
		}},
		{"composed body schema", "operation markSubmissions: body application/json: composed schema; teach the check to walk it", func(doc *openapi3.T, _ *coverageFile) {
			s := doc.Paths.Find("/api/v1/submissions/processed").Post.RequestBody.Value.Content["application/json"].Schema.Value
			s.AllOf = append(s.AllOf, openapi3.NewObjectSchema().NewRef())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, cov := loadContract(t), loadCoverage(t)
			tt.mutate(doc, cov)
			probs := coverageProblems(doc, cov)
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Fatalf("want a problem containing %q, got %q", tt.want, probs)
			}
		})
	}
}

func TestCoverage_DetectsToolDrift(t *testing.T) {
	e := newEnv(t, api.Config{})
	tools := listTools(t, e, "/mcp")
	for _, tt := range []struct {
		name   string
		want   string
		mutate func(cov *coverageFile)
	}{
		{"tool not registered", `tool "lst" is not registered`, func(cov *coverageFile) {
			cov.Operations["listSubmissions"].Tool = "lst"
		}},
		{"argument the tool lacks", "maps to argument limitx", func(cov *coverageFile) {
			cov.Operations["listSubmissions"].Parameters["limit"].Argument = "limitx"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cov := loadCoverage(t)
			tt.mutate(cov)
			probs := toolProblems(cov, tools)
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Fatalf("want a problem containing %q, got %q", tt.want, probs)
			}
		})
	}
}
