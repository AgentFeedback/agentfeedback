"""Seed only the live gate's isolated policy cache; never contact a server."""
import hashlib
import json
import os
import sqlite3
import sys
import time
from pathlib import Path

cache = Path(os.environ["XDG_CACHE_HOME"]) / "agentfeedback"
cache.mkdir(parents=True, exist_ok=True)
path = Path(os.environ["XDG_DATA_HOME"]) / "agentfeedback" / "agentfeedback.db"
now = time.time_ns() // 1000
with sqlite3.connect(path) as db:
    db.execute("DELETE FROM capture_policy")
    db.execute("DELETE FROM capture_refresh")
    db.execute("INSERT INTO capture_refresh VALUES (?, 1)", (os.environ["SWEEP_URL"],))
    db.execute("""INSERT INTO capture_policy
        (server, org_id, org_name, enabled, notice_version, retention_json,
         bounds_json, since, checked_at, credential_fingerprint, generation, state, is_current)
        VALUES (?, ?, 'Live fixture', 1, '1', '{}', ?, ?, ?, ?, 1, 'enabled', 1)""",
        (os.environ["SWEEP_URL"], "11111111-1111-4111-8111-111111111111",
         json.dumps({"max_part_bytes": 10485760, "max_session_bytes": 25165824,
                     "incomplete_upload_ttl_seconds": 86400}),
         now - 3600_000_000, now,
         hashlib.sha256(os.environ["AGENT_FEEDBACK_API_KEY"].encode()).hexdigest()))
if "--keep-markers" not in sys.argv:
    for name in ("push.last-started", "push.last-run.json"):
        (cache / name).unlink(missing_ok=True)
