#!/usr/bin/env python3
# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "jsonschema==4.26.0",
#   "pyyaml==6.0.3",
#   "rfc3339-validator==0.1.4",
# ]
# ///
"""Tests for check_api_examples in scripts/contract-check.py: markers, fences,
request/response validation, coverage. Inline OpenAPI document; no repo files.

Usage: uv run --locked --script tests/docs/test_api_examples.py
"""

import copy
import importlib.util
import sys
import unittest
from pathlib import Path

from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("contract_check", ROOT / "scripts/contract-check.py")
cc = importlib.util.module_from_spec(spec)
sys.modules["contract_check"] = cc
spec.loader.exec_module(cc)

DOC = {
    "openapi": "3.1.0",
    "paths": {
        "/things": {
            "post": {
                "operationId": "createThing",
                "requestBody": {"content": {"application/json": {"schema": {
                    "type": "object", "required": ["name"], "additionalProperties": False,
                    "properties": {"name": {"type": "string"}}}}}},
                "responses": {"200": {"$ref": "#/components/responses/Thing"}},
            },
        },
        "/other": {"get": {"operationId": "getOther", "responses": {"200": {"description": "ok"}}}},
    },
    "components": {"responses": {"Thing": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object", "required": ["id"], "properties": {"id": {"type": "integer"}}}}}}}},
}

OTHER = "<!-- example: getOther -->\n```sh\ncurl x\n```\n"
REQ = '<!-- example: createThing request -->\n```json\n{"name": "a"}\n```\n'
RESP = '<!-- example: createThing response 200 -->\n\n```json\n{"id": 1}\n```\n'


def registry(doc):
    return Registry().with_resource(cc.OPENAPI_URI, Resource.from_contents(doc, default_specification=DRAFT202012))


class ApiExamplesTest(unittest.TestCase):
    def run_check(self, text, doc=DOC):
        cc.problems.clear()
        count = cc.check_api_examples(doc, registry(doc), text, "api.md")
        out = list(cc.problems)
        cc.problems.clear()
        return count, out

    def test_valid_examples_pass_and_count(self):
        self.assertEqual(self.run_check(REQ + RESP + OTHER), (2, []))

    def test_invalid_json(self):
        count, out = self.run_check('<!-- example: createThing request -->\n```json\n{"name": \n```\n' + OTHER)
        self.assertEqual(count, 0)
        self.assertEqual(len(out), 1)
        self.assertTrue(out[0].startswith("api.md:1: example is not valid JSON"), out)

    def test_schema_violation(self):
        count, out = self.run_check('<!-- example: createThing response 200 -->\n```json\n{"id": "x"}\n```\n' + OTHER)
        self.assertEqual(len(out), 1)
        self.assertTrue(out[0].startswith("api.md:1: createThing response 200: 'x' is not of type"), out)

    def test_unknown_operation(self):
        _, out = self.run_check(REQ + OTHER + "<!-- example: nope -->\n```sh\nx\n```\n")
        self.assertEqual(out, ["api.md:9: example marker names 'nope', which docs/openapi.yaml does not define"])

    def test_missing_marker(self):
        _, out = self.run_check(REQ)
        self.assertEqual(out, ["api.md has no example marker for: getOther"])

    def test_bare_marker_is_coverage_only(self):
        self.assertEqual(self.run_check("<!-- example: createThing -->\n```text\nnot json\n```\n" + OTHER), (0, []))

    def test_malformed_marker(self):
        _, out = self.run_check(REQ + OTHER + "<!-- example createThing request -->\n```json\n{}\n```\n")
        self.assertEqual(len(out), 1)
        self.assertTrue(out[0].startswith("api.md:9: malformed example marker"), out)

    def test_unclosed_fence(self):
        _, out = self.run_check(OTHER + REQ + '<!-- example: createThing response 200 -->\n```json\n{"id": 1}\n')
        self.assertEqual(out, ["api.md:10: example block is not closed"])

    def test_marker_without_fence(self):
        _, out = self.run_check(REQ + "<!-- example: getOther -->\n\nprose\n")
        self.assertEqual(out, ["api.md:5: example marker is not followed by a fenced code block",
                               "api.md has no example marker for: getOther"])

    def test_text_between_marker_and_fence(self):
        _, out = self.run_check(REQ + "<!-- example: getOther -->\ntext\n```sh\nx\n```\n")
        self.assertEqual(out, ["api.md:5: example marker is not followed by a fenced code block",
                               "api.md has no example marker for: getOther"])

    def test_bad_ref(self):
        doc = copy.deepcopy(DOC)
        doc["paths"]["/things"]["post"]["responses"]["200"] = {"$ref": "#/components/responses/Missing"}
        _, out = self.run_check(RESP + OTHER, doc)
        self.assertEqual(out, ["api.md:1: createThing response 200: $ref #/components/responses/Missing does not resolve"])

    def test_marker_inside_fence_is_not_read(self):
        text = "````md\n<!-- example: createThing request -->\n```json\n{\"bad\": 1}\n```\n````\n"
        _, out = self.run_check(text + OTHER)
        self.assertEqual(out, ["api.md has no example marker for: createThing"])


if __name__ == "__main__":
    unittest.main()
