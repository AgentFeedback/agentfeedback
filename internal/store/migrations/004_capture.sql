-- Capture policy observations are scoped to a server and organisation. enabled
-- records the last successful consent response independently of transient state;
-- since is its consent epoch. All timestamps are unix microseconds UTC.
CREATE TABLE capture_policy (
  server              TEXT    NOT NULL,
  org_id              TEXT    NOT NULL,
  org_name            TEXT    NOT NULL DEFAULT '',
  enabled             INTEGER NOT NULL CHECK (enabled IN (0, 1)),
  notice_version      TEXT    NOT NULL DEFAULT '',
  last_notice_version TEXT    NOT NULL DEFAULT '',
  retention_json      TEXT    NOT NULL CHECK (json_valid(retention_json)),
  bounds_json         TEXT    NOT NULL CHECK (json_valid(bounds_json)),
  since               INTEGER,
  checked_at          INTEGER NOT NULL,
  retry_at            INTEGER,
  credential_fingerprint TEXT NOT NULL DEFAULT '',
  generation          INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  state               TEXT    NOT NULL CHECK (state IN
    ('enabled', 'disabled', 'unsupported', 'unauthorized', 'suspended', 'error')),
  is_current          INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  PRIMARY KEY (server, org_id)
);

-- A server-only lookup must remain unambiguous when organisations switch at the
-- same timestamp. Historical rows retain their independent consent epochs.
CREATE UNIQUE INDEX capture_policy_current ON capture_policy (server) WHERE is_current = 1;

-- Allocate before network discovery; only this server's newest request may
-- commit an observation, including when independent CLI processes overlap.
CREATE TABLE capture_refresh (
  server     TEXT    PRIMARY KEY,
  generation INTEGER NOT NULL CHECK (generation > 0)
);
