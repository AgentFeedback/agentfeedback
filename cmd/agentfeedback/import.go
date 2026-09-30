package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agentfeedback/agentfeedback/internal/api"
	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/store"
)

// importOutput is the one JSON line import prints: the restore result and
// whether it was a dry run.
type importOutput struct {
	core.ImportResult
	DryRun bool `json:"dry_run"`
}

// runImport restores an export (format 2) into the database, keeping ids.
// The whole file is verified before anything is written; a conflict is
// reported, never a failure.
func runImport(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("import")
	dryRun := fs.Bool("dry-run", false, "verify and count without writing")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("import", err)
	}
	if fs.NArg() != 1 {
		return errArgs("import", "import [--dry-run] <export.ndjson>")
	}
	path := fs.Arg(0)

	cfg, err := loadConfig(false)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	body, err := os.ReadFile(path)
	if err != nil {
		return errImportRead(path, err)
	}

	if err := core.VerifyExport(body); err != nil {
		return restoreErr(path, err)
	}

	dbPath := cfg.DatabasePath
	if *dryRun {
		// A dry run never creates DATABASE_PATH: against a missing database
		// it counts a restore into a fresh one in a scratch directory.
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			tmp, err := os.MkdirTemp("", "agentfeedback-import-")
			if err != nil {
				return fmt.Errorf("dry run scratch directory: %w", err)
			}
			defer func() { _ = os.RemoveAll(tmp) }()
			dbPath = filepath.Join(tmp, "dry-run.db")
		}
	}

	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	svc := core.New(db, core.Config{Version: cfg.ServiceVersion, Features: api.Features})
	res, err := svc.Restore(ctx, body, *dryRun)
	if err != nil {
		return restoreErr(path, err)
	}

	return writeJSON(stdout, importOutput{ImportResult: res, DryRun: *dryRun})
}

// restoreErr keeps "rejected" for a failed verification (400, 413); any other
// problem is a failed restore, which also wrote nothing.
func restoreErr(path string, err error) error {
	var p *core.Problem
	if errors.As(err, &p) {
		if p.Status == 400 || p.Status == 413 {
			return errImportRejected(path, p.Message)
		}

		return errImportFailed(path, p.Message)
	}

	return fmt.Errorf("restore %s: %w", path, err)
}
