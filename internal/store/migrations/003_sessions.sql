-- Session scanning state. sessions_seen holds one row per harness session
-- that was digested: the digest watermark (path, mtime, size, head,
-- digested_at) is written on every digest, and the processed_* columns copy
-- it when the session is marked, so a later digest can tell an unchanged
-- file from an appended, truncated or rotated one. mtime is unix
-- nanoseconds of the file; size counts the bytes of complete lines; head is
-- the SHA-256 hex of the first complete line. digested_at and processed_at
-- are unix microseconds UTC, like every other timestamp. outcome is filed,
-- nothing or skipped; refs is a JSON array of submission uids.
--
-- triage_state is a small key/value table for the processor's settings
-- (the session selection among them).
CREATE TABLE sessions_seen (
  harness         TEXT    NOT NULL,
  session_id      TEXT    NOT NULL,
  path            TEXT    NOT NULL,
  mtime           INTEGER NOT NULL,
  size            INTEGER NOT NULL,
  head            TEXT    NOT NULL,
  digested_at     INTEGER,
  processed_at    INTEGER,
  processed_size  INTEGER,
  processed_mtime INTEGER,
  processed_head  TEXT,
  outcome         TEXT,
  refs            TEXT    CHECK (refs IS NULL OR json_valid(refs)),
  PRIMARY KEY (harness, session_id)
);

CREATE TABLE triage_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
