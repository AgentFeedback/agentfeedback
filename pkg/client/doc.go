// Package client is the v1 client transport: it sends one submission to
// <url>/api/v1/submissions with both auth headers, never follows a redirect,
// classifies the answer by the retry table in retry.go, keeps what it cannot
// deliver in the spool (cache/spool, with cache/rejected beside it), flushes
// the spool with retention, and appends one line per outcome to the
// owner-only client log (cache/log/client.jsonl).
//
// Nothing here writes to stdout: the caller prints the Outcome line last.
// Payloads pass through as raw bytes; they are never decoded into
// map[string]any and re-encoded.
package client
