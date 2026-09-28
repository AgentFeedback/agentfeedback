"""The write path of the contract: raw body in, stored envelope and warnings out.

This is the reference the conformance fixtures are checked against. It follows
the inference table row by row, then the normalisation order, then the kind
schema as a guide, then the recommended-member check, and finally computes
the identity input and its hash. It is independent of the Go implementation
on purpose: both must agree with the fixtures and with each other.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path

from . import guide
from .canon import canonical, canonical_bytes
from .rawjson import NotJSON, RawNumber, Warning_, escape_token, parse
from .text import newlines_to_spaces, token, trim, truncate_bytes

BODY_LIMIT = 10 * 1024 * 1024
CONTEXT_ENTRIES = 32
CONTEXT_VALUE_BYTES = 2000
SCHEMA_VERSION_MAX = 2**53 - 1

ENVELOPE_MEMBERS = ("kind", "schema_version", "key", "summary", "machine", "model",
                    "harness", "project", "occurred_at", "context", "payload")
# (member, byte limit or None, transform): the string members and their normalisation.
_STRING_MEMBERS = {
    "key": (None, "trim"),
    "summary": (2000, "trim"),
    "machine": (200, "trim"),
    "model": (200, "trim"),
    "harness": (200, "token"),
    "project": (200, "trim"),
}
_RECOMMENDED = ("kind", "summary", "machine", "model")
HASH_MEMBERS = ("kind", "schema_version", "machine", "model", "harness", "project", "summary", "payload")

_SCHEMAS_DIR = Path(__file__).resolve().parents[2] / "schemas" / "kinds"
# RFC 3339 section 5.6 date-time: ASCII digits only, no leap second, T/Z in either case.
_RFC3339 = re.compile(
    r"([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(?:[Zz]|([+-])([0-9]{2}):([0-9]{2}))"
)


@dataclass
class Result:
    status: int
    error: str | None = None
    envelope: dict = field(default_factory=dict)
    warnings: list[Warning_] = field(default_factory=list)
    hash_input: dict = field(default_factory=dict)
    content_hash: str = ""

    def warning_keys(self) -> list[tuple[str, str]]:
        return sorted(w.key() for w in self.warnings)


def load_kind_schemas() -> dict[tuple[str, int], dict]:
    schemas = {}
    for path in sorted(_SCHEMAS_DIR.glob("*.v*.json")):
        kind, _, rest = path.name.partition(".v")
        version = int(rest.removesuffix(".json"))
        schema = json.loads(path.read_text(encoding="utf-8"))
        guide.check_schema(schema)
        schemas[(kind, version)] = schema
    return schemas


def _is_string(v) -> bool:
    return isinstance(v, str) and not isinstance(v, RawNumber)


def _collapse(warnings: list[Warning_], member: str) -> None:
    """A structured value became one string: warnings about its parts now
    point at the member itself."""
    for w in warnings:
        if w.pointer.startswith(member + "/"):
            w.pointer = member


def _remap(warnings: list[Warning_], old: str, new: str) -> None:
    """Parse-time warnings point into the body as sent; when inference moves a
    member, its warnings move with it."""
    for w in warnings:
        if w.pointer == old or w.pointer.startswith(old + "/"):
            w.pointer = new + w.pointer[len(old):]


def decode(body: bytes, schemas: dict[tuple[str, int], dict] | None = None) -> Result:
    if len(body) > BODY_LIMIT:
        return Result(413, "request_too_large")
    try:
        value, warnings = parse(body)
    except NotJSON:
        return Result(400, "bad_request")
    if not isinstance(value, dict):
        return Result(400, "bad_request")
    if schemas is None:
        schemas = load_kind_schemas()

    env: dict = {}
    placed: set[str] = set()   # payload members inference put there
    payload_present = "payload" in value

    # null members mean absent (one coerced warning each).
    for name in ENVELOPE_MEMBERS:
        if name in value and value[name] is None:
            warnings.append(Warning_("coerced", "/" + name, f"{name} is null; treated as absent"))
            del value[name]
            if name == "payload":
                payload_present = False

    # payload present but not an object.
    payload = value.get("payload")
    if payload_present and "payload" in value and not isinstance(payload, dict):
        _remap(warnings, "/payload", "/payload/value")
        payload = {"value": payload}
        placed.add("value")
        warnings.append(Warning_("payload_wrapped", "/payload", "payload was not an object; wrapped as {\"value\": ...}"))
    if payload is None:
        payload = {}

    # Unknown top-level members move into payload, in code point order of
    # their names; a flat body becomes the payload.
    unknown = sorted(n for n in value if n not in ENVELOPE_MEMBERS)
    for name in unknown:
        hint = ""
        if name.lower() in ENVELOPE_MEMBERS:
            hint = f"; did you mean {name.lower()}"
        placed.add(_move_into_payload(payload, name, value[name], warnings, hint))
    if not payload_present:
        warnings.append(Warning_("payload_inferred", "/payload", "payload was absent; built from the members the envelope does not know"))

    # kind
    kind = value.get("kind")
    if _is_string(kind):
        kind = token(kind)
    if not _is_string(kind) or kind == "":
        warnings.append(Warning_("missing_kind", "/kind", "kind is missing, empty or not a string; stored as unknown"))
        kind_inferred = True
        kind = "unknown"
    else:
        kind_inferred = False
    env["kind"] = kind

    # schema_version
    sv = value.get("schema_version")
    if sv is None:
        version = 1
    elif isinstance(sv, RawNumber) and re.fullmatch(r"[1-9][0-9]*", str(sv)) and len(str(sv)) <= 16 and int(sv) <= SCHEMA_VERSION_MAX:
        version = int(sv)
    else:
        version = 1
        warnings.append(Warning_("schema_version_defaulted", "/schema_version", "schema_version is not a positive integer; defaulted to 1"))
    env["schema_version"] = version

    # String members: coerce, then normalise in the fixed order.
    for name, (limit, transform) in _STRING_MEMBERS.items():
        if name not in value:
            continue
        s = value[name]
        if not _is_string(s):
            s = canonical(s)
            _collapse(warnings, "/" + name)
            warnings.append(Warning_("coerced", "/" + name, f"{name} was not a string; encoded as canonical JSON"))
        s = trim(s)
        if name == "summary":
            s = newlines_to_spaces(s)
        if transform == "token":
            s = token(s)
        if limit is not None:
            s, cut = truncate_bytes(s, limit)
            if cut:
                warnings.append(Warning_("truncated", "/" + name, f"{name} cut to {limit} bytes"))
        if s == "":
            continue  # empty after normalisation: omitted
        env[name] = s

    # context: a non-object goes to payload.context_raw before occurred_at_raw can be added.
    context = value.get("context")
    if "context" in value and not isinstance(context, dict):
        placed.add(_place(payload, "context_raw", context, warnings, "moved_to_payload",
                          "context was not an object; moved to payload.context_raw", "/context"))
        context = None
    context = dict(context) if isinstance(context, dict) else {}

    # occurred_at
    if "occurred_at" in value:
        raw = value["occurred_at"]
        coerced = not _is_string(raw)
        text = raw if _is_string(raw) else canonical(raw)
        if coerced:
            _collapse(warnings, "/occurred_at")
        parsed = _parse_rfc3339(text)
        if parsed is not None:
            env["occurred_at"] = parsed
            if coerced:
                warnings.append(Warning_("coerced", "/occurred_at", "occurred_at was not a string"))
        else:
            if "occurred_at_raw" in context:
                warnings.append(Warning_("duplicate_key", "/context/occurred_at_raw", "context.occurred_at_raw replaced"))
            _remap(warnings, "/occurred_at", "/context/occurred_at_raw")
            context["occurred_at_raw"] = text
            if coerced:
                warnings.append(Warning_("coerced", "/context/occurred_at_raw", "occurred_at was not a string"))
            warnings.append(Warning_("invalid_format", "/context/occurred_at_raw", "occurred_at is not an RFC 3339 date-time; kept as context.occurred_at_raw"))

    # context: cap, then value coercion and truncation of the kept entries.
    if len(context) > CONTEXT_ENTRIES:
        keys = sorted(context)
        overflow = {k: context[k] for k in keys[CONTEXT_ENTRIES:]}
        context = {k: context[k] for k in keys[:CONTEXT_ENTRIES]}
        placed_in = _place(payload, "context_overflow", overflow, warnings, "truncated",
                           f"context has more than {CONTEXT_ENTRIES} entries; the rest moved to payload.context_overflow")
        placed.add(placed_in)
        # Remap after placing: placing may wrap a non-object payload.moved,
        # and that remap must not catch the overflow entries.
        target = "/payload/moved/context_overflow" if placed_in == "moved" else "/payload/context_overflow"
        for k in overflow:
            _remap(warnings, "/context/" + escape_token(k), target + "/" + escape_token(k))
    for k in list(context):
        v = context[k]
        p = "/context/" + escape_token(k)
        if not _is_string(v):
            v = canonical(v)
            _collapse(warnings, p)
            warnings.append(Warning_("coerced", p, "context values are strings; encoded as canonical JSON"))
        v, cut = truncate_bytes(v, CONTEXT_VALUE_BYTES)
        if cut:
            warnings.append(Warning_("truncated", p, f"context value cut to {CONTEXT_VALUE_BYTES} bytes"))
        context[k] = v
    if context:
        env["context"] = context

    # Kind schema as a guide.
    if not kind_inferred:
        if (kind, version) in schemas:
            warnings.extend(guide.validate(payload, schemas[(kind, version)], placed))
        elif any(k == kind for k, _ in schemas):
            warnings.append(Warning_("unknown_schema_version", "/schema_version", f"no schema for {kind} version {version}"))
        else:
            warnings.append(Warning_("no_schema", "/kind", f"no schema for kind {kind!r}; payload stored unvalidated"))
    env["payload"] = payload

    # Recommended envelope members.
    for name in _RECOMMENDED:
        if name not in env:
            warnings.append(Warning_("missing_recommended", "/" + name, f"{name} is recommended"))

    ordered = {name: env[name] for name in ENVELOPE_MEMBERS if name in env}
    hash_input = {name: ordered[name] for name in HASH_MEMBERS if name in ordered}
    digest = hashlib.sha256(canonical_bytes(hash_input)).hexdigest()
    return Result(201, None, ordered, warnings, hash_input, digest)


def _place(payload: dict, name: str, val, warnings: list[Warning_], code: str, message: str,
           old_pointer: str | None = None) -> str:
    """Put an inferred member into payload under ``name``; when that name is
    taken it goes under payload.moved.<name>, and a second collision there
    replaces the earlier value with a duplicate_key warning. Parse-time
    warnings under ``old_pointer`` follow the member. Returns the top-level
    payload member that received it."""
    # under_moved, not the returned name: a member itself named "moved"
    # that finds no payload.moved is placed at the top level.
    under_moved = name in payload
    if not under_moved:
        pointer = "/payload/" + escape_token(name)
    else:
        moved = payload.get("moved")
        if "moved" in payload and not isinstance(moved, dict):
            _remap(warnings, "/payload/moved", "/payload/moved/value")
            payload["moved"] = {"value": moved}
            warnings.append(Warning_("payload_wrapped", "/payload/moved", "payload.moved is reserved and was not an object; wrapped"))
        payload.setdefault("moved", {})
        pointer = "/payload/moved/" + escape_token(name)
        if name in payload["moved"]:
            warnings.append(Warning_("duplicate_key", pointer, f"payload.moved.{name} replaced"))
    if old_pointer is not None:
        _remap(warnings, old_pointer, pointer)
    if under_moved:
        payload["moved"][name] = val
    else:
        payload[name] = val
    warnings.append(Warning_(code, pointer, message))
    return "moved" if under_moved else name


def _move_into_payload(payload: dict, name: str, val, warnings: list[Warning_], hint: str) -> str:
    return _place(payload, name, val, warnings, "moved_to_payload",
                  f"{name} is not an envelope member; moved into payload{hint}", "/" + escape_token(name))


def _parse_rfc3339(text: str) -> str | None:
    """RFC 3339 section 5.6 date-time without leap seconds. Returns the UTC
    form with six fractional digits, or None. A result outside years 0001 to
    9999 is unrepresentable and counts as unparseable."""
    m = _RFC3339.fullmatch(text)
    if not m:
        return None
    year, month, day, hour, minute, second, frac, sign, oh, om = m.groups()
    try:
        dt = datetime(int(year), int(month), int(day), int(hour), int(minute), int(second), tzinfo=timezone.utc)
        dt = dt.replace(microsecond=int((frac or "")[:6].ljust(6, "0")))
        if sign:
            if int(oh) > 23 or int(om) > 59:
                return None
            offset = timedelta(hours=int(oh), minutes=int(om))
            dt = dt - offset if sign == "+" else dt + offset
    except (ValueError, OverflowError):
        return None
    return (f"{dt.year:04d}-{dt.month:02d}-{dt.day:02d}T{dt.hour:02d}:{dt.minute:02d}:{dt.second:02d}"
            f".{dt.microsecond:06d}Z")


def gen_body(spec: dict) -> bytes:
    """Build a body from a body.gen.json description: prefix, one pad byte
    repeated, suffix, to an exact total length."""
    prefix = spec["prefix"].encode("utf-8")
    suffix = spec.get("suffix", "").encode("utf-8")
    pad = spec["pad"].encode("utf-8")
    total = spec["total_bytes"]
    fill = total - len(prefix) - len(suffix)
    if fill < 0 or len(pad) != 1:
        raise ValueError("bad body.gen.json")
    return prefix + pad * fill + suffix


def summarise(result: Result) -> dict:
    """The expected.json view of a result."""
    out: dict = {"status": result.status}
    if result.error:
        out["error"] = result.error
    else:
        out["warnings"] = [{"code": c, "pointer": p} for c, p in result.warning_keys()]
    return out
