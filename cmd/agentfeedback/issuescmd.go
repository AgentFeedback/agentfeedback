package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"golang.org/x/term"
)

const (
	issuesSynopsis = "issues list|rank|detail|source-reports|prompt|correct|merge|split|record-outcome|recheck [issue UUID] [flags]"
	issueBodyLimit = 1024 * 1024
)

var issueUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// issueOnceValue prevents flag's last-value-wins behavior for scalar options.
type issueOnceValue struct {
	flag.Value
	seen bool
}

func (v *issueOnceValue) String() string {
	// flag.PrintDefaults calls String on a zero value.
	if v.Value == nil {
		return ""
	}
	return v.Value.String()
}

func (v *issueOnceValue) Set(s string) error {
	if v.seen {
		return fmt.Errorf("duplicate scalar flag")
	}
	v.seen = true
	return v.Value.Set(s)
}

func (v *issueOnceValue) IsBoolFlag() bool {
	b, ok := v.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func runIssues(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errArgs("issues", issuesSynopsis)
	}
	op := args[0]
	if op == "-h" || op == "--help" || op == "help" {
		fmt.Fprintf(stderr, "usage: agentfeedback %s\nRead operations accept hosted filters; mutations require --body-file PATH|-.\nUse agentfeedback issues <operation> --help for its flags.\n", issuesSynopsis)
		return flag.ErrHelp
	}
	method, suffix, perIssue := http.MethodGet, "", true
	switch op {
	case "list":
		perIssue = false
	case "rank":
		perIssue, suffix = false, "/rank"
	case "detail":
	case "source-reports", "prompt":
		suffix = "/" + op
	case "correct":
		method = http.MethodPatch
	case "merge", "split", "recheck":
		method, suffix = http.MethodPost, "/"+op
	case "record-outcome":
		method, suffix = http.MethodPost, "/outcomes"
	default:
		return usageErr("unknown issues operation "+strconv.Quote(op), "use agentfeedback "+issuesSynopsis)
	}
	mutation := method != http.MethodGet
	fs := newFlagSet("issues " + op)
	mf := addModeFlags(fs)
	asJSON := fs.Bool("json", false, "print the complete server JSON without changing scalar values")
	q := url.Values{}
	filters := map[string]*multiFlag{}
	if !mutation {
		for _, name := range []string{"repo", "project", "product", "team", "machine", "harness", "target_id", "target_kind", "assessment", "actionability", "resolution", "environment", "version"} {
			v := new(multiFlag)
			filters[name] = v
			fs.Var(v, strings.ReplaceAll(name, "_", "-"), "comma-separated values (repeatable; server limit 50 total)")
		}
	}
	scalars := map[string]*string{}
	addScalar := func(name string) {
		scalars[name] = fs.String(strings.ReplaceAll(name, "_", "-"), "", "hosted query parameter "+name)
	}
	if !mutation {
		for _, name := range []string{"external_tooling_environment", "from", "to"} {
			addScalar(name)
		}
		if op != "prompt" {
			addScalar("limit")
		}
		if op == "list" || op == "rank" || op == "source-reports" {
			addScalar("cursor")
		}
		if op == "rank" {
			addScalar("rollup")
		}
	}
	var bodyFile *string
	if mutation {
		bodyFile = fs.String("body-file", "", "required JSON object file, or - for stdin; at most 1 MiB; server validates evidence and expected_versions")
	}
	fs.VisitAll(func(f *flag.Flag) {
		if _, ok := f.Value.(*multiFlag); !ok {
			f.Value = &issueOnceValue{Value: f.Value}
		}
	})
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", issuesSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args[1:], stderr)
	if err != nil {
		return errFlags("issues "+op, err)
	}
	want := 0
	if perIssue {
		want = 1
	}
	if len(pos) != want {
		return errArgs("issues "+op, issuesSynopsis)
	}
	path, advertised := "/api/v1/issues", "/api/v1/issues"
	if perIssue {
		if !issueUUID.MatchString(pos[0]) {
			return usageErr("invalid issue UUID "+strconv.Quote(pos[0]), "pass an issue UUID, not a submission integer ID")
		}
		path += "/" + pos[0]
		advertised += "/{id}"
	}
	path, advertised = path+suffix, advertised+suffix
	set := visited(fs)
	for name, values := range filters {
		if len(*values) > 0 {
			q.Set(name, strings.Join(*values, ","))
		}
	}
	for name, value := range scalars {
		if set[strings.ReplaceAll(name, "_", "-")] {
			if *value == "" {
				return usageErr("empty --"+strings.ReplaceAll(name, "_", "-"), "provide a nonempty value")
			}
			q.Set(name, *value)
		}
	}
	if q.Has("from") != q.Has("to") {
		return usageErr("--from and --to must be supplied together", "pass both RFC 3339 UTC window bounds")
	}
	if text, ok := q["limit"]; ok {
		n, err := strconv.Atoi(text[0])
		if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != text[0] {
			return usageErr("--limit must be an integer from 1 to 100", "request one bounded page")
		}
	}
	var body []byte
	if mutation {
		if bodyFile == nil || *bodyFile == "" {
			return usageErr("--body-file is required", "pass a JSON object file or --body-file - for stdin; include explicit preconditions, reason and evidence")
		}
		body, err = readIssueBody(*bodyFile, stdin)
		if err != nil {
			return err
		}
	}
	c, err := apiClient(os.Getenv, *mf, stderr)
	if err != nil {
		return err
	}
	if err := issueCapability(c, method, advertised, stderr); err != nil {
		return err
	}
	resp, err := c.Do(context.Background(), method, path, q, body)
	if err != nil {
		return issueAPIError(err, stderr)
	}
	if !json.Valid(resp.Body) {
		return errBadResponse("issues "+op, errors.New("invalid JSON response"))
	}
	if *asJSON {
		return writeBody(stdout, resp.Body)
	}
	if op == "prompt" {
		var out struct {
			Prompt *string `json:"prompt"`
		}
		if err := json.Unmarshal(resp.Body, &out); err != nil || out.Prompt == nil {
			return errBadResponse("issues prompt", errors.New("missing string prompt"))
		}
		prompt := *out.Prompt
		if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			prompt = safe(prompt)
		}
		_, err := io.WriteString(stdout, prompt)
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, resp.Body, "", "  "); err != nil {
		return errBadResponse("issues "+op, err)
	}
	return writeBody(stdout, []byte(safe(pretty.String())))
}

func readIssueBody(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, usageErr("cannot read mutation body: "+err.Error(), "pass a readable --body-file PATH or - for stdin")
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	body, err := io.ReadAll(io.LimitReader(r, issueBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read mutation body: %w", err)
	}
	if len(body) > issueBodyLimit {
		return nil, usageErr("mutation body exceeds 1 MiB", "provide a bounded JSON object")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(trimmed) || !json.Valid(trimmed) {
		return nil, usageErr("mutation body must be one valid JSON object", "pass exactly one object, with no trailing JSON or other text")
	}
	return body, nil
}

// Discovery authenticates first: missing routes cannot disguise a refused key or outage.
func issueCapability(c *client.Client, method, path string, stderr io.Writer) error {
	meta, err := c.Do(context.Background(), http.MethodGet, "/api/v1/meta", nil, nil)
	if err != nil {
		return issueAPIError(err, stderr)
	}
	var metadata struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(meta.Body, &metadata); err != nil || metadata.APIVersion == "" {
		return errBadResponse("issue metadata discovery", errors.New("missing api_version or malformed metadata"))
	}
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if err != nil {
		return issueAPIError(err, stderr)
	}
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil || !strings.HasPrefix(doc.OpenAPI, "3.") || doc.Paths == nil {
		return errBadResponse("issue capability discovery", errors.New("malformed OpenAPI document"))
	}
	item, exists := doc.Paths[path]
	if !exists && strings.HasPrefix(path, "/api/v1/issues/{id}") {
		suffix := strings.TrimPrefix(path, "/api/v1/issues/{id}")
		for template, candidate := range doc.Paths {
			rest, parameter := strings.CutPrefix(template, "/api/v1/issues/{")
			if !parameter {
				continue
			}
			name, tail, closed := strings.Cut(rest, "}")
			if closed && name != "" && !strings.ContainsAny(name, "/{") && tail == suffix {
				item, exists = candidate, true
				break
			}
		}
	}
	if exists && item == nil {
		return errBadResponse("issue capability discovery", errors.New("malformed OpenAPI path item"))
	}
	operation, ok := item[strings.ToLower(method)]
	if !ok {
		return failErr("unsupported: server does not advertise "+method+" "+path, "use a server that advertises this issue operation; no backend switch was attempted")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(operation, &object); err != nil || object == nil {
		return errBadResponse("issue capability discovery", errors.New("malformed OpenAPI operation"))
	}
	return nil
}

// Preserve authorized conflict/version details, not just the usual 400 details.
func issueAPIError(err error, stderr io.Writer) error {
	var ae *client.APIError
	if errors.As(err, &ae) {
		for _, detail := range ae.Details {
			fmt.Fprintln(stderr, safe(string(detail)))
		}
		clean := *ae
		clean.Message, clean.RequestID, clean.Location = safe(ae.Message), safe(ae.RequestID), safe(ae.Location)
		mapped := apiErr(&clean, io.Discard)
		if ae.Status == http.StatusConflict || ae.Status == http.StatusPreconditionRequired {
			mapped = failErr(clean.Error(), "read current issue versions and evidence, then provide an explicit corrected --body-file; no automatic retry was attempted")
		}
		if ae.Code != "" {
			return fmt.Errorf("%s: %w", safe(ae.Code), mapped)
		}
		return mapped
	}
	return apiErr(err, stderr)
}
