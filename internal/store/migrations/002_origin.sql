-- context.origin lifted into an indexable column: the string the submitter
-- sent (agent, hook-nudge, session-scan, inbox, import, ...), stored as
-- sent. Rows without a context or without an origin are NULL and never
-- match an origin filter or group.
ALTER TABLE submissions ADD COLUMN origin TEXT GENERATED ALWAYS AS (json_extract(context, '$.origin')) VIRTUAL;

CREATE INDEX ix_submissions_origin ON submissions(origin) WHERE origin IS NOT NULL;
