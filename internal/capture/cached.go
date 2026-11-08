package capture

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// CachedPolicy reads a matching observation without network access. It is for
// offline watermark display only, never permission to upload; Push refreshes.
func CachedPolicy(ctx context.Context, db *store.DB, c *client.Client, opts Options) (Policy, error) {
	if opts.Local || db == nil || c == nil {
		return Policy{State: NotApplicable}, nil
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(opts.Server), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Policy{}, errors.New("sessions: invalid server URL")
	}
	u.Host = strings.ToLower(u.Host)
	var p Policy
	err = db.Read(ctx, func(q store.Querier) error {
		r, found, err := store.CurrentCapturePolicy(ctx, q, u.String())
		if err != nil || !found || r.CredentialFingerprint != c.CredentialFingerprint() {
			return err
		}
		current, err := store.CaptureGenerationCurrent(ctx, q, r.Server, r.Generation)
		if err != nil || !current {
			return err
		}
		quiet := opts.Quiet
		if quiet <= 0 {
			quiet = defaultQuiet
		}
		p, err = fromStored(r, quietString(quiet))
		return err
	})
	return p, err
}
