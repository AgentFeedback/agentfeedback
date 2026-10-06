// Package client is the v1 client transport: it sends one submission to
// <url>/api/v1/submissions with both auth headers (or through an in-process
// Transport over the local database), never follows a redirect, classifies
// the answer by the retry table in retry.go, keeps what it cannot deliver in
// the spool (data/spool, with data/rejected beside it; every entry records
// the destination it is bound to), flushes the spool with retention, and
// appends one line per outcome to the owner-only client log
// (cache/log/client.jsonl).
//
// Nothing here writes to stdout: the caller prints the Outcome line last.
// Payloads pass through as raw bytes; they are never decoded into
// map[string]any and re-encoded.
package client
