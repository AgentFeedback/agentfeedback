"""Token-level JSON reader that keeps what a decoder must keep.

Standard parsers lose three things the contract depends on: the source
spelling of numbers (``1.0`` versus ``1``), duplicate member names (last value
wins, with a warning), and the exact points where invalid UTF-8 or escaped lone
surrogates were replaced by U+FFFD. This reader keeps all three. It implements
RFC 8259 exactly: anything else is not JSON and the decoder answers 400.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

_NUMBER = re.compile(rb"-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?")
_WS = b" \t\n\r"
# Containers nested deeper than this are rejected like non-JSON (the body
# object itself is level 1). Keeps every implementation, and the SQLite JSON
# functions behind the store, inside their own depth limits.
MAX_DEPTH = 512


class RawNumber(str):
    """A JSON number kept as its source spelling."""

    __slots__ = ()


class NotJSON(ValueError):
    """The body is not RFC 8259 JSON."""


@dataclass
class Warning_:
    code: str
    pointer: str
    message: str = ""

    def key(self) -> tuple[str, str]:
        return (self.code, self.pointer)


def escape_token(token: str) -> str:
    """RFC 6901 escaping of one reference token."""
    return token.replace("~", "~0").replace("/", "~1")


@dataclass
class Reader:
    data: bytes
    pos: int = 0
    warnings: list[Warning_] = field(default_factory=list)
    depth: int = 0

    def _enter(self) -> None:
        self.depth += 1
        if self.depth > MAX_DEPTH:
            raise NotJSON(f"nested deeper than {MAX_DEPTH} levels")

    def parse(self):
        self._skip_ws()
        value = self._value("")
        self._skip_ws()
        if self.pos != len(self.data):
            raise NotJSON(f"trailing bytes at offset {self.pos}")
        return value

    def _skip_ws(self) -> None:
        while self.pos < len(self.data) and self.data[self.pos] in _WS:
            self.pos += 1

    def _value(self, pointer: str):
        if self.pos >= len(self.data):
            raise NotJSON("unexpected end of body")
        c = self.data[self.pos]
        if c == 0x7B:
            return self._object(pointer)
        if c == 0x5B:
            return self._array(pointer)
        if c == 0x22:
            return self._string(pointer)
        if self.data.startswith(b"true", self.pos):
            self.pos += 4
            return True
        if self.data.startswith(b"false", self.pos):
            self.pos += 5
            return False
        if self.data.startswith(b"null", self.pos):
            self.pos += 4
            return None
        m = _NUMBER.match(self.data, self.pos)
        if m and m.end() > self.pos:
            self.pos = m.end()
            return RawNumber(m.group().decode("ascii"))
        raise NotJSON(f"unexpected byte 0x{c:02x} at offset {self.pos}")

    def _object(self, pointer: str) -> dict:
        self._enter()
        self.pos += 1
        result: dict = {}
        self._skip_ws()
        if self._peek() == 0x7D:
            self.pos += 1
            self.depth -= 1
            return result
        while True:
            self._skip_ws()
            if self._peek() != 0x22:
                raise NotJSON(f"expected a member name at offset {self.pos}")
            # The name's own pointer is the member it names.
            name = self._string(None)
            member = pointer + "/" + escape_token(name)
            if name in result:
                # Last value wins. Warnings about the discarded value point at
                # content that no longer exists, so they are dropped too.
                self.warnings = [w for w in self.warnings
                                 if not (w.pointer == member or w.pointer.startswith(member + "/"))]
                self.warnings.append(Warning_("duplicate_key", member, f"member {name!r} appears more than once; the last value is kept"))
                del result[name]
            if self._pending_name_warning:
                self.warnings.append(Warning_("invalid_utf8", member, "invalid UTF-8 in a member name replaced by U+FFFD"))
                self._pending_name_warning = False
            self._skip_ws()
            if self._peek() != 0x3A:
                raise NotJSON(f"expected ':' at offset {self.pos}")
            self.pos += 1
            self._skip_ws()
            value = self._value(member)
            result[name] = value
            self._skip_ws()
            c = self._peek()
            if c == 0x2C:
                self.pos += 1
                continue
            if c == 0x7D:
                self.pos += 1
                self.depth -= 1
                return result
            raise NotJSON(f"expected ',' or '}}' at offset {self.pos}")

    def _array(self, pointer: str) -> list:
        self._enter()
        self.pos += 1
        result: list = []
        self._skip_ws()
        if self._peek() == 0x5D:
            self.pos += 1
            self.depth -= 1
            return result
        while True:
            self._skip_ws()
            result.append(self._value(f"{pointer}/{len(result)}"))
            self._skip_ws()
            c = self._peek()
            if c == 0x2C:
                self.pos += 1
                continue
            if c == 0x5D:
                self.pos += 1
                self.depth -= 1
                return result
            raise NotJSON(f"expected ',' or ']' at offset {self.pos}")

    _pending_name_warning: bool = False

    def _string(self, pointer: str | None) -> str:
        """Read a string literal. ``pointer`` None means a member name: the
        warning is deferred until the full pointer is known."""
        self.pos += 1
        start = self.pos
        # Find the closing quote, honouring escapes; reject raw control characters.
        while True:
            if self.pos >= len(self.data):
                raise NotJSON("unterminated string")
            c = self.data[self.pos]
            if c == 0x22:
                break
            if c == 0x5C:
                self.pos += 2
                continue
            if c < 0x20:
                raise NotJSON(f"raw control character in string at offset {self.pos}")
            self.pos += 1
        raw = self.data[start:self.pos]
        self.pos += 1
        replaced = False
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            # One U+FFFD per maximal ill-formed subsequence (Unicode ch. 3, table 3-8).
            text = raw.decode("utf-8", "replace")
            replaced = True
        text, lone = _unescape(text)
        if replaced or lone:
            if pointer is None:
                self._pending_name_warning = True
            else:
                self.warnings.append(Warning_("invalid_utf8", pointer, "invalid UTF-8 or a lone surrogate escape replaced by U+FFFD"))
        return text

    def _peek(self) -> int:
        if self.pos >= len(self.data):
            raise NotJSON("unexpected end of body")
        return self.data[self.pos]


_SIMPLE = {"\"": "\"", "\\": "\\", "/": "/", "b": "\b", "f": "\f", "n": "\n", "r": "\r", "t": "\t"}


def _unescape(text: str) -> tuple[str, bool]:
    """Resolve JSON escapes. Returns the text and whether a lone surrogate was replaced."""
    if "\\" not in text:
        return text, False
    out: list[str] = []
    i = 0
    lone = False
    n = len(text)
    while i < n:
        c = text[i]
        if c != "\\":
            out.append(c)
            i += 1
            continue
        if i + 1 >= n:
            raise NotJSON("dangling backslash")
        e = text[i + 1]
        if e in _SIMPLE:
            out.append(_SIMPLE[e])
            i += 2
            continue
        if e != "u":
            raise NotJSON(f"invalid escape \\{e}")
        cp = _hex4(text, i + 2)
        i += 6
        if 0xD800 <= cp <= 0xDBFF:
            # High surrogate: needs a following \uDC00-\uDFFF escape.
            if text.startswith("\\u", i):
                low = _hex4(text, i + 2)
                if 0xDC00 <= low <= 0xDFFF:
                    out.append(chr(0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00)))
                    i += 6
                    continue
            out.append("�")
            lone = True
            continue
        if 0xDC00 <= cp <= 0xDFFF:
            out.append("�")
            lone = True
            continue
        out.append(chr(cp))
    return "".join(out), lone


def _hex4(text: str, at: int) -> int:
    h = text[at:at + 4]
    if len(h) != 4 or not all(ch in "0123456789abcdefABCDEF" for ch in h):
        raise NotJSON("invalid \\u escape")
    return int(h, 16)


def parse(data: bytes) -> tuple[object, list[Warning_]]:
    """Parse ``data``. Raises NotJSON. Returns the value and the decode warnings
    (invalid_utf8, duplicate_key) with pointers into the parsed value."""
    reader = Reader(data)
    value = reader.parse()
    return value, reader.warnings
