"""Check identity vectors and their fixed-clock state transitions independently."""

from __future__ import annotations

import copy
import hashlib
import json
from datetime import datetime, timedelta
from pathlib import Path

from .canon import canonical_bytes
from .decode import decode
from .rawjson import Reader


def _time(text: str) -> datetime:
    value = datetime.fromisoformat(text.replace("Z", "+00:00"))
    if value.strftime("%Y-%m-%dT%H:%M:%S.%fZ") != text:
        raise ValueError(f"not a canonical UTC fixture clock: {text}")
    return value


def check(d: Path, schemas) -> list[str]:
    """Compare every create's normalization, canonical bytes, hash and replay state."""
    scenario = json.loads((d / "scenario.json").read_text(encoding="utf-8"))
    expected = json.loads((d / "expected.json").read_text(encoding="utf-8"))
    actions, results = scenario["actions"], expected["results"]
    if len(actions) != len(results):
        return ["actions/results length differs"]
    problems: list[str] = []
    rows: dict[int, dict] = {}
    uid_index = 0
    previous_time = None

    def compare_record(filename: str, record: dict, label: str) -> None:
        want = (d / filename).read_bytes()
        if canonical_bytes(record) != want:
            problems.append(f"{label}: {filename} differs from independent record state")
        # Requiring canonical fixture bytes also catches duplicate or reordered members.
        if canonical_bytes(Reader(want).parse()) != want:
            problems.append(f"{label}: {filename} is not canonical JSON")

    for index, (action, want) in enumerate(zip(actions, results), 1):
        label = f"action {index} ({action['op']})"
        now = _time(action["at"])
        if previous_time is not None and now < previous_time:
            problems.append(f"{label}: clock moved backwards")
        previous_time = now
        op = action["op"]
        status = 200
        details = []
        error = None
        if op == "create":
            decoded = decode((d / action["body"]).read_bytes(), schemas)
            if decoded.status != 201:
                problems.append(f"{label}: body rejected with {decoded.status}")
                continue
            stored = (d / want["stored"]).read_bytes()
            if canonical_bytes(decoded.envelope) != stored:
                problems.append(f"{label}: normalized envelope differs from {want['stored']}")
            canonical = (d / want["canonical"]).read_bytes()
            if canonical_bytes(decoded.hash_input) != canonical:
                problems.append(f"{label}: identity projection differs from {want['canonical']}")
            digest = hashlib.sha256(canonical).hexdigest()
            if want["sha256"] != digest or decoded.content_hash != digest:
                problems.append(f"{label}: expected/reference hash differs from authored canonical bytes")
            warning_keys = sorted((w["code"], w["pointer"]) for w in want["warnings"])
            if warning_keys != decoded.warning_keys():
                problems.append(f"{label}: warning code/pointer multiset differs: {decoded.warning_keys()}")
            env = decoded.envelope
            key = env.get("key")
            if key:
                existing = next((r for r in rows.values() if r["kind"] == env["kind"] and r.get("key") == key), None)
            else:
                eligible = [r for r in rows.values() if r["content_hash"] == decoded.content_hash
                            and "processed_at" not in r
                            and _time(r["created_at"]) >= now - timedelta(hours=24)]
                existing = max(eligible, key=lambda r: (r["created_at"], r["id"]), default=None)
            if existing is not None:
                record = existing
                if key and record["content_hash"] != decoded.content_hash:
                    status, error = 409, "replay_mismatch"
                    details = [{"code": "key_reused", "pointer": "/key", "existing_id": record["id"]}]
            else:
                status = 201
                record = copy.deepcopy(env)
                record.update(id=len(rows) + 1, uid=scenario["uids"][uid_index],
                              content_hash=decoded.content_hash, created_at=action["at"])
                uid_index += 1
                rows[record["id"]] = record
        elif op in ("mark", "redact"):
            record = rows[action["id"]]
            if op == "mark":
                # These identity scenarios deliberately use already-normalized processing marks.
                mark = action["body"]
                if mark.get("processed", True) is not True:
                    raise ValueError("identity fixtures only use processed=true marks")
                record.setdefault("processed_at", action["at"])
                record.update({k: v for k, v in mark.items() if k != "processed"})
            elif "redacted_at" not in record:
                record.pop("summary", None)
                record.pop("context", None)
                record.update(payload={"redacted": True}, redacted_at=action["at"])
        else:
            raise ValueError(f"unknown identity action {op!r}")
        if (status, record["id"], error, details) != (want["status"], want["id"], want.get("error"), want.get("details", [])):
            problems.append(f"{label}: status/id/error/details disagree with independent identity state")
        if status != 409:
            compare_record(want["record"], record, label)
    if len(rows) != expected["rows"]:
        problems.append("final row count differs")
    if uid_index != len(scenario["uids"]):
        problems.append("not every supplied UID was consumed")
    if len(expected["records"]) != len(rows):
        problems.append("final record snapshot count differs")
    seen = set()
    for filename in expected["records"]:
        snapshot = Reader((d / filename).read_bytes()).parse()
        id_ = int(snapshot["id"])
        if id_ in seen or id_ not in rows:
            problems.append(f"final snapshot {filename} has duplicate/unknown id {id_}")
            continue
        seen.add(id_)
        compare_record(filename, rows[id_], "final state")
    return problems
