package capture

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// CachedSweepPolicy reads only the existing destination/credential-matched
// observation. It never creates or migrates a database, changes a policy, or
// contacts the server. The caller supplies a short context deadline; SQLite's
// busy timeout is zero so a hook never waits for the normal writer timeout.
// Retained enabled consent may trigger recovery after an error or elapsed retry,
// but permits launching a child only: Push independently refreshes permission.
func CachedSweepPolicy(ctx context.Context, path string, c *client.Client, opts Options) (Policy, error) {
	if opts.Local || c == nil {
		return Policy{State: NotApplicable}, nil
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(opts.Server), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Policy{}, errors.New("sessions: invalid server URL")
	}
	u.Host = strings.ToLower(u.Host)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	if !info.Mode().IsRegular() {
		return Policy{}, errors.New("sessions: invalid policy database")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Policy{}, err
	}
	query := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(0)"}}
	db, err := sql.Open("sqlite", store.FileURI(path, query))
	if err != nil {
		return Policy{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Policy{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Capture tables first appeared in migration 004. Only an intact older
	// schema is an expected no-op; missing current tables remain an error.
	var hasVersion, hasPolicy, hasRefresh bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'schema_version'),
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'capture_policy'),
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'capture_refresh')`).
		Scan(&hasVersion, &hasPolicy, &hasRefresh); err != nil {
		return Policy{}, err
	}
	if !hasVersion {
		return Policy{}, errors.New("sessions: invalid policy database schema")
	}
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version); err != nil {
		return Policy{}, err
	}
	if version >= 0 && version < 4 && !hasPolicy && !hasRefresh {
		return Policy{}, nil
	}
	if version < 4 || !hasPolicy || !hasRefresh {
		return Policy{}, errors.New("sessions: incomplete capture policy database schema")
	}
	row, found, err := store.CurrentCapturePolicy(ctx, tx, u.String())
	if err != nil || !found || row.CredentialFingerprint != c.CredentialFingerprint() {
		return Policy{}, err
	}
	current, err := store.CaptureGenerationCurrent(ctx, tx, row.Server, row.Generation)
	if err != nil || !current {
		return Policy{}, err
	}
	if row.Enabled {
		switch row.State {
		case Error:
			row.State = Enabled
		case Suspended:
			now := opts.Now
			if now.IsZero() {
				if opts.Clock != nil {
					now = opts.Clock()
				} else {
					now = time.Now()
				}
			}
			if row.RetryAt != nil && !now.Before(time.UnixMicro(*row.RetryAt)) {
				row.State = Enabled
			}
		}
	}
	quiet := opts.Quiet
	if quiet <= 0 {
		quiet = defaultQuiet
	}
	return fromStored(row, quietString(quiet))
}
