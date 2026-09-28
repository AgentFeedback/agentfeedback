"""Guide validation of a payload against a kind schema.

A kind schema never rejects. Its transforming keywords (``x-trim``,
``x-normalize``, ``x-on-violation: truncate``) rewrite the payload silently;
every other violation is a warning whose code comes from the keyword that
failed (the mapping is listed in ``conformance/warnings.json``). Members a
schema does not list are kept and reported as ``unknown_field``.

Only the keywords the shipped kind schemas use are implemented; an unknown
keyword in a kind schema is an error here, so the reference never passes a
schema it silently ignores.
"""

from __future__ import annotations

from decimal import Decimal, InvalidOperation

from .rawjson import RawNumber, Warning_, escape_token
from .text import token, trim, truncate_bytes, utf8_len

_SUPPORTED = {
    "$schema", "$id", "title", "description", "examples", "default",
    "type", "enum", "minimum", "maximum", "minLength", "minItems", "maxItems",
    "maxProperties", "properties", "additionalProperties", "items",
    "x-max-bytes", "x-trim", "x-normalize", "x-recommended", "x-unique-by", "x-on-violation",
}


def validate(payload: dict, schema: dict, placed: set[str] | None = None) -> list[Warning_]:
    """``placed`` names the top-level payload members inference put there;
    they were already reported once and get no unknown_field."""
    warnings: list[Warning_] = []
    _apply(payload, schema, "/payload", warnings, placed or set())
    return warnings


def _type_of(value) -> str:
    if value is None:
        return "null"
    if value is True or value is False:
        return "boolean"
    if isinstance(value, RawNumber):
        try:
            d = Decimal(str(value))
        except InvalidOperation:
            return "number"
        return "integer" if d == d.to_integral_value() else "number"
    if isinstance(value, str):
        return "string"
    if isinstance(value, list):
        return "array"
    if isinstance(value, dict):
        return "object"
    raise TypeError(type(value).__name__)


def _matches_type(actual: str, wanted) -> bool:
    wanted = wanted if isinstance(wanted, list) else [wanted]
    return actual in wanted or (actual == "integer" and "number" in wanted)


def _apply(value, schema: dict, pointer: str, warnings: list[Warning_], placed: set[str] = frozenset()):
    """Validate ``value`` in place (transforms mutate containers). Returns the
    possibly transformed value so callers holding a scalar can store it."""
    unknown = set(schema) - _SUPPORTED
    if unknown:
        raise ValueError(f"kind schema uses unsupported keywords at {pointer}: {sorted(unknown)}")

    actual = _type_of(value)
    if "type" in schema and not _matches_type(actual, schema["type"]):
        warnings.append(Warning_("type_mismatch", pointer, f"expected {schema['type']}, got {actual}"))
        return value

    if actual == "string":
        if schema.get("x-normalize") == "token":
            value = token(value)
        elif schema.get("x-trim"):
            value = trim(value)
        limit = schema.get("x-max-bytes")
        if limit is not None and utf8_len(value) > limit:
            if schema.get("x-on-violation") == "truncate":
                value, _ = truncate_bytes(value, limit)
            else:
                warnings.append(Warning_("too_long", pointer, f"longer than {limit} bytes"))
        if "minLength" in schema and len(value) < schema["minLength"]:
            warnings.append(Warning_("out_of_range", pointer, f"shorter than {schema['minLength']} code points"))
        if "enum" in schema and value not in schema["enum"]:
            warnings.append(Warning_("out_of_range", pointer, f"not one of {schema['enum']}"))
        return value

    if actual in ("integer", "number"):
        d = Decimal(str(value))
        if "minimum" in schema and d < Decimal(str(schema["minimum"])):
            warnings.append(Warning_("out_of_range", pointer, f"below {schema['minimum']}"))
        if "maximum" in schema and d > Decimal(str(schema["maximum"])):
            warnings.append(Warning_("out_of_range", pointer, f"above {schema['maximum']}"))
        if "enum" in schema and value not in schema["enum"]:
            warnings.append(Warning_("out_of_range", pointer, f"not one of {schema['enum']}"))
        return value

    if actual == "array":
        if "minItems" in schema and len(value) < schema["minItems"]:
            warnings.append(Warning_("out_of_range", pointer, f"fewer than {schema['minItems']} items"))
        if "maxItems" in schema and len(value) > schema["maxItems"]:
            warnings.append(Warning_("out_of_range", pointer, f"more than {schema['maxItems']} items"))
        items = schema.get("items")
        if items:
            for i, item in enumerate(value):
                value[i] = _apply(item, items, f"{pointer}/{i}", warnings)
        member = schema.get("x-unique-by")
        if member:
            seen: set = set()
            for i, item in enumerate(value):
                if isinstance(item, dict) and isinstance(item.get(member), str):
                    if item[member] in seen:
                        warnings.append(Warning_(f"duplicate_{member}", f"{pointer}/{i}", f"{member} {item[member]!r} appears more than once"))
                    seen.add(item[member])
        return value

    if actual == "object":
        if "maxProperties" in schema and len(value) > schema["maxProperties"]:
            warnings.append(Warning_("out_of_range", pointer, f"more than {schema['maxProperties']} members"))
        props = schema.get("properties", {})
        for name in list(value):
            child = f"{pointer}/{escape_token(name)}"
            if name in props:
                value[name] = _apply(value[name], props[name], child, warnings)
            elif pointer == "/payload" and name in placed:
                continue
            else:
                # additionalProperties in a guide only says the member is kept.
                warnings.append(Warning_("unknown_field", child, f"{name!r} is not a member the schema knows"))
        for name in schema.get("x-recommended", []):
            if name not in value:
                warnings.append(Warning_("missing_recommended", f"{pointer}/{escape_token(name)}", f"{name} is recommended"))
        return value

    if "enum" in schema and value not in schema["enum"]:
        warnings.append(Warning_("out_of_range", pointer, f"not one of {schema['enum']}"))
    return value
