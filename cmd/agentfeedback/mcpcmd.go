package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

const mcpSynopsis = "mcp [--local | --server URL]"

// The relay's limits: the dial limit is pkg/client's; the handshake and the
// tools/list each get relayConnectTimeout, a forwarded call mcp.CallTimeout.
const (
	relayConnectTimeout = 15 * time.Second
	relayDialLimit      = 2 * time.Second
)

// runMcp serves the MCP tools over stdio until stdin closes or SIGINT or
// SIGTERM arrives: on the local database in local mode, relayed to the
// configured server's /mcp in remote mode; the session tools and
// submit_feedback's context_from are answered on this machine either way.
// Stdout carries the protocol and nothing else: every error goes to stderr
// through run, and the in-process handler logs to stderr.
func runMcp(args []string, _ io.Reader, _, stderr io.Writer) error {
	fs := newFlagSet("mcp")
	mf := addModeFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s: serve the MCP tools over stdio: the local database, or the configured server with this machine's session tools\n", mcpSynopsis)
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("mcp", err)
	}
	if fs.NArg() != 0 {
		return errArgs("mcp", mcpSynopsis)
	}
	m, err := resolveMode(*mf, os.Getenv)
	if err != nil {
		return err
	}
	from := &contextFromService{mode: *mf, stderr: stderr}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var srv *sdk.Server
	if m.Mode != modeLocal {
		relay, closeRemote, err := buildRelay(ctx, m, *mf, from, stderr)
		if err != nil {
			return err
		}
		defer closeRemote()
		srv = relay
	} else {
		if _, err := openLocalClient(m, os.Getenv, stderr); err != nil {
			return err
		}
		env, err := sessionsEnv(os.Getenv)
		if err != nil {
			return err
		}
		if srv, err = localTarget.MCPServer(&sessionsService{db: localTarget.DB(), env: env}, from); err != nil {
			return failErr("the MCP server cannot be built: "+oneLine(err.Error()), "report this as a bug")
		}
	}
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return failErr("the MCP session ended: "+oneLine(err.Error()), "restart the MCP server from the harness")
	}

	return nil
}

// buildRelay connects to the configured server's /mcp with its key and
// builds the stdio server that relays to it. The URL and the key are
// checked as every remote-mode command checks them; a server that does not
// answer as an MCP endpoint fails here, before anything is served. The key
// is sent only to that endpoint, in the two auth headers pkg/client sends,
// and appears in no message: the SDK gets a logger that discards, and every
// message built from a remote error is redacted. closeRemote closes the
// session in use, which a reconnect may have replaced.
func buildRelay(ctx context.Context, m clientMode, mf modeFlags, from mcp.ContextFrom, stderr io.Writer) (srv *sdk.Server, closeRemote func(), err error) {
	if _, _, err := newAPIClient(os.Getenv, mf, stderr); err != nil {
		return nil, nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	key := m.Settings.APIKey.Value
	endpoint := strings.TrimRight(strings.TrimSpace(m.Settings.URL.Value), "/") + "/mcp"
	shown := redactURL(endpoint)
	redact := func(msg string) string {
		msg = oneLine(msg)
		if key != "" {
			msg = strings.ReplaceAll(msg, key, "[REDACTED]")
		}

		return msg
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{Timeout: relayDialLimit, KeepAlive: 30 * time.Second}).DialContext
	// No overall client timeout: each forwarded call has its own deadline
	// (mcp.CallTimeout), the handshake relayConnectTimeout.
	hc := &http.Client{
		Transport:     keyTransport{key: key, next: t},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	connect := func(ctx context.Context) (*sdk.ClientSession, error) {
		ctx, cancel := context.WithTimeout(ctx, relayConnectTimeout)
		defer cancel()
		client := sdk.NewClient(&sdk.Implementation{Name: "agentfeedback", Version: clientVersion().Version},
			&sdk.ClientOptions{Logger: mcp.DiscardLogger})

		return client.Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
	}
	remote, err := connect(ctx)
	if err != nil {
		return nil, nil, errMcpConnect(shown, redact(err.Error()))
	}
	var current atomic.Pointer[sdk.ClientSession]
	current.Store(remote)
	closeRemote = func() { _ = current.Load().Close() }
	sessions, err := newSessionsService(os.Getenv, stderr)
	if err != nil {
		closeRemote()

		return nil, nil, err
	}
	instructions, err := mcp.Instructions("", "")
	if err != nil {
		closeRemote()

		return nil, nil, failErr("the MCP server cannot be built: "+oneLine(err.Error()), "report this as a bug")
	}
	listCtx, cancel := context.WithTimeout(ctx, relayConnectTimeout)
	defer cancel()
	srv, err = mcp.NewRelay(listCtx, mcp.RelayConfig{
		Remote: remote,
		Reconnect: func(ctx context.Context) (*sdk.ClientSession, error) {
			cs, err := connect(ctx)
			if err == nil {
				current.Store(cs)
			}

			return cs, err
		},
		Sessions: sessions, ContextFrom: from,
		ErrorBody: api.ToolErrorBody, CallContext: api.WithNewRequestID,
		Redact: redact,
		Warn: func(msg string) {
			fmt.Fprintf(stderr, "agentfeedback mcp: warning: %s\n", redact(msg))
		},
		Instructions: instructions, Version: clientVersion().Version,
	})
	if err != nil {
		closeRemote()

		return nil, nil, errMcpConnect(shown, redact(err.Error()))
	}

	return srv, closeRemote, nil
}

// errMcpConnect is a relay that could not start: the endpoint is shown
// redacted, as doctor shows it.
func errMcpConnect(shown, msg string) error {
	return failErr(fmt.Sprintf("the MCP endpoint %s cannot be reached: %s", shown, msg),
		"check the server URL and the API key with agentfeedback doctor, or pass --local for the local database")
}

// keyTransport sets the API key on every request in both headers
// pkg/client sends.
type keyTransport struct {
	key  string
	next http.RoundTripper
}

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+k.key)
	r.Header.Set("X-Api-Key", k.key)

	return k.next.RoundTrip(r)
}

// contextFromService answers submit_feedback's context_from and ordinal:
// the other arguments are what submit <kind> --stdin --context-from would
// read from stdin, and the body is what it would send.
type contextFromService struct {
	mode   modeFlags
	stderr io.Writer
}

// Body builds the body submit --context-from builds. Every failure is a
// validation problem of context_from carrying the CLI's message.
func (c *contextFromService) Body(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	body, err := c.body(args)
	if err != nil {
		return nil, mcp.InvalidArgument(mcp.ArgContextFrom, oneLine(err.Error()))
	}

	return body, nil
}

func (c *contextFromService) body(args json.RawMessage) (json.RawMessage, error) {
	obj, err := parseObject(args)
	if err != nil {
		return nil, fmt.Errorf("the arguments are not one JSON object: %w", err)
	}
	rawFrom, hasFrom := obj.get(mcp.ArgContextFrom)
	rawOrdinal, hasOrdinal := obj.get(mcp.ArgOrdinal)
	obj = obj.remove(mcp.ArgContextFrom).remove(mcp.ArgOrdinal)
	if !hasFrom {
		return nil, errors.New("ordinal applies only with context_from; add context_from <harness>:<session id>#<span>, or drop it")
	}
	var value string
	if json.Unmarshal(rawFrom, &value) != nil {
		return nil, errors.New("context_from must be a string: <harness>:<session id>#<span>")
	}
	ordinal := 1
	if hasOrdinal {
		if json.Unmarshal(rawOrdinal, &ordinal) != nil {
			return nil, errors.New("ordinal must be an integer from 1")
		}
	}
	var kind string
	if raw, ok := obj.get("kind"); ok {
		_ = json.Unmarshal(raw, &kind)
	}
	if schema.Token(kind) == "" {
		return nil, errors.New(`kind is required with context_from, such as "friction"`)
	}
	from, err := locateContext(value, ordinal, false, nil)
	if err != nil {
		return nil, err
	}
	s, err := loadSubmitter("mcp", false, c.mode, io.Discard, c.stderr)
	if err != nil {
		return nil, err
	}
	if err := from.resolve(s); err != nil {
		return nil, err
	}
	if _, off := s.disabled(); off {
		return nil, fmt.Errorf("the [collect] rules exclude the session's working directory (%s)", s.decision.Reason)
	}
	if schema.Token(kind) == "friction" && !obj.has("summary") {
		return nil, errors.New("a friction needs a summary")
	}
	out, _, err := buildEnvelope(s, envelopeInput{kind: kind, str: map[string]*string{}, given: map[string]bool{}, obj: obj, from: from})
	if err != nil {
		return nil, err
	}

	return out.encode(), nil
}
