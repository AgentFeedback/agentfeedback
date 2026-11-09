"""Stateless format-2 checks; state transitions belong to the Go consumer.

Framing, record validation, canonical identity, hashes, normalisation and
pre-deduplication warnings are computed here, including create-probe envelopes
and hashes. No counts, UID/key lookups, assigned IDs or transactions are simulated.
"""
from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path

from .canon import canonical_bytes
from .decode import ENVELOPE_MEMBERS, _parse_rfc3339, decode
from .rawjson import NotJSON, RawNumber, escape_token, parse

IMPORT_LIMIT = 33554432
IMPORT_DEPTH = 514  # Create's 512 levels plus two levels from payload inference.
# readObject decodes each raw member separately with encoding/json's 10,000
# level limit; its already-tokenized outer object adds one more level.
_OBJECT_DEPTH = 10_001
_REQUIRED = ("id", "uid", "kind", "schema_version", "payload", "content_hash", "created_at")
_MEMBERS = set(ENVELOPE_MEMBERS) | {"id", "uid", "content_hash", "created_at", "processed_at", "redacted_at", "verdict", "resolution", "ref", "processed_by"}
_UUID = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\Z")


class Invalid(ValueError):
    def __init__(self, line, code, pointer, status=400):
        self.value = {"status": status, "error": "request_too_large" if status == 413 else "validation_error"}
        if status != 413:
            self.value.update(line=line, code=code, pointer=pointer)


def _object(raw, line, record=False):
    try:
        value, warnings = parse(raw, max_depth=_OBJECT_DEPTH)
    except NotJSON:
        raise Invalid(line, "invalid_format", "") from None
    if not isinstance(value, dict):
        raise Invalid(line, "invalid_format", "")
    for warning in warnings:
        # readObject rejects duplicate *record members*, not nested payload keys.
        if warning.code == "duplicate_key" and warning.pointer.count("/") == 1:
            raise Invalid(line, "duplicate_key" if record else "invalid_format", warning.pointer if record else "")
    return value


def _record(raw, line, schemas):
    value = _object(raw, line, record=True)
    for name in value:
        if name not in _MEMBERS:
            raise Invalid(line, "unknown_field", "/" + escape_token(name))
    for name in _REQUIRED:
        if name not in value:
            raise Invalid(line, "required", "/" + name)
    id_ = value["id"]
    if not isinstance(id_, RawNumber) or not re.fullmatch(r"[1-9][0-9]*", id_) or int(id_) > 2**63 - 1:
        raise Invalid(line, "type_mismatch", "/id")

    def string(name):
        v = value[name]
        if not isinstance(v, str) or isinstance(v, RawNumber):
            raise Invalid(line, "type_mismatch", "/" + name)
        return v

    if not _UUID.fullmatch(string("uid")):
        raise Invalid(line, "invalid_format", "/uid")
    hash_ = string("content_hash")
    if not re.fullmatch("[0-9a-f]{64}", hash_):
        raise Invalid(line, "invalid_format", "/content_hash")
    kind = string("kind")
    for name in ("created_at", "occurred_at", "processed_at", "redacted_at"):
        if name in value and _parse_rfc3339(string(name)) is None:
            raise Invalid(line, "invalid_format", "/" + name)
    for name in ("verdict", "resolution", "ref", "processed_by"):
        if name in value:
            string(name)
            if "processed_at" not in value:
                raise Invalid(line, "invalid_value", "/" + name)
    tombstone = "redacted_at" in value
    if tombstone:
        if value["payload"] != {"redacted": True}:
            raise Invalid(line, "invalid_value", "/payload")
        for name in ("summary", "context"):
            if name in value:
                raise Invalid(line, "invalid_value", "/" + name)
    envelope = {k: v for k, v in value.items() if k in ENVELOPE_MEMBERS and not (k == "kind" and kind == "unknown")}
    result = decode(canonical_bytes(envelope), schemas,
                    max_bytes=IMPORT_LIMIT, max_depth=IMPORT_DEPTH)
    if result.status != 201:
        raise Invalid(line, "invalid_format", "")
    if not tombstone and result.content_hash != hash_:
        raise Invalid(line, "invalid_value", "/content_hash")
    return {
        "line": line,
        "identity": result.hash_input,
        "envelope": result.envelope,
        "warnings": [] if tombstone else [{"code": w.code, "pointer": w.pointer} for w in result.warnings],
        "retained_hash": tombstone,
        "content_hash": hash_,
    }


def _verify(body, operation, schemas):
    if operation == "import" and len(body) > IMPORT_LIMIT:
        raise Invalid(None, None, None, 413)
    lines = body.split(b"\n")
    if lines[-1] == b"":
        lines.pop()
    for line, raw in enumerate(lines, 1):
        if not raw or raw.endswith(b"\r"):
            raise Invalid(line, "invalid_format", "")
    if len(lines) < 2:
        raise Invalid(None, "invalid_format", "")
    header = _object(lines[0], 1)
    if not isinstance(header.get("export_format"), RawNumber) or header["export_format"] != "2":
        raise Invalid(1, "invalid_format", "/export_format")
    trailer = _object(lines[-1], len(lines))
    if trailer.get("export_complete") is not True:
        raise Invalid(len(lines), "invalid_format", "/export_complete")
    count = trailer.get("count")
    if not isinstance(count, RawNumber) or not re.fullmatch(r"-?[0-9]+", count) or int(count) != len(lines) - 2:
        raise Invalid(len(lines), "invalid_value", "/count")
    digest = hashlib.sha256(b"".join(raw + b"\n" for raw in lines[1:-1])).hexdigest()
    if trailer.get("sha256") != digest:
        raise Invalid(len(lines), "invalid_value", "/sha256")
    return [_record(raw, line, schemas) for line, raw in enumerate(lines[1:-1], 2)]


def _input(d, step):
    if "input" in step:
        return (d / step["input"]).read_bytes()
    spec = json.loads((d / step["generator"]).read_text())
    # Compact size-cap fixtures insert ASCII whitespace after a complete header.
    # Check the construction, then verify its semantically identical short form;
    # Go expands and submits all total_bytes bytes to the real service.
    if spec.get("pad") != " " or spec.get("prefix") != '{"export_format":2}' or not spec.get("suffix", "").startswith("\n"):
        raise ValueError("unsupported import generator construction")
    minimum = len(spec["prefix"].encode()) + len(spec["suffix"].encode())
    if type(spec["total_bytes"]) is not int or spec["total_bytes"] < minimum:
        raise ValueError("invalid generator total_bytes")
    if step["operation"] == "import" and spec["total_bytes"] > IMPORT_LIMIT:
        raise Invalid(None, None, None, 413)
    return (spec["prefix"] + spec["suffix"]).encode()


def check(d: Path, schemas) -> list[str]:
    scenario = json.loads((d / "scenario.json").read_text())
    if scenario.get("mode") == "client-migration":
        from .migration_fixtures import check as check_migration
        return check_migration(d, schemas)
    expected = json.loads((d / "expected.json").read_text(),
                          parse_int=RawNumber, parse_float=RawNumber)
    problems = []
    if len(scenario["steps"]) != len(expected["steps"]):
        return ["scenario/expected step count differs"]
    for index, (step, want) in enumerate(zip(scenario["steps"], expected["steps"]), 1):
        if step["operation"] == "create":
            result = decode(_input(d, step), schemas)
            if result.status != 201:
                problems.append(f"step {index}: create probe rejected with {result.status} {result.error}")
                continue
            # The assigned ID is a Go-only assertion; use it only to select
            # the authored snapshot whose computable envelope/hash we check.
            records = [record for record in want["records"]
                       if int(record["id"]) == int(want["created_id"])]
            if len(records) != 1:
                problems.append(f"step {index}: create probe snapshot is missing or duplicated")
                continue
            record = records[0]
            envelope = {name: record[name] for name in ENVELOPE_MEMBERS if name in record}
            if canonical_bytes(result.envelope) != canonical_bytes(envelope):
                problems.append(f"step {index}: create probe canonical envelope differs")
            if result.content_hash != record.get("content_hash"):
                problems.append(f"step {index}: create probe content_hash differs")
            continue
        try:
            verified = _verify(_input(d, step), step["operation"], schemas)
        except Invalid as error:
            expected_error = want.get("error")
            if expected_error is not None:
                expected_error = {
                    name: int(value) if name in ("status", "line") and value is not None else value
                    for name, value in expected_error.items()
                }
            if error.value != expected_error:
                problems.append(f"step {index}: validation {error.value!r} != {expected_error!r}")
            continue
        if "error" in want:
            problems.append(f"step {index}: invalid stream unexpectedly verified")
            continue
        wanted = want["verified"]
        if len(verified) != len(wanted):
            problems.append(f"step {index}: verified record count differs")
            continue
        for got, record in zip(verified, wanted):
            if got["line"] != int(record["line"]):
                problems.append(f"step {index}: line differs")
            if got["retained_hash"] != record["retained_hash"]:
                problems.append(f"step {index}: retained_hash differs")
            if "original_identity" in record:
                original = canonical_bytes(record["original_identity"])
                if not got["retained_hash"] or hashlib.sha256(original).hexdigest() != got["content_hash"]:
                    problems.append(f"step {index}, line {got['line']}: retained original hash differs")
            for name in ("identity", "envelope"):
                if canonical_bytes(got[name]) != canonical_bytes(record[name]):
                    problems.append(f"step {index}, line {got['line']}: canonical {name} differs")
            keys = lambda ws: sorted((w["code"], w["pointer"]) for w in ws)
            if keys(got["warnings"]) != keys(record["warnings"]):
                problems.append(f"step {index}, line {got['line']}: pre-dedupe warnings differ")
    return problems
