#!/usr/bin/env python3
# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "jsonschema==4.26.0",
#   "pyyaml==6.0.3",
#   "rfc3339-validator==0.1.4",
# ]
# ///
"""Check the contract files against each other.

    uv run --locked scripts/contract-check.py

1. The schema files are valid JSON Schema 2020-12 and use only known keywords
   (the custom x- keywords with the value types the contract gives them).
2. Every example in the schema files and in docs/openapi.yaml validates
   against its schema, with $ref resolved to the files under schemas/.
3. Every fixture under conformance/ has the documented shape, every warning
   code it uses is in conformance/warnings.json, every code there (except the
   409-only one) is exercised by a fixture, every stored envelope validates
   against the envelope schema, and every fixture agrees with the reference
   implementation.
4. The rendered Agent Plugins manifests (plugins/agentfeedback/plugin.json and
   the golden mcp.json internal/skillgen renders for a server) validate
   against the vendored Agent Plugins 1.0.0 schemas, and both name the same
   spec version.
5. docs/api.md shows every docs/openapi.yaml operation: a marker comment on
   its own line, `<!-- example: <operationId> -->`, `... <operationId>
   request -->` or `... <operationId> response <status> -->`, applies to the
   next fenced code block (only blank lines between). Every marker names an
   operationId the document has and every operationId has a marker. A request
   block is json and validates against the operation's application/json
   request body schema; a response block is json and validates against that
   status's application/json or application/problem+json schema. A bare
   marker is coverage only, in any language. A line starting with
   `<!-- example` that is not one of these forms, an unclosed example block
   and a $ref that does not resolve are failures.

OpenAPI linting is a separate step (`just contract` runs both).
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

ROOT = Path(__file__).resolve().parents[1]
SCHEMA_FILES = sorted(str(p.relative_to(ROOT)) for p in (ROOT / "schemas").rglob("*.json"))
OPENAPI = ROOT / "docs" / "openapi.yaml"
OPENAPI_URI = "file:///docs/openapi.yaml"

# JSON Schema 2020-12 keywords (core, applicator, validation, meta-data,
# format-annotation, content, unevaluated vocabularies) plus the custom ones.
JSON_SCHEMA_KEYWORDS = {
    "$schema", "$id", "$ref", "$anchor", "$dynamicRef", "$dynamicAnchor", "$vocabulary", "$comment", "$defs",
    "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependentSchemas", "prefixItems", "items", "contains",
    "properties", "patternProperties", "additionalProperties", "propertyNames",
    "type", "enum", "const", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum",
    "maxLength", "minLength", "pattern", "maxItems", "minItems", "uniqueItems", "maxContains", "minContains",
    "maxProperties", "minProperties", "required", "dependentRequired",
    "title", "description", "default", "deprecated", "readOnly", "writeOnly", "examples",
    "format", "contentEncoding", "contentMediaType", "contentSchema", "unevaluatedItems", "unevaluatedProperties",
}
X_KEYWORDS = {
    "x-max-bytes": lambda v: isinstance(v, int) and not isinstance(v, bool) and v > 0,
    "x-trim": lambda v: v is True,
    "x-normalize": lambda v: v == "token",
    "x-recommended": lambda v: isinstance(v, list) and all(isinstance(s, str) for s in v),
    "x-unique-by": lambda v: isinstance(v, str) and v != "key",
    "x-on-violation": lambda v: v in ("warn", "truncate", "move"),
}
# Positions whose values are schemas, for the keyword walk.
_SCHEMA_MAP_KEYWORDS = {"properties", "patternProperties", "$defs", "dependentSchemas"}
_SCHEMA_KEYWORDS = {"additionalProperties", "items", "contains", "propertyNames", "not", "if", "then", "else",
                    "unevaluatedItems", "unevaluatedProperties", "contentSchema"}
_SCHEMA_LIST_KEYWORDS = {"allOf", "anyOf", "oneOf", "prefixItems"}

problems: list[str] = []


def fail(msg: str) -> None:
    problems.append(msg)


def load_json(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def walk_schema(schema, where: str, on_schema) -> None:
    """Call on_schema(subschema, where) for every schema object."""
    if isinstance(schema, bool):
        return
    if not isinstance(schema, dict):
        fail(f"{where}: schema is not an object")
        return
    on_schema(schema, where)
    for k, v in schema.items():
        if k in _SCHEMA_MAP_KEYWORDS and isinstance(v, dict):
            for name, sub in v.items():
                walk_schema(sub, f"{where}/{k}/{name}", on_schema)
        elif k in _SCHEMA_KEYWORDS:
            walk_schema(v, f"{where}/{k}", on_schema)
        elif k in _SCHEMA_LIST_KEYWORDS and isinstance(v, list):
            for i, sub in enumerate(v):
                walk_schema(sub, f"{where}/{k}/{i}", on_schema)


def lint_keywords(schema: dict, where: str) -> None:
    for k, v in schema.items():
        if k.startswith("x-"):
            check = X_KEYWORDS.get(k)
            if check is None:
                fail(f"{where}: unknown custom keyword {k}")
            elif not check(v):
                fail(f"{where}: {k} has an invalid value {v!r}")
        elif k not in JSON_SCHEMA_KEYWORDS:
            fail(f"{where}: {k!r} is not a JSON Schema 2020-12 keyword (typo?)")
    for name in schema.get("x-recommended", []):
        if name not in schema.get("properties", {}):
            fail(f"{where}: x-recommended names {name!r}, which properties does not declare")
    if "x-unique-by" in schema and schema.get("type") != "array":
        fail(f"{where}: x-unique-by on a non-array")
    if schema.get("x-on-violation") == "move" and where != "schemas/envelope.v1.json/additionalProperties":
        fail(f"{where}: x-on-violation move is only for the envelope's additionalProperties")
    if "x-normalize" in schema and "x-trim" in schema:
        fail(f"{where}: x-normalize implies x-trim; declare one")


def registry_with_schemas(schemas: dict[str, dict]) -> Registry:
    registry = Registry()
    for rel, schema in schemas.items():
        resource = Resource.from_contents(schema, default_specification=DRAFT202012)
        registry = registry.with_resource(schema["$id"], resource)
        registry = registry.with_resource(f"file:///{rel}", resource)
    return registry


def validate_examples_in_schema(schema: dict, rel: str, registry: Registry) -> int:
    count = 0

    def on_schema(sub: dict, where: str) -> None:
        nonlocal count
        for i, example in enumerate(sub.get("examples", [])):
            count += 1
            validator = Draft202012Validator(sub, registry=registry, format_checker=FormatChecker())
            for err in validator.iter_errors(example):
                fail(f"{where}/examples/{i}: {err.message} at {err.json_path}")

    walk_schema(schema, rel, on_schema)
    return count


def pointer_escape(token: str) -> str:
    return token.replace("~", "~0").replace("/", "~1")


def validate_openapi_examples(doc: dict, registry: Registry) -> int:
    """Validate every example/examples[].value in the OpenAPI document against
    the schema of its media type, and every example on a schema object."""
    count = 0

    def check(example, pointer: str, where: str) -> None:
        nonlocal count
        count += 1
        validator = Draft202012Validator({"$ref": f"{OPENAPI_URI}#{pointer}"}, registry=registry, format_checker=FormatChecker())
        for err in validator.iter_errors(example):
            fail(f"openapi {where}: {err.message} at {err.json_path}")

    def media_types(node: dict, pointer: str) -> None:
        for mime, media in (node.get("content") or {}).items():
            mp = f"{pointer}/content/{pointer_escape(mime)}"
            if "schema" not in media:
                continue
            if "example" in media:
                check(media["example"], f"{mp}/schema", f"{mp}/example")
            for name, ex in (media.get("examples") or {}).items():
                if "value" in ex:
                    check(ex["value"], f"{mp}/schema", f"{mp}/examples/{name}")

    for path, item in (doc.get("paths") or {}).items():
        pp = f"/paths/{pointer_escape(path)}"
        for method, op in item.items():
            if not isinstance(op, dict) or "responses" not in op:
                continue
            op_p = f"{pp}/{method}"
            if "requestBody" in op:
                media_types(op["requestBody"], f"{op_p}/requestBody")
            for code, resp in op["responses"].items():
                if isinstance(resp, dict):
                    media_types(resp, f"{op_p}/responses/{code}")
            for i, param in enumerate(op.get("parameters") or []):
                if isinstance(param, dict) and "schema" in param and "example" in param:
                    check(param["example"], f"{op_p}/parameters/{i}/schema", f"{op_p}/parameters/{i}/example")
    for name, resp in ((doc.get("components") or {}).get("responses") or {}).items():
        media_types(resp, f"/components/responses/{name}")
    for name, schema in ((doc.get("components") or {}).get("schemas") or {}).items():
        def on_schema(sub: dict, where: str) -> None:
            for i, example in enumerate(sub.get("examples", [])):
                check(example, _pointer_of(where), f"{where}/examples/{i}")
            if "example" in sub:
                check(sub["example"], _pointer_of(where), f"{where}/example")
        walk_schema(schema, f"/components/schemas/{name}", on_schema)
    return count


def _pointer_of(where: str) -> str:
    """walk_schema builds paths with raw names; turn one into a JSON Pointer."""
    parts = where.split("/")[1:]
    return "/" + "/".join(pointer_escape(part) for part in parts)


_EXPECTED_DECODE = {
    "type": "object", "additionalProperties": False,
    "required": ["row", "status"],
    "properties": {
        "row": {"type": "string", "minLength": 1},
        "status": {"enum": [201, 400, 413]},
        "error": {"enum": ["bad_request", "request_too_large"]},
        "warnings": {"type": "array", "items": {"type": "object", "required": ["code", "pointer"], "additionalProperties": False,
                                                "properties": {"code": {"type": "string"}, "pointer": {"type": "string", "pattern": "^(/|$)"}}}},
        "note": {"type": "string"},
    },
}
_EXPECTED_HASH = {
    "type": "object", "additionalProperties": False, "required": ["rule", "sha256"],
    "properties": {"rule": {"type": "string", "minLength": 1}, "sha256": {"type": "string", "pattern": "^[0-9a-f]{64}$"}, "note": {"type": "string"}},
}


def check_fixtures(envelope: dict, registry: Registry, codes: set[str], families: set[str]) -> None:
    used: set[str] = set()
    env_validator = Draft202012Validator(envelope, registry=registry, format_checker=FormatChecker())
    for group, shape in (("decode/rows", _EXPECTED_DECODE), ("decode/interactions", _EXPECTED_DECODE), ("hash", _EXPECTED_HASH)):
        for d in sorted((ROOT / "conformance" / group).iterdir()):
            if not d.is_dir():
                continue
            where = f"conformance/{group}/{d.name}"
            files = {p.name for p in d.iterdir()}
            if ("body.json" in files) == ("body.gen.json" in files):
                fail(f"{where}: exactly one of body.json and body.gen.json")
            if "expected.json" not in files:
                fail(f"{where}: expected.json missing")
                continue
            expected = load_json(d / "expected.json")
            for err in Draft202012Validator(shape).iter_errors(expected):
                fail(f"{where}/expected.json: {err.message} at {err.json_path}")
            if group == "hash":
                if "canonical.json" not in files:
                    fail(f"{where}: canonical.json missing")
                continue
            if expected.get("status") == 201:
                if "stored.json" not in files:
                    fail(f"{where}: stored.json missing")
                else:
                    stored = json.loads((d / "stored.json").read_bytes().decode("utf-8"))
                    for err in env_validator.iter_errors(stored):
                        fail(f"{where}/stored.json: {err.message} at {err.json_path}")
                for w in expected.get("warnings", []):
                    code = w["code"]
                    used.add(code)
                    family = re.sub(r"^duplicate_(?!key$).+$", "duplicate_<member>", code)
                    if code not in codes and family not in families:
                        fail(f"{where}: warning code {code!r} is not in conformance/warnings.json")
            elif "warnings" in expected or "stored.json" in files:
                fail(f"{where}: a rejected body has no warnings and no stored.json")
    for code in codes:
        if code == "key_reused":
            continue
        if code not in used and not (code in families and any(re.fullmatch(r"duplicate_(?!key$).+", u) for u in used)):
            fail(f"conformance/warnings.json: {code!r} is not exercised by any fixture")


API_MD = ROOT / "docs" / "api.md"
_MARKER = re.compile(r"^<!-- example: (\S+)(?: (request|response (\S+)))? -->\s*$")
_MARKER_START = re.compile(r"^\s*<!-- ?example")
_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})\s*(.*?)\s*$")
_MARKER_FORMS = ("`<!-- example: <operationId> -->`, `<!-- example: <operationId> request -->` "
                 "or `<!-- example: <operationId> response <status> -->`")


def _fence_open(line: str):
    """The opening-fence match, or None. A backtick fence whose info string
    contains a backtick is not a fence (CommonMark; as check-docs.py)."""
    m = _FENCE.match(line)
    if m and m.group(1)[0] == "`" and "`" in m.group(2):
        return None
    return m


def _deref(doc: dict, node, pointer: str, where: str):
    """Follow local $refs (#/...) on an OpenAPI object; return it and its
    pointer, or (None, None) after fail() when a target does not exist."""
    while isinstance(node, dict) and isinstance(node.get("$ref"), str) and node["$ref"].startswith("#/"):
        ref = node["$ref"]
        pointer = ref[1:]
        node = doc
        for token in pointer.split("/")[1:]:
            key = token.replace("~1", "/").replace("~0", "~")
            if not isinstance(node, dict) or key not in node:
                fail(f"{where}: $ref {ref} does not resolve")
                return None, None
            node = node[key]
    return node, pointer


def check_api_examples(doc: dict, registry: Registry, text: str, name: str) -> int:
    """Check the example markers in text (the Markdown file shown as name);
    return the validated count."""
    ops: dict[str, tuple[dict, str]] = {}
    for path, item in (doc.get("paths") or {}).items():
        for method, op in item.items():
            if isinstance(op, dict) and "operationId" in op:
                ops[op["operationId"]] = (op, f"/paths/{pointer_escape(path)}/{method}")
    lines = text.split("\n")
    covered: set[str] = set()
    count = 0
    fence: str | None = None
    i = 0
    while i < len(lines):
        line = lines[i]
        m = _FENCE.match(line)
        if fence is not None:
            if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence) and not m.group(2):
                fence = None
            i += 1
            continue
        if _fence_open(line):
            fence = m.group(1)
            i += 1
            continue
        mk = _MARKER.match(line)
        i += 1
        where = f"{name}:{i}"
        if not mk:
            if _MARKER_START.match(line):
                fail(f"{where}: malformed example marker {line.strip()!r}; the accepted forms are {_MARKER_FORMS}")
            continue
        op_id, mode, status = mk.group(1), mk.group(2), mk.group(3)
        j = i
        while j < len(lines) and not lines[j].strip():
            j += 1
        fm = _fence_open(lines[j]) if j < len(lines) else None
        if not fm:
            fail(f"{where}: example marker is not followed by a fenced code block")
            continue
        k = j + 1
        while k < len(lines):
            cm = _FENCE.match(lines[k])
            if cm and cm.group(1)[0] == fm.group(1)[0] and len(cm.group(1)) >= len(fm.group(1)) and not cm.group(2):
                break
            k += 1
        if k >= len(lines):
            fail(f"{name}:{j + 1}: example block is not closed")
            break
        body = "\n".join(lines[j + 1:k])
        lang = fm.group(2).split()[0] if fm.group(2) else ""
        i = k + 1
        if op_id not in ops:
            fail(f"{where}: example marker names {op_id!r}, which docs/openapi.yaml does not define")
            continue
        covered.add(op_id)
        if mode is None:
            continue
        op, op_p = ops[op_id]
        if mode == "request":
            rb = op.get("requestBody")
            rb, rb_p = _deref(doc, rb, f"{op_p}/requestBody", f"{where}: {op_id} request body") if rb is not None else (None, "")
            if rb_p is None:
                continue
            media = ((rb or {}).get("content") or {}).get("application/json")
            if not media or "schema" not in media:
                fail(f"{where}: {op_id} has no application/json request body schema")
                continue
            pointer = f"{rb_p}/content/application~1json/schema"
        else:
            resp = (op.get("responses") or {}).get(status)
            if resp is None:
                fail(f"{where}: {op_id} has no response {status}")
                continue
            resp, resp_p = _deref(doc, resp, f"{op_p}/responses/{status}", f"{where}: {op_id} response {status}")
            if resp_p is None:
                continue
            content = resp.get("content") or {}
            mime = next((t for t in ("application/json", "application/problem+json") if "schema" in (content.get(t) or {})), None)
            if mime is None:
                fail(f"{where}: {op_id} response {status} has no application/json or application/problem+json schema")
                continue
            pointer = f"{resp_p}/content/{pointer_escape(mime)}/schema"
        if lang != "json":
            fail(f"{where}: a {mode} example must be a json block, found {lang or 'no language'!r}")
            continue
        try:
            value = json.loads(body)
        except json.JSONDecodeError as e:
            fail(f"{where}: example is not valid JSON: {e}")
            continue
        count += 1
        validator = Draft202012Validator({"$ref": f"{OPENAPI_URI}#{pointer}"}, registry=registry, format_checker=FormatChecker())
        for err in validator.iter_errors(value):
            fail(f"{where}: {op_id} {mode}: {err.message} at {err.json_path}")
    missing = sorted(set(ops) - covered)
    if missing:
        fail(f"{name} has no example marker for: {', '.join(missing)}")
    return count


AGENT_PLUGINS = ROOT / "internal" / "skillgen" / "testdata" / "agent-plugins" / "1.0.0"


def check_agent_plugin() -> None:
    # Here, not in Go: the plugin schema's name pattern has a lookahead, which Go's regexp lacks.
    pairs = (("plugins/agentfeedback/plugin.json", "plugin.schema.json"),
             ("internal/skillgen/testdata/agent-plugin-mcp.json", "mcp.schema.json"))
    versions = []
    for rel, schema_name in pairs:
        doc = load_json(ROOT / rel)
        for err in Draft202012Validator(load_json(AGENT_PLUGINS / schema_name), format_checker=FormatChecker()).iter_errors(doc):
            fail(f"{rel}: {err.message} at {err.json_path}")
        m = re.search(r"/schemas/([^/]+)/", doc.get("$schema", ""))
        versions.append(m.group(1) if m else None)
    if versions[0] is None or versions[0] != versions[1]:
        fail(f"the Agent Plugins spec versions of plugin.json and mcp.json differ: {versions}")


def main() -> int:
    schemas = {rel: load_json(ROOT / rel) for rel in SCHEMA_FILES}
    for rel, schema in schemas.items():
        try:
            Draft202012Validator.check_schema(schema)
        except Exception as e:  # jsonschema.SchemaError
            fail(f"{rel}: not a valid JSON Schema 2020-12 document: {e}")
        if schema.get("$schema") != "https://json-schema.org/draft/2020-12/schema":
            fail(f"{rel}: $schema must name draft 2020-12")
        if schema.get("$id") != f"https://agentfeedback.dev/{rel}":
            fail(f"{rel}: $id must be https://agentfeedback.dev/{rel}")
        walk_schema(schema, rel, lint_keywords)
    registry = registry_with_schemas(schemas)

    examples = 0
    for rel, schema in schemas.items():
        if not schema.get("examples"):
            fail(f"{rel}: no top-level examples")
        examples += validate_examples_in_schema(schema, rel, registry)

    doc = yaml.safe_load(OPENAPI.read_text(encoding="utf-8"))
    registry = registry.with_resource(OPENAPI_URI, Resource.from_contents(doc, default_specification=DRAFT202012))
    examples += validate_openapi_examples(doc, registry)
    referenced = set(re.findall(r"\.\./schemas/([\w./-]+\.json)", OPENAPI.read_text(encoding="utf-8")))
    for rel in SCHEMA_FILES:
        if rel.removeprefix("schemas/") not in referenced:
            fail(f"docs/openapi.yaml does not reference {rel}")

    warnings_doc = load_json(ROOT / "conformance" / "warnings.json")
    codes = {c["code"] for c in warnings_doc["codes"]}
    families = {c["code"] for c in warnings_doc["codes"] if c.get("family")}
    check_fixtures(schemas["schemas/envelope.v1.json"], registry, codes, families)
    check_agent_plugin()
    examples += check_api_examples(doc, registry, API_MD.read_text(encoding="utf-8"), "docs/api.md")

    sys.path.insert(0, str(ROOT / "conformance"))
    from reference import decode as ref_decode, guide as ref_guide, rawjson as ref_rawjson  # noqa: E402
    from reference.fixtures import run as run_fixtures  # noqa: E402

    if warnings_doc.get("keyword_codes") != ref_guide.KEYWORD_CODES:
        fail("conformance/warnings.json keyword_codes differs from the reference's table")
    for code in set(warnings_doc.get("keyword_codes", {}).values()):
        if code not in codes:
            fail(f"conformance/warnings.json keyword_codes names unknown code {code!r}")
    for rel, schema in schemas.items():
        if rel.startswith("schemas/kinds/"):
            try:
                ref_guide.check_schema(schema)
            except ValueError as e:
                fail(f"{rel}: {e}")

    manifest = load_json(ROOT / "conformance" / "manifest.json")
    for group in ("decode/rows", "decode/interactions", "hash"):
        on_disk = sorted(p.name for p in (ROOT / "conformance" / group).iterdir() if p.is_dir())
        listed = sorted(manifest.get(group, []))
        if on_disk != listed:
            fail(f"conformance/manifest.json {group}: listed {listed} but on disk {on_disk}")

    limits = doc["paths"]["/api/v1/meta"]["get"]["responses"]["200"]["content"]["application/json"]["example"]["limits"]
    for name, want in (("body_bytes", ref_decode.BODY_LIMIT), ("context_entries", ref_decode.CONTEXT_ENTRIES),
                       ("context_value_bytes", ref_decode.CONTEXT_VALUE_BYTES), ("summary_bytes", 2000), ("identifier_bytes", 200)):
        if limits.get(name) != want:
            fail(f"docs/openapi.yaml /meta example limits.{name} is {limits.get(name)}, the reference uses {want}")
    if manifest.get("max_depth") != ref_rawjson.MAX_DEPTH:
        fail("conformance/manifest.json max_depth differs from the reference")

    try:
        if run_fixtures() != 0:
            fail("conformance fixtures disagree with the reference implementation")
    except Exception as e:  # a schema the reference cannot apply, or a malformed fixture
        fail(f"reference implementation failed: {e!r}")

    if problems:
        for p in problems:
            print(f"contract-check: {p}", file=sys.stderr)
        print(f"contract-check: {len(problems)} problem(s)", file=sys.stderr)
        return 1
    print(f"contract-check: {len(schemas)} schemas, {examples} examples, warnings list, manifest, fixtures, Agent Plugins manifests and docs/api.md examples OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
