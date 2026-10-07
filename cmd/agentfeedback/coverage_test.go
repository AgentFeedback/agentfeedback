package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/pelletier/go-toml/v2"
)

// coverageFile is coverage.toml: one entry per OpenAPI operation.
type coverageFile struct {
	Operations map[string]*opCoverage `toml:"operations"`
}

// opCoverage is one operation: the command that calls it or the reason none
// does, the other commands that send through it, and an entry per parameter
// and body property.
type opCoverage struct {
	Command    string                    `toml:"command"`
	Also       []string                  `toml:"also"`
	Reason     string                    `toml:"reason"`
	Parameters map[string]*fieldCoverage `toml:"parameters"`
	Body       map[string]*fieldCoverage `toml:"body"`
}

// fieldCoverage maps one parameter or body property to a flag, a positional
// argument, or a reason.
type fieldCoverage struct {
	Flag   string `toml:"flag"`
	Arg    string `toml:"arg"`
	Reason string `toml:"reason"`
}

// helpArgs are the arguments between a command and -h that reach the flag
// set carrying its flags: submit friction has the generic submit flags plus
// the friction payload flags.
var helpArgs = map[string][]string{
	"submit": {"friction"},
}

func loadCoverageContract(t *testing.T) *openapi3.T {
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

// contractFields is every parameter of every location (path-item parameters
// included) and every request-body property of every operation, by
// operationId, plus every problem that keeps an operation from being checked.
func contractFields(doc *openapi3.T) (map[string]struct{ params, body map[string]bool }, []string) {
	out := map[string]struct{ params, body map[string]bool }{}
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
			params, body := map[string]bool{}, map[string]bool{}
			for _, list := range []openapi3.Parameters{item.Parameters, op.Parameters} {
				for _, p := range list {
					if p.Value != nil {
						params[p.Value.Name] = true
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
						body[name] = true
					}
				}
			}
			out[op.OperationID] = struct{ params, body map[string]bool }{params, body}
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
	for id, fields := range contract {
		oc, ok := cov.Operations[id]
		if !ok || oc == nil {
			add("operation %s: missing from coverage.toml", id)

			continue
		}
		if set(oc.Command) == set(oc.Reason) {
			add("operation %s: needs exactly one of command or reason", id)
		}
		for _, part := range []struct {
			name     string
			contract map[string]bool
			entries  map[string]*fieldCoverage
		}{{"parameter", fields.params, oc.Parameters}, {"body property", fields.body, oc.Body}} {
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
				n := 0
				for _, v := range []string{fc.Flag, fc.Arg, fc.Reason} {
					if set(v) {
						n++
					}
				}
				if n != 1 {
					add("operation %s: %s %s: needs exactly one non-empty flag, arg or reason", id, part.name, name)
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

// isolateCLI keeps run away from the user's config file and environment.
func isolateCLI(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv("HOME", t.TempDir())
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "AGENT_FEEDBACK_") {
			t.Setenv(name, "")
		}
	}
}

// cliProblems returns every command, flag or argument of the coverage file
// the CLI lacks, sorted. It runs each named command with -h.
func cliProblems(cov *coverageFile) []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	usage := map[string]string{}
	flagRE := map[string]*regexp.Regexp{}
	for id, oc := range cov.Operations {
		if oc == nil {
			continue
		}
		for _, name := range oc.Also {
			if !slices.ContainsFunc(commands, func(c command) bool { return c.name == name && c.run != nil }) {
				add("operation %s: also-command %q is not in the router", id, name)
			}
		}
		if oc.Command == "" {
			continue
		}
		i := slices.IndexFunc(commands, func(c command) bool { return c.name == oc.Command && c.run != nil })
		if i < 0 {
			add("operation %s: command %q is not in the router", id, oc.Command)

			continue
		}
		out, ok := usage[oc.Command]
		if !ok {
			args := append(append([]string{oc.Command}, helpArgs[oc.Command]...), "-h")
			var buf bytes.Buffer
			if rc := run(args, strings.NewReader(""), &buf, &buf); rc != 0 || buf.Len() == 0 {
				add("operation %s: %s exited %d with %d bytes of output", id, strings.Join(args, " "), rc, buf.Len())
			}
			out = buf.String()
			usage[oc.Command] = out
		}
		for _, entries := range []map[string]*fieldCoverage{oc.Parameters, oc.Body} {
			for name, fc := range entries {
				if fc == nil {
					continue
				}
				if fc.Arg != "" && !strings.Contains(commands[i].summary, fc.Arg) && !strings.Contains(out, fc.Arg) {
					add("operation %s: %s maps to %s, which the %s synopsis does not name", id, name, fc.Arg, oc.Command)
				}
				if fc.Flag == "" {
					continue
				}
				flagName := strings.TrimLeft(fc.Flag, "-")
				re, ok := flagRE[flagName]
				if !ok {
					re = regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(flagName) + `(\s|$)`)
					flagRE[flagName] = re
				}
				if !re.MatchString(out) {
					add("operation %s: %s maps to %s, which %s -h does not list", id, name, fc.Flag, oc.Command)
				}
			}
		}
	}
	sort.Strings(probs)

	return probs
}

func TestCoverage_EveryOperationMapped(t *testing.T) {
	if probs := coverageProblems(loadCoverageContract(t), loadCoverage(t)); len(probs) > 0 {
		t.Fatalf("coverage.toml disagrees with docs/openapi.yaml:\n%s", strings.Join(probs, "\n"))
	}
}

func TestCoverage_CommandsAndFlagsExist(t *testing.T) {
	isolateCLI(t)
	if probs := cliProblems(loadCoverage(t)); len(probs) > 0 {
		t.Fatalf("coverage.toml names what the CLI lacks:\n%s", strings.Join(probs, "\n"))
	}
}

func TestCoverage_DetectsCLIDrift(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(cov *coverageFile)
	}{
		{"command not in router", `command "lst" is not in the router`, func(cov *coverageFile) {
			cov.Operations["listSubmissions"].Command = "lst"
		}},
		{"flag the command lacks", "maps to --limitx", func(cov *coverageFile) {
			cov.Operations["listSubmissions"].Parameters["limit"].Flag = "--limitx"
		}},
		{"prefix-only flag", "maps to --processed-b,", func(cov *coverageFile) {
			cov.Operations["markSubmissions"].Body["processed_by"].Flag = "--processed-b"
		}},
		{"arg not in synopsis", "maps to <uid>", func(cov *coverageFile) {
			cov.Operations["getSubmission"].Parameters["id"].Arg = "<uid>"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateCLI(t)
			cov := loadCoverage(t)
			tt.mutate(cov)
			probs := cliProblems(cov)
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Fatalf("want a problem containing %q, got %q", tt.want, probs)
			}
		})
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
			cov.Operations["gone"] = &opCoverage{Command: "list"}
		}},
		{"stale parameter entry", "parameter gone: not in openapi.yaml", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getStats"].Parameters["gone"] = &fieldCoverage{Flag: "--gone"}
		}},
		{"command and reason both set", "needs exactly one of command or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getStats"].Reason = "x"
		}},
		{"field with flag and reason", "needs exactly one non-empty flag, arg or reason", func(_ *openapi3.T, cov *coverageFile) {
			for _, fc := range cov.Operations["getStats"].Parameters {
				fc.Reason, fc.Flag = "x", "--x"
			}
		}},
		{"empty reason", "operation getMetrics: needs exactly one of command or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getMetrics"].Reason = ""
		}},
		{"whitespace reason", "operation getMetrics: needs exactly one of command or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["getMetrics"].Reason = "  "
		}},
		{"whitespace field reason", "parameter project: needs exactly one non-empty flag, arg or reason", func(_ *openapi3.T, cov *coverageFile) {
			cov.Operations["postMcpProject"].Parameters["project"].Reason = "  "
		}},
		{"duplicate operationId", "operation getStats: duplicate operationId on", func(doc *openapi3.T, _ *coverageFile) {
			doc.Paths.Find("/metrics").Get.OperationID = "getStats"
		}},
		{"composed body schema", "operation markSubmissions: body application/json: composed schema; teach the check to walk it", func(doc *openapi3.T, _ *coverageFile) {
			s := doc.Paths.Find("/api/v1/submissions/processed").Post.RequestBody.Value.Content["application/json"].Schema.Value
			s.AllOf = append(s.AllOf, openapi3.NewObjectSchema().NewRef())
		}},
		{"header parameter added", "operation getStats: parameter X-New: missing", func(doc *openapi3.T, _ *coverageFile) {
			op := doc.Paths.Find("/api/v1/stats").Get
			op.Parameters = append(op.Parameters, &openapi3.ParameterRef{Value: openapi3.NewHeaderParameter("X-New")})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, cov := loadCoverageContract(t), loadCoverage(t)
			tt.mutate(doc, cov)
			probs := coverageProblems(doc, cov)
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Fatalf("want a problem containing %q, got %q", tt.want, probs)
			}
		})
	}
}
