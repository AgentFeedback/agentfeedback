"""Canonical JSON as the contract defines it, written byte by byte.

UTF-8, no whitespace, members sorted by the code point order of their keys at
every level, arrays in order, numbers verbatim as received, strings escaped
minimally (only ``"``, ``\\``, LF, CR, TAB by name and the other C0 controls as
``\\u00xx``); everything else, including ``/``, U+2028 and U+2029 and every
non-ASCII code point, is written raw.
"""

from __future__ import annotations

from .rawjson import RawNumber

_NAMED = {'"': '\\"', "\\": "\\\\", "\n": "\\n", "\r": "\\r", "\t": "\\t"}


def canonical_string(s: str) -> str:
    out = ['"']
    for ch in s:
        if ch in _NAMED:
            out.append(_NAMED[ch])
        elif ord(ch) < 0x20:
            out.append(f"\\u{ord(ch):04x}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def canonical(value) -> str:
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, RawNumber):
        return str(value)
    if isinstance(value, int) and not isinstance(value, bool):
        return str(value)
    if isinstance(value, str):
        return canonical_string(value)
    if isinstance(value, list):
        return "[" + ",".join(canonical(v) for v in value) + "]"
    if isinstance(value, dict):
        # Python compares str by code point, which is the byte order of UTF-8 keys.
        items = sorted(value.items(), key=lambda kv: kv[0])
        return "{" + ",".join(canonical_string(k) + ":" + canonical(v) for k, v in items) + "}"
    raise TypeError(f"not a JSON value: {type(value).__name__}")


def canonical_bytes(value) -> bytes:
    return canonical(value).encode("utf-8")
