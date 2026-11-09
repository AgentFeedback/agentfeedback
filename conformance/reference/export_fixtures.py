"""Independently check fixed export state, filters, byte framing and digests."""

from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path

from .canon import canonical_bytes
from .decode import decode
from .text import token

# Public full-record field order; payload/context retain canonical number tokens.
_RECORD_MEMBERS = (
    "id", "uid", "kind", "schema_version", "key", "summary", "machine", "model",
    "harness", "project", "occurred_at", "context", "payload", "content_hash",
    "created_at", "processed_at", "redacted_at", "verdict", "resolution", "ref",
    "processed_by",
)


def _time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(timezone.utc)


def _timestamp(value: str) -> str:
    return _time(value).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _ordered(value: dict, members) -> bytes:
    return b"{" + b",".join(
        canonical_bytes(name) + b":" + canonical_bytes(value[name])
        for name in members if name in value
    ) + b"}"


def _check(d: Path, schemas) -> list[str]:
    scenario = json.loads((d / "scenario.json").read_text(encoding="utf-8"))
    expected = json.loads((d / "expected.json").read_text(encoding="utf-8"))
    stream = (d / "expected.ndjson").read_bytes()
    problems = []
    records = []
    for index, seed in enumerate(scenario["seed"], 1):
        result = decode(seed["body"].encode("utf-8"), schemas)
        if result.status != 201:
            problems.append(f"seed {index}: body rejected with {result.status}")
            continue
        # Recompute identity, not the hash of the post-redaction payload.
        content_hash = hashlib.sha256(canonical_bytes(result.hash_input)).hexdigest()
        records.append(dict(result.envelope, id=index, uid=seed["uid"],
                            content_hash=content_hash, created_at=_timestamp(seed["at"])))
    if problems:
        return problems
    for mutation in scenario["mutations"]:
        record = records[mutation["id"] - 1]
        if mutation["op"] == "mark":
            mark = json.loads(mutation["body"])
            if mark.get("processed", True) is not True:
                raise ValueError("export fixture marks must set processed=true")
            record["processed_at"] = _timestamp(mutation["at"])
            for name in ("verdict", "resolution", "ref", "processed_by"):
                if name in mark:
                    record[name] = mark[name]
        elif mutation["op"] == "redact":
            record["payload"] = {"redacted": True}
            record.pop("summary", None)
            record.pop("context", None)
            record.setdefault("redacted_at", _timestamp(mutation["at"]))
        else:
            raise ValueError(f"unknown mutation {mutation['op']!r}")

    params = scenario["params"]
    invalid_pointer = None
    if params.get("after_id", 0) < 0:
        invalid_pointer = "?after_id"
    elif "limit" in params and not 1 <= params["limit"] <= 500:
        invalid_pointer = "?limit"
    if invalid_pointer is not None:
        want = {"status": 400, "error": "validation_error", "details": [
            {"code": "out_of_range", "pointer": invalid_pointer}]}
        if expected != want:
            problems.append(f"invalid-parameter expectation differs: expected {want!r}")
        if stream:
            problems.append("invalid export must write no bytes")
        return problems

    kind = token(params.get("kind", ""))
    since = _time(params["since"]) if "since" in params else None
    selected = [r for r in records
                if (not kind or r["kind"] == kind)
                and (since is None or _time(r["created_at"]) >= since)
                and r["id"] > params.get("after_id", 0)]
    selected.sort(key=lambda r: r["id"])
    if "limit" in params:
        selected = selected[:params["limit"]]
    ids = [r["id"] for r in selected]
    record_bytes = b"".join(_ordered(r, _RECORD_MEMBERS) + b"\n" for r in selected)
    digest = hashlib.sha256(record_bytes).hexdigest()
    want = {"status": 200, "ids": ids, "sha256": digest}
    if expected != want:
        problems.append(f"filter/digest expectation differs: expected {want!r}")
    header = {"export_format": 2, "kind": kind or None,
              "since": _timestamp(params["since"]) if since is not None else None,
              "after_id": params.get("after_id"),
              "exported_at": _timestamp(scenario["exported_at"])}
    trailer = {"export_complete": True, "count": len(selected),
               "first_id": ids[0] if ids else None,
               "last_id": ids[-1] if ids else None, "sha256": digest}
    computed = (_ordered(header, header) + b"\n" + record_bytes
                + _ordered(trailer, trailer) + b"\n")
    if stream != computed:
        problems.append("expected.ndjson differs from independent full-record/framing bytes")

    # Check the authored stream's digest directly as well, never including its
    # header or trailer; LF is part of every record's digest input.
    lines = stream.split(b"\n")
    if not lines or lines[-1] != b"" or len(lines) < 3:
        problems.append("expected.ndjson lacks LF-terminated header/trailer framing")
    else:
        actual_records = b"".join(line + b"\n" for line in lines[1:-2])
        actual_trailer = json.loads(lines[-2])
        if actual_trailer.get("sha256") != hashlib.sha256(actual_records).hexdigest():
            problems.append("trailer sha256 differs from authored record lines plus LF")
        if actual_trailer.get("count") != len(lines) - 3:
            problems.append("trailer count differs from authored record-line count")
    return problems


def check(d: Path, schemas) -> list[str]:
    """Return fixture disagreements; malformed fixture files are errors too."""
    try:
        return _check(d, schemas)
    except (OSError, ValueError, TypeError, KeyError, IndexError) as exc:
        return [f"invalid export fixture: {exc}"]
