package core

import (
	"fmt"
	"slices"

	"github.com/agentfeedback/agentfeedback/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// MetaLimits are the limits a client needs to know.
type MetaLimits struct {
	BodyBytes          int `json:"body_bytes"`
	ImportBytes        int `json:"import_bytes"`
	ListMax            int `json:"list_max"`
	ListMaxWithPayload int `json:"list_max_with_payload"`
	ProcessedIDsMax    int `json:"processed_ids_max"`
	ContextEntries     int `json:"context_entries"`
	ContextValueBytes  int `json:"context_value_bytes"`
	IdentifierBytes    int `json:"identifier_bytes"`
	SummaryBytes       int `json:"summary_bytes"`
}

// MetaClient tells a client which versions the server expects.
type MetaClient struct {
	MinVersion  string `json:"min_version"`
	LatestKnown string `json:"latest_known"`
}

// Meta is the service metadata.
type Meta struct {
	ServiceVersion string         `json:"service_version"`
	APIVersion     string         `json:"api_version"`
	ExportFormat   int            `json:"export_format"`
	DedupeWindowS  int64          `json:"dedupe_window_s"`
	Limits         MetaLimits     `json:"limits"`
	Kinds          []schema.Entry `json:"kinds"`
	Features       []string       `json:"features"`
	Client         MetaClient     `json:"client"`
}

// limits reads the member limits from the compiled envelope schema, where
// the decoder reads them too.
var limits = func() MetaLimits {
	env := schema.Envelope()
	prop := func(name string) *schema.Schema {
		p, ok := env.Property(name)
		if !ok {
			panic(fmt.Sprintf("core: envelope schema lacks %s", name))
		}
		return p
	}
	context := prop("context")
	entries, ok := context.MaxProperties()
	if !ok || context.AdditionalProperties() == nil {
		panic("core: envelope schema lacks the context limits")
	}
	return MetaLimits{
		BodyBytes:          envelope.BodyLimit,
		ImportBytes:        ImportLimit,
		ListMax:            ListMax,
		ListMaxWithPayload: ListMaxWithPayload,
		ProcessedIDsMax:    ProcessedIDsMax,
		ContextEntries:     entries,
		ContextValueBytes:  context.AdditionalProperties().MaxBytes(),
		IdentifierBytes:    prop("machine").MaxBytes(),
		SummaryBytes:       prop("summary").MaxBytes(),
	}
}()

// Meta returns the service metadata: versions from the Config, the limits,
// the kinds with schemas (the envelope excluded) and the configured features.
func (s *Service) Meta() Meta {
	var kinds []schema.Entry
	for _, e := range schema.List() {
		if e.Kind != schema.EnvelopeKind {
			kinds = append(kinds, e)
		}
	}
	features := slices.Clone(s.config.Features)
	if features == nil {
		features = []string{}
	}
	return Meta{
		ServiceVersion: s.config.Version,
		APIVersion:     APIVersion,
		ExportFormat:   ExportFormat,
		DedupeWindowS:  int64(DedupeWindow.Seconds()),
		Limits:         limits,
		Kinds:          kinds,
		Features:       features,
		Client:         MetaClient{MinVersion: s.config.Version, LatestKnown: s.config.Version},
	}
}

// Schemas lists every schema the server ships, the envelope included.
func (s *Service) Schemas() []schema.Entry { return schema.List() }

// Schema returns the schema document of (kind, version) verbatim; the
// envelope is kind "envelope". version must be positive (400); an unknown
// pair is 404.
func (s *Service) Schema(kind string, version int64) ([]byte, error) {
	if version <= 0 {
		return nil, invalid("out_of_range", "/version", "version must be a positive integer, got %d", version)
	}
	doc, ok := schema.Document(kind, uint64(version))
	if !ok {
		return nil, &Problem{Status: 404, Code: CodeNotFound, Message: fmt.Sprintf("no schema for kind %s version %d", short(kind), version)}
	}
	return doc, nil
}
