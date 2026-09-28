"""Normalisation primitives defined by the contract, without the host's helpers.

Python's ``str.strip`` and ``str.lower`` are deliberately not used: ``strip``
removes U+001C..U+001F, which are not White_Space, and ``lower`` applies the
full, context-sensitive mapping (final sigma, U+0130 to two code points),
where the contract wants the simple, context-free mapping of each code point.
"""

from __future__ import annotations

# Unicode White_Space property (PropList.txt, unchanged since Unicode 6.3,
# when U+180E left the set).
WHITE_SPACE = frozenset(
    [chr(c) for c in range(0x09, 0x0E)]           # TAB, LF, VT, FF, CR
    + [chr(0x20), chr(0x85), chr(0xA0), chr(0x1680)]
    + [chr(c) for c in range(0x2000, 0x200B)]     # EN QUAD .. HAIR SPACE
    + [chr(0x2028), chr(0x2029), chr(0x202F), chr(0x205F), chr(0x3000)]
)

# Code points whose full lower-case mapping is not their simple mapping
# (SpecialCasing.txt, unconditional entries). Only one exists for lower-casing.
_SIMPLE_LOWER_OVERRIDES = {"\u0130": "i"}


def trim(s: str) -> str:
    start, end = 0, len(s)
    while start < end and s[start] in WHITE_SPACE:
        start += 1
    while end > start and s[end - 1] in WHITE_SPACE:
        end -= 1
    return s[start:end]


def simple_lower(s: str) -> str:
    out = []
    for ch in s:
        if ch in _SIMPLE_LOWER_OVERRIDES:
            out.append(_SIMPLE_LOWER_OVERRIDES[ch])
            continue
        low = ch.lower()
        # A single code point applied out of context gives the simple mapping
        # for every other character (a lone U+03A3 lowers to U+03C3).
        out.append(low if len(low) == 1 else ch)
    return "".join(out)


def token(s: str) -> str:
    """x-normalize: token. Trim, lower-case, runs of whitespace to one '-'."""
    s = simple_lower(trim(s))
    out = []
    in_ws = False
    for ch in s:
        if ch in WHITE_SPACE:
            if not in_ws:
                out.append("-")
                in_ws = True
        else:
            out.append(ch)
            in_ws = False
    return "".join(out)


def newlines_to_spaces(s: str) -> str:
    """In summary: CR LF, lone CR and lone LF each become one space."""
    return s.replace("\r\n", " ").replace("\r", " ").replace("\n", " ")


def truncate_bytes(s: str, limit: int) -> tuple[str, bool]:
    """Cut at the last complete code point at or before ``limit`` UTF-8 bytes."""
    b = s.encode("utf-8")
    if len(b) <= limit:
        return s, False
    cut = limit
    while cut > 0 and (b[cut] & 0xC0) == 0x80:
        cut -= 1
    return b[:cut].decode("utf-8"), True


def utf8_len(s: str) -> int:
    return len(s.encode("utf-8"))
