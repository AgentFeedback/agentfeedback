package capture

import (
	"errors"
	"math/big"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

func captureRevision(c sessions.CaptureCandidate) string {
	if c.Revision != "" {
		return c.Revision
	}
	return formatRevision(c.Size)
}

// resolveRevision uses only the already loaded rows in the current push scope.
func resolveRevision(c *sessions.CaptureCandidate, rows []store.SessionPush) error {
	if c.Harness != "omp" && c.Harness != "pi" {
		return nil
	}
	latest := -1
	maximum := new(big.Int)
	for i, row := range rows {
		if row.Harness != c.Harness || row.SessionID != c.SessionID {
			continue
		}
		n, ok := new(big.Int).SetString(row.Revision, 10)
		if !ok || n.Sign() < 0 {
			return errors.New("sessions: invalid persisted revision")
		}
		if latest == -1 || n.Cmp(maximum) > 0 {
			maximum.Set(n)
			latest = i
		}
	}
	if latest >= 0 && rows[latest].Head == c.Head && rows[latest].SourceSize == c.Size {
		c.Revision = rows[latest].Revision
	} else {
		c.Revision = maximum.Add(maximum, big.NewInt(1)).String()
	}
	return nil
}

func (r *pushRun) reserveRevision(c sessions.CaptureCandidate) error {
	if r.opts.DryRun {
		return nil
	}
	for _, row := range r.rows {
		if row.Harness == c.Harness && row.SessionID == c.SessionID && row.Revision == c.Revision {
			return nil
		}
	}
	row := store.SessionPush{
		PushScope: policyScope(r.policy), Harness: c.Harness, SessionID: c.SessionID,
		Revision: c.Revision, Head: c.Head, SourceSize: c.Size,
		ObservedAt: r.clock().UnixMicro(), Status: "reserved",
	}
	if err := r.db.Write(r.ctx, func(q store.Querier) error {
		return store.SetSessionPush(r.ctx, q, row)
	}); err != nil {
		return err
	}
	r.rows = append(r.rows, row)
	return nil
}
