package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpSynopsis = "mcp [--local]"

// runMcp serves the MCP tools over stdio on the local database until stdin
// closes or SIGINT or SIGTERM arrives. Stdout carries the protocol and
// nothing else: every error goes to stderr through run, and the in-process
// handler logs to stderr.
func runMcp(args []string, _ io.Reader, _, stderr io.Writer) error {
	fs := newFlagSet("mcp")
	mf := addModeFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s: serve the MCP tools over stdio on the local database\n", mcpSynopsis)
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
	if m.Mode != modeLocal {
		return errMcpRemote(m.Settings.URL)
	}
	if _, err := openLocalClient(m, os.Getenv, stderr); err != nil {
		return err
	}
	env, err := sessionsEnv(os.Getenv)
	if err != nil {
		return err
	}
	srv, err := localTarget.MCPServer(&sessionsService{db: localTarget.DB(), env: env})
	if err != nil {
		return failErr("the MCP server cannot be built: "+oneLine(err.Error()), "report this as a bug")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return failErr("the MCP session ended: "+oneLine(err.Error()), "restart the MCP server from the harness")
	}

	return nil
}

// errMcpRemote refuses remote mode: the stdio server is the local
// database's. The URL is shown redacted, as doctor shows it, and without its
// query and fragment, which can hold a secret too.
func errMcpRemote(u resolvedValue) error {
	from := map[string]string{sourceFlag: "--server", sourceEnv: envURL, sourceConfig: "the config file"}[u.Source]
	shown := redactURL(u.Value)
	if p, err := url.Parse(shown); err == nil {
		p.RawQuery, p.ForceQuery, p.Fragment, p.RawFragment = "", false, "", ""
		shown = p.String()
	}

	return failErr(fmt.Sprintf("agentfeedback mcp serves the local database, and a server URL is configured (%s)", from),
		"point the harness at "+strings.TrimRight(shown, "/")+"/mcp with agentfeedback install <harness> --mcp, or pass --local")
}
