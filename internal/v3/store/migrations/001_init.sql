-- One table for every family. ids are global and never reused: the AUTOINCREMENT
-- keyword keeps sqlite_sequence monotonic even after deletes, so an exported id
-- always refers to the same record.
CREATE TABLE submissions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  family TEXT NOT NULL CHECK (family IN ('review','friction','event')),
  submission_type TEXT NOT NULL,
  machine_name TEXT NOT NULL,
  coordinator_model TEXT NOT NULL,
  run_id TEXT,
  payload TEXT NOT NULL CHECK (json_valid(payload)),
  payload_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,   -- unix microseconds UTC
  processed_at INTEGER,
  resolution TEXT
);

-- Idempotency key for reviews and events. Frictions carry no run_id and are
-- excluded from the constraint by the partial index.
CREATE UNIQUE INDEX ux_submissions_key ON submissions(family, submission_type, run_id) WHERE run_id IS NOT NULL;
CREATE INDEX ix_submissions_created ON submissions(created_at);
CREATE INDEX ix_submissions_type ON submissions(submission_type);
CREATE INDEX ix_submissions_machine ON submissions(machine_name);
CREATE INDEX ix_submissions_hash ON submissions(payload_hash);
CREATE INDEX ix_submissions_open ON submissions(id) WHERE processed_at IS NULL;
