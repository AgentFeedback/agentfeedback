-- Only scrubbed upload metadata is retained; transcript bytes never enter SQLite.
CREATE TABLE session_manifests (
    server TEXT NOT NULL,
    org_id TEXT NOT NULL,
    credential_fingerprint TEXT NOT NULL,
    consent_since INTEGER NOT NULL,
    harness TEXT NOT NULL,
    session_id TEXT NOT NULL,
    revision TEXT NOT NULL,
    manifest BLOB NOT NULL,
    part_bytes INTEGER NOT NULL,
    PRIMARY KEY (server, org_id, credential_fingerprint, consent_since, harness, session_id, revision)
);
