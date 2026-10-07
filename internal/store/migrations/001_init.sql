-- The v1 schema: one table for every kind. This is the first migration; the
-- previous major version's database is not migrated, so a database this
-- binary did not create is refused at open (the application_id stamp is
-- written by store.Open before this file runs).
--
-- ids are global and never reused: AUTOINCREMENT keeps sqlite_sequence
-- monotonic after deletes, so an exported id always names the same record.
-- uid is the UUIDv7 that survives an export and import between instances.
-- Timestamps are unix microseconds UTC. payload and context are JSON text
-- stored as given and returned byte-exact. category and fix_status are the
-- friction payload's fields lifted into indexable columns; other kinds
-- never match them.
CREATE TABLE submissions (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  uid            TEXT    NOT NULL UNIQUE,
  kind           TEXT    NOT NULL,
  schema_version INTEGER NOT NULL DEFAULT 1,
  key            TEXT,
  summary        TEXT,
  machine        TEXT,
  model          TEXT,
  harness        TEXT,
  project        TEXT,
  occurred_at    INTEGER,
  context        TEXT    CHECK (context IS NULL OR json_valid(context)),
  payload        TEXT    NOT NULL CHECK (json_valid(payload)),
  content_hash   TEXT    NOT NULL,
  created_at     INTEGER NOT NULL,
  processed_at   INTEGER,
  verdict        TEXT,
  resolution     TEXT,
  ref            TEXT,
  processed_by   TEXT,
  redacted_at    INTEGER,
  category       TEXT GENERATED ALWAYS AS (CASE WHEN kind = 'friction' THEN json_extract(payload, '$.category') END) VIRTUAL,
  fix_status     TEXT GENERATED ALWAYS AS (CASE WHEN kind = 'friction' THEN json_extract(payload, '$.fix_status') END) VIRTUAL
);

-- Keyed replays: one row per (kind, key); keyless rows are outside the constraint.
CREATE UNIQUE INDEX ux_submissions_key     ON submissions(kind, key) WHERE key IS NOT NULL;
-- Keyless dedupe: same hash inside the window, newest first.
CREATE INDEX ix_submissions_hash_created   ON submissions(content_hash, created_at);
-- The queue: open rows by id.
CREATE INDEX ix_submissions_open           ON submissions(id) WHERE processed_at IS NULL;
CREATE INDEX ix_submissions_kind_created   ON submissions(kind, created_at);
CREATE INDEX ix_submissions_project        ON submissions(project);
CREATE INDEX ix_submissions_machine        ON submissions(machine);
CREATE INDEX ix_submissions_occurred       ON submissions(occurred_at);
CREATE INDEX ix_submissions_verdict        ON submissions(verdict);
CREATE INDEX ix_submissions_category       ON submissions(category) WHERE category IS NOT NULL;
CREATE INDEX ix_submissions_fix_status     ON submissions(fix_status) WHERE fix_status IS NOT NULL;
