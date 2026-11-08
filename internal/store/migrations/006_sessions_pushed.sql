-- Push state is independent of digest state and contains no transcript bytes.
CREATE TABLE sessions_pushed (
    server TEXT NOT NULL,
    org_id TEXT NOT NULL,
    credential_fingerprint TEXT NOT NULL,
    consent_since INTEGER NOT NULL,
    harness TEXT NOT NULL,
    session_id TEXT NOT NULL,
    revision TEXT NOT NULL,
    head TEXT NOT NULL,
    source_size INTEGER NOT NULL,
    content_sha256 TEXT NOT NULL DEFAULT '',
    envelope_bytes INTEGER NOT NULL DEFAULT 0,
    max_session_bytes INTEGER NOT NULL,
    pushed_at INTEGER,
    observed_at INTEGER NOT NULL,
    submission_uid TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('pushed', 'replayed', 'too_large', 'replay_mismatch')),
    reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (server, org_id, credential_fingerprint, consent_since, harness, session_id, revision)
);
CREATE INDEX sessions_pushed_scope ON sessions_pushed
    (server, org_id, credential_fingerprint, consent_since, observed_at);
