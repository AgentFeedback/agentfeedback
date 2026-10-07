package main

import (
	"errors"
	"io"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// schemaTooNewReason prefixes the client-log reason of a database written by
// a newer binary, so the line is findable without parsing the message.
const schemaTooNewReason = "schema_too_new: "

// logSchemaTooNew appends an error line to the client log under cache when
// err says the database schema is newer than this binary, and reports
// whether it did. A hook that exits 0 and prints nothing leaves this line as
// the only trace of the version skew.
func logSchemaTooNew(cache string, err error) bool {
	if !errors.Is(err, store.ErrSchemaTooNew) {
		return false
	}
	client.LogTo(cache, client.Outcome{Outcome: client.OutcomeError, Reason: schemaTooNewReason + oneLine(err.Error())}, nowFunc(), io.Discard)

	return true
}
