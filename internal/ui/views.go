package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

const (
	submissionsPath = "/api/v1/submissions"
	statsPath       = "/api/v1/stats"
	metaPath        = "/api/v1/meta"
	pageSize        = "50"
	installCheck    = "install-check"
)

var funcs = template.FuncMap{
	"states":  func() []string { return []string{"all", "open", "processed"} },
	"presets": func() []string { return []string{"all", "1h", "24h", "7d", "30d"} },
}

// textFilters are the free-text form fields, each passed through as the
// list parameter of the same name when not empty.
var textFilters = []string{"project", "category", "harness", "model", "origin", "kind"}

// sincePresets maps a form preset to how far back it reaches; "all" and
// empty send no since.
var sincePresets = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// form is the filter form, as submitted and as shown again.
type form struct {
	Values map[string]string
	State  string
	Since  string
}

// readForm reads the filter form from q and returns it with the API query
// it maps to. Kind not given excludes install-check, as list and stats do.
func (h *handler) readForm(q url.Values) (form, url.Values, error) {
	f := form{Values: map[string]string{}, State: strings.TrimSpace(q.Get("state")), Since: strings.TrimSpace(q.Get("since"))}
	api := url.Values{}
	for _, name := range textFilters {
		v := q.Get(name)
		if name != "origin" {
			// origin is matched exactly and stored verbatim, so it is
			// sent as typed.
			v = strings.TrimSpace(v)
		}
		f.Values[name] = v
		if v != "" {
			api.Set(name, v)
		}
	}
	if f.Values["kind"] == "" {
		api.Set("exclude_kind", installCheck)
	}
	switch f.State {
	case "", "all":
		f.State = "all"
	case "open":
		api.Set("processed", "false")
	case "processed":
		api.Set("processed", "true")
	default:
		return f, nil, fmt.Errorf("state must be open, processed or all, not %q", f.State)
	}
	switch d, ok := sincePresets[f.Since]; {
	case ok:
		api.Set("since", h.now().UTC().Add(-d).Format(time.RFC3339))
	case f.Since == "" || f.Since == "all":
		f.Since = "all"
	default:
		return f, nil, fmt.Errorf("since must be 1h, 24h, 7d, 30d or all, not %q", f.Since)
	}

	return f, api, nil
}

// formQuery is the form's own query string, for links that keep it.
func (f form) formQuery() url.Values {
	q := url.Values{}
	for _, name := range textFilters {
		if v := f.Values[name]; v != "" {
			q.Set(name, v)
		}
	}
	if f.State != "all" {
		q.Set("state", f.State)
	}
	if f.Since != "all" {
		q.Set("since", f.Since)
	}

	return q
}

// listRow is the part of a list row the queue shows.
type listRow struct {
	ID          int64           `json:"id"`
	CreatedAt   string          `json:"created_at"`
	Kind        string          `json:"kind"`
	Project     string          `json:"project"`
	Harness     string          `json:"harness"`
	Model       string          `json:"model"`
	Summary     string          `json:"summary"`
	ProcessedAt string          `json:"processed_at"`
	Verdict     string          `json:"verdict"`
	RedactedAt  string          `json:"redacted_at"`
	Context     json.RawMessage `json:"context"`
	Origin      string          `json:"-"`
}

type listBody struct {
	Submissions  []listRow `json:"submissions"`
	Total        int64     `json:"total"`
	HasMore      bool      `json:"has_more"`
	NextBeforeID *int64    `json:"next_before_id"`
}

// queueView is the queue page. Next and First are url.Values.Encode
// output, already query-escaped, so the template only normalises them.
type queueView struct {
	Form  form
	Rows  []listRow
	Total int64
	Next  template.URL
	First template.URL
	Paged bool
}

// queue lists one page of submissions, newest first, without payloads.
func (h *handler) queue(w http.ResponseWriter, r *http.Request) {
	f, q, err := h.readForm(r.URL.Query())
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err.Error())

		return
	}
	v := queueView{Form: f, First: template.URL(f.formQuery().Encode())}
	if b := r.URL.Query().Get("before_id"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < 1 {
			h.fail(w, r, http.StatusBadRequest, "before_id must be a positive integer")

			return
		}
		q.Set("before_id", b)
		v.Paged = true
	}
	q.Set("limit", pageSize)
	body, err := h.get(r.Context(), submissionsPath, q)
	if err != nil {
		h.upstream(w, r, err)

		return
	}
	var lb listBody
	if err := json.Unmarshal(body, &lb); err != nil {
		h.fail(w, r, http.StatusBadGateway, "the list response does not parse: "+err.Error())

		return
	}
	for i := range lb.Submissions {
		row := &lb.Submissions[i]
		var c struct {
			Origin string `json:"origin"`
		}
		_ = json.Unmarshal(row.Context, &c)
		row.Origin = c.Origin
	}
	v.Rows, v.Total = lb.Submissions, lb.Total
	if lb.HasMore && lb.NextBeforeID != nil {
		nq := f.formQuery()
		nq.Set("before_id", strconv.FormatInt(*lb.NextBeforeID, 10))
		v.Next = template.URL(nq.Encode())
	}
	h.render(w, r, http.StatusOK, "queue", page{Title: "Queue", Data: v})
}

// member is one top-level member of a record, in record order.
type member struct {
	Name   string
	Raw    json.RawMessage
	Scalar bool
	Text   string
}

type recordView struct {
	ID       int64
	Members  []member
	Redacted bool
	Warnings []schema.Detail
	Problem  string
}

// record shows one submission and the warnings its stored envelope
// decodes with again.
func (h *handler) record(w http.ResponseWriter, r *http.Request, id int64) {
	body, err := h.get(r.Context(), submissionsPath+"/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		h.upstream(w, r, err)

		return
	}
	members, err := orderedMembers(body)
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, "the record does not parse: "+err.Error())

		return
	}
	v := recordView{ID: id, Members: members}
	byName := map[string]json.RawMessage{}
	for _, m := range members {
		byName[m.Name] = m.Raw
	}
	if _, ok := byName["redacted_at"]; ok {
		v.Redacted = true
	} else {
		v.Warnings, err = recomputeWarnings(byName)
		if err != nil {
			v.Problem = err.Error()
		}
	}
	h.render(w, r, http.StatusOK, "record", page{Title: fmt.Sprintf("Submission %d", id), Data: v})
}

// recomputeWarnings rebuilds the envelope from the record's envelope
// members, in envelope order, and decodes it again. An inferred kind
// ("unknown") is left out so the decoder infers it again, as create did.
func recomputeWarnings(byName map[string]json.RawMessage) ([]schema.Detail, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, name := range envelope.Members() {
		v, ok := byName[name]
		if !ok || (name == "kind" && bytes.Equal(bytes.TrimSpace(v), []byte(`"unknown"`))) {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	_, warnings, err := envelope.DecodeWithLimits(b.Bytes(), b.Len(), envelope.MaxDepth+envelope.StoredDepthHeadroom)
	if err != nil {
		var rej *envelope.Rejection
		if errors.As(err, &rej) {
			return nil, errors.New("the stored envelope is refused: " + rej.Message)
		}

		return nil, err
	}

	return warnings, nil
}

// orderedMembers splits a JSON object into its members in order, keeping
// each value's bytes; objects and arrays are indented, scalars shown as
// text (a string unquoted).
func orderedMembers(body []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []member
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		m := member{Name: name, Raw: raw}
		switch raw[0] {
		case '{', '[':
			var ind bytes.Buffer
			if err := json.Indent(&ind, raw, "", "  "); err != nil {
				return nil, err
			}
			m.Text = ind.String()
		case '"':
			var s string
			_ = json.Unmarshal(raw, &s)
			m.Scalar, m.Text = true, s
		default:
			m.Scalar, m.Text = true, string(raw)
		}
		out = append(out, m)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the object")
	}

	return out, nil
}

// statsKeys are the group keys the stats page shows, one request each.
var statsKeys = []string{"kind", "project", "harness", "category", "origin"}

type statsBody struct {
	Total     int64 `json:"total"`
	Open      int64 `json:"open"`
	Processed int64 `json:"processed"`
	Redacted  int64 `json:"redacted"`
	Groups    []struct {
		Keys      map[string]any `json:"keys"`
		Total     int64          `json:"total"`
		Open      int64          `json:"open"`
		Processed int64          `json:"processed"`
	} `json:"groups"`
	Recurring []struct {
		ContentHash string `json:"content_hash"`
		Count       int64  `json:"count"`
		FirstID     int64  `json:"first_id"`
		LastID      int64  `json:"last_id"`
		Summary     string `json:"summary"`
		Kind        string `json:"kind"`
		Project     string `json:"project"`
	} `json:"recurring"`
}

type groupRow struct {
	Value                  string
	Total, Open, Processed int64
}

type statsGroup struct {
	Key  string
	Rows []groupRow
}

type statsView struct {
	Form    form
	Totals  statsBody
	Groups  []statsGroup
	Service string
	API     string
}

// stats shows the totals, one table per group key and the recurring
// content hashes, under the same filters as the queue.
func (h *handler) stats(w http.ResponseWriter, r *http.Request) {
	f, q, err := h.readForm(r.URL.Query())
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err.Error())

		return
	}
	v := statsView{Form: f}
	for i, key := range statsKeys {
		sq := url.Values{}
		for k, vs := range q {
			sq[k] = vs
		}
		sq.Set("by", key)
		if i == 0 {
			sq.Set("top", "10")
		} else {
			sq.Set("top", "0")
		}
		body, err := h.get(r.Context(), statsPath, sq)
		if err != nil {
			h.upstream(w, r, err)

			return
		}
		var s statsBody
		if err := json.Unmarshal(body, &s); err != nil {
			h.fail(w, r, http.StatusBadGateway, "the stats response does not parse: "+err.Error())

			return
		}
		if i == 0 {
			v.Totals = s
		}
		g := statsGroup{Key: key}
		for _, gr := range s.Groups {
			val := "(none)"
			if x, ok := gr.Keys[key]; ok && x != nil {
				val = fmt.Sprint(x)
			}
			g.Rows = append(g.Rows, groupRow{Value: val, Total: gr.Total, Open: gr.Open, Processed: gr.Processed})
		}
		v.Groups = append(v.Groups, g)
	}
	if body, err := h.get(r.Context(), metaPath, nil); err == nil {
		var m struct {
			Service string `json:"service_version"`
			API     string `json:"api_version"`
		}
		if json.Unmarshal(body, &m) == nil {
			v.Service, v.API = m.Service, m.API
		}
	}
	h.render(w, r, http.StatusOK, "stats", page{Title: "Stats", Data: v})
}
