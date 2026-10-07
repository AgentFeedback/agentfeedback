package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/ui"
)

const uiSynopsis = "ui [--addr ADDR] [--local | --server URL]"

// runUI serves the read-only page until SIGINT or SIGTERM.
func runUI(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serveUI(ctx, args, stdout, stderr)
}

// serveUI parses args, listens on the loopback address, prints the page's
// URL with its launch token as one stdout line and serves until ctx ends.
func serveUI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("ui")
	mf := addModeFlags(fs)
	addr := fs.String("addr", "127.0.0.1:0", "loopback address to listen on: a loopback IP or localhost, with a port (0 picks one)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s: serve a read-only web page over the queue on loopback until Ctrl-C\n", uiSynopsis)
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("ui", err)
	}
	if fs.NArg() != 0 {
		return errArgs("ui", uiSynopsis)
	}
	listen, err := uiListenAddr(*addr)
	if err != nil {
		return err
	}
	c, m, err := newAPIClient(os.Getenv, *mf, stderr)
	if err != nil {
		return err
	}
	mode := modeLocal
	if m.Mode == modeRemote {
		mode = "server " + uiServerHost(m.Settings.URL.Value)
	}
	token, err := ui.NewToken()
	if err != nil {
		return failErr("no launch token can be generated: "+oneLine(err.Error()), "retry")
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return failErr(fmt.Sprintf("agentfeedback ui cannot listen on %s: %s", listen, oneLine(err.Error())), "pick another --addr port")
	}
	bound := ln.Addr().String()
	h, err := ui.New(ui.Config{Source: c, Token: token, Addr: bound, Mode: mode, Now: nowFunc})
	if err != nil {
		_ = ln.Close()

		return failErr("the page cannot be built: "+oneLine(err.Error()), "report this as a bug")
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	fmt.Fprintf(stdout, "agentfeedback ui: http://%s/%s/\n", bound, token)
	fmt.Fprintln(stderr, "agentfeedback ui: read-only; press Ctrl-C to stop")

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return failErr("the page server stopped: "+oneLine(err.Error()), "start agentfeedback ui again")
		}

		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	return nil
}

// uiListenAddr checks --addr: the host must be a loopback IP literal or
// localhost, which listens on 127.0.0.1.
func uiListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", usageErr(fmt.Sprintf("--addr %q is not host:port", addr), "pass --addr 127.0.0.1:PORT (0 picks a port)")
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", usageErr(fmt.Sprintf("--addr %q is not a loopback address; agentfeedback ui listens only on a loopback IP or localhost", addr),
			"pass --addr 127.0.0.1:PORT (0 picks a port)")
	}

	return net.JoinHostPort(host, port), nil
}

// uiServerHost is the server URL's host alone: no credentials, path or
// query reach the page.
func uiServerHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable URL)"
	}

	return u.Host
}
