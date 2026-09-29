// Command agentfeedback serves the AgentFeedback API and provides the offline
// maintenance subcommands (import, backup) that operate on the same database.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/api"
	"github.com/agentfeedback/agentfeedback/internal/v3/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// defaultServiceVersion is overridden by SERVICE_VERSION.
const defaultServiceVersion = "v3.0.0"

const usage = `usage:
  agentfeedback [serve]              serve the HTTP API (default)
  agentfeedback import <file.jsonl>  import an export stream into the database
  agentfeedback backup <dest.db>     write a consistent copy of the database

environment: API_KEY (serve only), DATABASE_PATH, HTTP_LISTEN_ADDR,
GRACEFUL_SHUTDOWN_TIMEOUT, SERVICE_VERSION, LOG_LEVEL
`

func main() {
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		command = args[0]
		args = args[1:]
	}

	var err error
	switch command {
	case "serve":
		err = runServe()
	case "import":
		err = runImport(args)
	case "backup":
		err = runBackup(args)
	case "help", "-h", "--help":
		fmt.Print(usage)

		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "agentfeedback %s: %v\n", command, err)
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func setupLogger(level string) {
	lvl := slog.LevelInfo
	if level == "debug" {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}

func runServe() error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	svc := core.New(db)

	var shuttingDown atomic.Bool
	srv := api.New(api.Config{
		Service:      svc,
		APIKey:       cfg.APIKey,
		ShuttingDown: &shuttingDown,
	})

	httpSrv := &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	slog.Info("service is up",
		"version", cfg.ServiceVersion, "addr", cfg.HTTPListenAddr, "database", cfg.DatabasePath)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server failed: %w", err)
		}

		return nil
	case sig := <-stop:
		slog.Info("received signal, shutting down", "signal", sig.String())
	}

	// /ready starts failing immediately: a load balancer stops sending new
	// requests while the in-flight ones drain.
	shuttingDown.Store(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server did not shut down gracefully", "error", err, "timeout", cfg.ShutdownTimeout)
	}
	slog.Info("service shut down")

	return nil
}

func runBackup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agentfeedback backup <dest.db>")
	}
	dest := args[0]

	cfg, err := loadConfig(false)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	// Refuse rather than overwrite: a backup command that clobbers an existing
	// backup is a data-loss command.
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination %s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check destination %s: %w", dest, err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.VacuumInto(ctx, dest); err != nil {
		return err
	}
	fmt.Printf("backed up %s to %s\n", cfg.DatabasePath, dest)

	return nil
}
