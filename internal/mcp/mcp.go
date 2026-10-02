// Package mcp is the remote MCP server of the v1 contract (POST /mcp and
// POST /mcp/{project} in docs/openapi.yaml): six tools over internal/core,
// each one-to-one with a REST operation and answering with the same JSON
// body, served stateless over the Streamable HTTP transport. Authentication,
// the body limit and the error shape of transport failures belong to the
// HTTP layer that mounts the handler.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/skillgen"
	"github.com/agentfeedback/agentfeedback/pkg/envelope"
)

// Config wires a Handler.
type Config struct {
	Service *core.Service
	// Instructions is operator text appended to the generated server
	// instructions after a blank line; empty after trimming appends nothing.
	Instructions string
	// ErrorBody renders err as the REST error body (the contract's Error
	// shape) for a tool result.
	ErrorBody func(ctx context.Context, err error) []byte
	// BuildFailed answers a request whose server could not be built; it
	// must write a 500.
	BuildFailed func(w http.ResponseWriter, r *http.Request, err error)
	// ObserveCreate, when set, is told the outcome of every submit_feedback
	// call that reached core: created, or the error core returned.
	ObserveCreate func(created bool, err error)
}

// Handler serves MCP for any base URL and project preset.
type Handler struct {
	cfg  Config
	sdk  *sdk.StreamableHTTPHandler
	mu   sync.Mutex
	srvs map[connection]*sdk.Server
}

// connection is what a request selects: the base URL the instructions name
// and the project preset ("" for none).
type connection struct{ base, preset string }

type connectionKey struct{}

// RequestLimit is the largest /mcp request body: the create body limit plus
// room for the JSON-RPC message around the arguments. core still applies its
// own body limits to the arguments.
const RequestLimit = envelope.BodyLimit + 64<<10

// maxServers bounds the server cache: the base URL can come from the Host
// header, so the key set is client input.
const maxServers = 256

// New builds a Handler. It builds an unscoped and a preset server once, so
// a tool schema or instructions that cannot be built fail here, not on a
// request.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, srvs: map[connection]*sdk.Server{}}
	for _, c := range []connection{{"", ""}, {"", "p"}} {
		if _, err := h.build(c, ""); err != nil {
			panic("mcp: build server: " + err.Error())
		}
	}
	h.sdk = sdk.NewStreamableHTTPHandler(h.server, &sdk.StreamableHTTPOptions{
		Stateless: true,
		// A reverse proxy on the same host sends the public Host to a
		// loopback listener; the API key check stands against DNS rebinding.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        RequestLimit,
	})
	return h
}

// For returns the HTTP handler of one connection: base is the normalised
// base URL ("" renders the placeholder) and preset the normalised project
// ("" for /mcp).
func (h *Handler) For(base, preset string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := connection{base, preset}
		if _, err := h.lookup(c); err != nil {
			h.cfg.BuildFailed(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), connectionKey{}, c)
		h.sdk.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) server(r *http.Request) *sdk.Server {
	c, _ := r.Context().Value(connectionKey{}).(connection)
	s, _ := h.lookup(c)
	return s
}

// lookup returns the cached server of c, building it on first use.
func (h *Handler) lookup(c connection) (*sdk.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.srvs[c]; ok {
		return s, nil
	}
	s, err := h.build(c, h.cfg.Service.Meta().ServiceVersion)
	if err != nil {
		return nil, err
	}
	if len(h.srvs) >= maxServers {
		clear(h.srvs)
	}
	h.srvs[c] = s
	return s, nil
}

// Instructions returns the server instructions for base: the mcp form of the
// skill, then the operator text after a blank line when there is any.
func Instructions(base, extra string) (string, error) {
	b, err := skillgen.Render(skillgen.FormMCP, base)
	if err != nil {
		return "", err
	}
	text := string(b)
	if extra = strings.TrimSpace(extra); extra != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + extra
	}
	return text, nil
}

func (h *Handler) build(c connection, version string) (*sdk.Server, error) {
	instructions, err := Instructions(c.base, h.cfg.Instructions)
	if err != nil {
		return nil, err
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "agentfeedback", Version: version},
		&sdk.ServerOptions{Instructions: instructions})
	t := &tools{svc: h.cfg.Service, preset: c.preset, errorBody: h.cfg.ErrorBody, observeCreate: h.cfg.ObserveCreate}
	t.register(s)
	return s, nil
}

// Preset normalises a /mcp/{project} path segment exactly as the envelope's
// project member: it runs the write path on a body carrying only that
// member. "" means the segment normalises to nothing.
func Preset(segment string) string {
	q, err := json.Marshal(segment)
	if err != nil {
		return ""
	}
	body := append(append([]byte(`{"project":`), q...), '}')
	env, _, err := envelope.DecodeWithLimits(body, len(body), envelope.MaxDepth)
	if err != nil {
		return ""
	}
	return env.Project
}
