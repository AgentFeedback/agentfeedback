-- OpenCode V1's public identity changes; its physical stores do not.
-- Merge processed progress independently of the latest digest. For the same
-- transcript head keep the furthest watermark; for a rotated transcript keep
-- the most recently processed generation. Keep each watermark's metadata whole.
UPDATE sessions_seen AS current SET
  (processed_at, processed_size, processed_mtime, processed_head, outcome, refs) =
  (SELECT old.processed_at, old.processed_size, old.processed_mtime,
          old.processed_head, old.outcome, old.refs
   FROM sessions_seen AS old
   WHERE old.harness = 'opencode' AND old.session_id = current.session_id)
WHERE current.harness = 'opencode-v1' AND EXISTS (
  SELECT 1 FROM sessions_seen AS old
  WHERE old.harness = 'opencode' AND old.session_id = current.session_id
    AND old.processed_at IS NOT NULL
    AND (current.processed_at IS NULL
      OR (old.processed_head = current.processed_head
          AND (old.processed_size > current.processed_size
            OR (old.processed_size = current.processed_size AND old.processed_at > current.processed_at)))
      OR (old.processed_head IS NOT current.processed_head
          AND old.processed_at > current.processed_at))
);

INSERT INTO sessions_seen
  (harness, session_id, path, mtime, size, head, digested_at,
   processed_at, processed_size, processed_mtime, processed_head, outcome, refs)
SELECT 'opencode-v1', session_id, path, mtime, size, head, digested_at,
       processed_at, processed_size, processed_mtime, processed_head, outcome, refs
FROM sessions_seen WHERE harness = 'opencode'
ON CONFLICT (harness, session_id) DO UPDATE SET
  (path, mtime, size, head, digested_at) =
  (excluded.path, excluded.mtime, excluded.size, excluded.head, excluded.digested_at)
WHERE excluded.digested_at > sessions_seen.digested_at
   OR sessions_seen.digested_at IS NULL;

DELETE FROM sessions_seen WHERE harness = 'opencode';
