package api

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// Detail codes of the query grammar and path parameters, in core's
// vocabulary where core has the same notion for body members.
const (
	detailUnknown  = "unknown_field"  // unknown parameter name
	detailEmpty    = "empty"          // parameter sent without a value
	detailRepeated = "duplicate_key"  // singleton parameter sent more than once
	detailType     = "type_mismatch"  // integer or boolean that is not one
	detailFormat   = "invalid_format" // timestamp that is not RFC 3339; malformed path id
	detailRange    = "out_of_range"   // enum value not accepted; integer past int64 (or int)
)

// Parameter sets per route, from docs/openapi.yaml.
var (
	filterParams = []string{"kind", "schema_version", "key", "machine", "model", "project", "harness",
		"category", "origin", "fix_status", "exclude_kind", "verdict", "processed", "redacted", "content_hash",
		"since", "until", "on", "q"}
	listParams   = append(slices.Clone(filterParams), "before_id", "after_id", "limit", "include")
	statsParams  = append(slices.Clone(filterParams), "by", "top", "bucket")
	exportParams = []string{"kind", "since", "after_id", "limit"}
)

// repeatable are the parameters that may appear more than once.
var repeatable = map[string]bool{"exclude_kind": true}

var plainInteger = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// short quotes v for a message, truncated when long.
func short(v string) string {
	const limit = 64
	if len(v) > limit {
		cut, _ := schema.TruncateBytes(v, limit)
		v = cut + "…"
	}
	return strconv.Quote(v)
}

// query is a request's parsed query string, checked against a route's
// parameter set: unknown names, empty values and repeated singletons are
// rejected up front; the typed getters reject values of the wrong shape.
type query url.Values

// parseQuery parses r's query against allowed. The raw query is decoded pair
// by pair (split on &, as url.ParseQuery does, with a ; rejected) so a
// malformed percent-encoding names its parameter: an undecodable value
// reports ?<name>, an undecodable name reports ? with the raw name. Names are
// checked in sorted order so the reported error is deterministic.
func parseQuery(r *http.Request, allowed []string) (query, error) {
	vals := url.Values{}
	badValue := map[string]bool{}
	var badNames []string
	for pair := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if pair == "" {
			continue
		}
		if strings.Contains(pair, ";") {
			return nil, invalid(detailFormat, "?", "the query string is not valid URL encoding: ; is not a separator; use &")
		}
		rawName, rawValue, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			badNames = append(badNames, rawName)
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			badValue[name] = true
		}
		vals[name] = append(vals[name], value)
	}
	if len(badNames) > 0 {
		slices.Sort(badNames)
		return nil, invalid(detailFormat, "?", fmt.Sprintf("query parameter name %s is not valid URL encoding", short(badNames[0])))
	}
	names := make([]string, 0, len(vals))
	for name := range vals {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		ptr := "?" + name
		if !slices.Contains(allowed, name) {
			accepted := "none"
			if len(allowed) > 0 {
				accepted = strings.Join(allowed, ", ")
			}
			return nil, invalid(detailUnknown, ptr, fmt.Sprintf("query parameter %s is not accepted by this route; accepted: %s", short(name), accepted))
		}
		if badValue[name] {
			return nil, invalid(detailFormat, ptr, fmt.Sprintf("query parameter %s has a value that is not valid URL encoding", name))
		}
		vs := vals[name]
		if len(vs) > 1 && !repeatable[name] {
			return nil, invalid(detailRepeated, ptr, fmt.Sprintf("query parameter %s appears %d times; send it once", name, len(vs)))
		}
		for _, v := range vs {
			if v == "" {
				return nil, invalid(detailEmpty, ptr, fmt.Sprintf("query parameter %s has an empty value; omit it or give a value", name))
			}
		}
	}
	return query(vals), nil
}

func (q query) str(name string) string { return url.Values(q).Get(name) }

func (q query) int64(name string) (*int64, error) {
	v, ok := q[name]
	if !ok {
		return nil, nil
	}
	if !plainInteger.MatchString(v[0]) {
		return nil, invalid(detailType, "?"+name, fmt.Sprintf("%s must be an integer written as plain digits (no sign, no leading zero), got %s", name, short(v[0])))
	}
	n, err := strconv.ParseInt(v[0], 10, 64)
	if err != nil {
		return nil, invalid(detailRange, "?"+name, fmt.Sprintf("%s must be at most %d, got %s", name, int64(math.MaxInt64), short(v[0])))
	}
	return &n, nil
}

func (q query) int(name string) (*int, error) {
	n, err := q.int64(name)
	if n == nil || err != nil {
		return nil, err
	}
	if *n > math.MaxInt {
		return nil, invalid(detailRange, "?"+name, fmt.Sprintf("%s must be at most %d, got %s", name, math.MaxInt, short(q[name][0])))
	}
	v := int(*n)
	return &v, nil
}

func (q query) bool(name string) (*bool, error) {
	v, ok := q[name]
	if !ok {
		return nil, nil
	}
	switch v[0] {
	case "true":
		b := true
		return &b, nil
	case "false":
		b := false
		return &b, nil
	}
	return nil, invalid(detailType, "?"+name, fmt.Sprintf("%s must be true or false, got %s", name, short(v[0])))
}

func (q query) time(name string) (*time.Time, error) {
	v, ok := q[name]
	if !ok {
		return nil, nil
	}
	t, ok := schema.ParseDateTime(v[0])
	if !ok {
		return nil, invalid(detailFormat, "?"+name, fmt.Sprintf("%s must be an RFC 3339 timestamp such as 2026-09-27T10:00:00Z (percent-encode +), got %s", name, short(v[0])))
	}
	return &t, nil
}

// filter reads the filter set List and Stats share.
func (q query) filter() (core.Filter, error) {
	f := core.Filter{
		Kind: q.str("kind"), Key: q.str("key"), Machine: q.str("machine"), Model: q.str("model"),
		Project: q.str("project"), Harness: q.str("harness"), Category: q.str("category"),
		Origin:    q.str("origin"),
		FixStatus: q.str("fix_status"), ExcludeKind: q["exclude_kind"], Verdict: q.str("verdict"),
		ContentHash: q.str("content_hash"), On: q.str("on"), Q: q.str("q"),
	}
	var err error
	if f.SchemaVersion, err = q.int64("schema_version"); err != nil {
		return f, err
	}
	if f.Processed, err = q.bool("processed"); err != nil {
		return f, err
	}
	if f.Redacted, err = q.bool("redacted"); err != nil {
		return f, err
	}
	if f.Since, err = q.time("since"); err != nil {
		return f, err
	}
	if f.Until, err = q.time("until"); err != nil {
		return f, err
	}
	return f, nil
}

func parseListParams(r *http.Request) (core.ListParams, error) {
	q, err := parseQuery(r, listParams)
	if err != nil {
		return core.ListParams{}, err
	}
	var p core.ListParams
	if p.Filter, err = q.filter(); err != nil {
		return p, err
	}
	if p.BeforeID, err = q.int64("before_id"); err != nil {
		return p, err
	}
	if p.AfterID, err = q.int64("after_id"); err != nil {
		return p, err
	}
	if p.Limit, err = q.int("limit"); err != nil {
		return p, err
	}
	if inc, ok := q["include"]; ok {
		if inc[0] != "payload" {
			return p, invalid(detailRange, "?include", fmt.Sprintf("include must be payload, got %s", short(inc[0])))
		}
		p.IncludePayload = true
	}
	return p, nil
}

func parseStatsParams(r *http.Request) (core.StatsParams, error) {
	q, err := parseQuery(r, statsParams)
	if err != nil {
		return core.StatsParams{}, err
	}
	var p core.StatsParams
	if p.Filter, err = q.filter(); err != nil {
		return p, err
	}
	if by, ok := q["by"]; ok {
		p.By = strings.Split(by[0], ",")
	}
	if p.Top, err = q.int("top"); err != nil {
		return p, err
	}
	p.Bucket = q.str("bucket")
	return p, nil
}

func parseExportParams(r *http.Request) (core.ExportParams, error) {
	q, err := parseQuery(r, exportParams)
	if err != nil {
		return core.ExportParams{}, err
	}
	p := core.ExportParams{Kind: q.str("kind")}
	if p.Since, err = q.time("since"); err != nil {
		return p, err
	}
	if p.AfterID, err = q.int64("after_id"); err != nil {
		return p, err
	}
	if p.Limit, err = q.int("limit"); err != nil {
		return p, err
	}
	return p, nil
}

// noQuery rejects any query parameter on a route that takes none.
func noQuery(r *http.Request) error {
	_, err := parseQuery(r, nil)
	return err
}

var pathInteger = regexp.MustCompile(`^[1-9][0-9]*$`)

// pathID parses the path parameter name as a positive integer.
func pathID(r *http.Request, name string) (int64, error) {
	v := r.PathValue(name)
	if !pathInteger.MatchString(v) {
		return 0, invalid(detailFormat, "/"+name, fmt.Sprintf("%s must be a positive integer matching ^[1-9][0-9]*$, got %s", name, short(v)))
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, invalid(detailRange, "/"+name, fmt.Sprintf("%s must be at most %d, got %s", name, int64(^uint64(0)>>1), short(v)))
	}
	return n, nil
}
