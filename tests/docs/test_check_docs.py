#!/usr/bin/env python3
"""Tests for scripts/check-docs.py: slugs, code skipping, link and anchor
resolution, the route tables. Fixture repositories are temporary
directories; no git.

Usage: python3 tests/docs/test_check_docs.py
"""

import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("check_docs", ROOT / "scripts/check-docs.py")
cd = importlib.util.module_from_spec(spec)
sys.modules["check_docs"] = cd
spec.loader.exec_module(cd)

TABLE = """# Repo

## Start here

| Task | Doc |
|---|---|
| Use | [docs/api.md](docs/api.md) |

## Route by task

| Task | Doc |
|---|---|
| Use | [api](docs/api.md) |
"""


class Repo:
    """A fixture repository: files written under a temporary root."""

    def __init__(self, files: dict[str, str]):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        files = {"README.md": TABLE, "AGENTS.md": TABLE, "docs/api.md": "# API\n", **files}
        for rel, text in files.items():
            p = self.root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(text, encoding="utf-8")
        self.files = sorted(files)

    def check(self) -> list[str]:
        md = [f for f in self.files if f.endswith(".md")]
        problems, _ = cd.check(self.root, md, self.files)
        return problems

    def close(self):
        self._tmp.cleanup()


class SlugTest(unittest.TestCase):
    def test_punctuation(self):
        self.assertEqual(cd.slug("Run, deploy & back up!"), "run-deploy--back-up")
        self.assertEqual(cd.slug("What's new? (v4.0.0)"), "whats-new-v400")

    def test_backticks_and_markup(self):
        self.assertEqual(cd.slug("The `AF_PLAYBOOK_*` inputs"), "the-af_playbook_-inputs")
        self.assertEqual(cd.slug("**Bold** and _em_ [link](x.md)"), "bold-and-em-link")
        self.assertEqual(cd.slug("snake_case <code>tag</code>"), "snake_case-tag")

    def test_unicode(self):
        self.assertEqual(cd.slug("Café — Über"), "café--über")

    def test_duplicates(self):
        a = cd.anchors("# Setup\n\n## Setup\n\n### Setup\n\nTitle\n=====\n")
        self.assertEqual(a, {"setup", "setup-1", "setup-2", "title"})

    def test_duplicates_skip_taken_suffix(self):
        a = cd.anchors("# Setup\n\n# Setup-1\n\n# Setup\n")
        self.assertEqual(a, {"setup", "setup-1", "setup-2"})

    def test_front_matter_skipped(self):
        text = "---\nname: x\nlink: \"[a](missing.md)\"\n---\n\n# Real\n"
        self.assertEqual(cd.anchors(text), {"real"})
        self.assertEqual(cd.links(cd.strip_code(text)), [])

    def test_explicit_ids(self):
        a = cd.anchors('<a id="Custom"></a>\n<a name="old-name"></a>\n<div id="box"></div>\n')
        self.assertEqual(a, {"custom", "old-name", "box"})


class CodeSkipTest(unittest.TestCase):
    def test_fences_and_inline(self):
        text = "a\n```sh\n[x](missing.md)\n```\n~~~\n[y](gone.md)\n~~~\n`[z](nope.md)` [ok](real.md)\n"
        self.assertEqual([t for _, t in cd.links(cd.strip_code(text))], ["real.md"])

    def test_heading_in_fence_is_not_an_anchor(self):
        self.assertEqual(cd.anchors("```\n# not a heading\n```\n# Real\n"), {"real"})

    def test_comment_opener_in_code_span(self):
        text = "Use `<!--` to open.\n\n[bad](missing.md)\n\n-->\n"
        self.assertEqual(cd.links(cd.strip_code(text)), [(3, "missing.md")])

    def test_comment_opener_in_fence(self):
        text = "```\n<!--\n```\n[bad](missing.md)\n-->\n"
        self.assertEqual(cd.links(cd.strip_code(text)), [(4, "missing.md")])

    def test_line_numbers_kept(self):
        text = "<!--\n[c](c.md)\n-->\n```\nx\n```\n[l](l.md)\n"
        self.assertEqual(cd.links(cd.strip_code(text)), [(7, "l.md")])


class LinkTest(unittest.TestCase):
    def run_repo(self, files):
        repo = Repo(files)
        self.addCleanup(repo.close)
        return repo.check()

    def test_schemes_skipped(self):
        self.assertEqual(self.run_repo({"x.md": "[a](https://e.com/x) [b](mailto:a@b) [c](//cdn/x) <a href=\"http://x\">x</a>\n"}), [])

    def test_broken_file(self):
        self.assertEqual(self.run_repo({"guide/x.md": "Intro\n\n[a](../missing.md)\n"}),
                         ["guide/x.md:3: broken link ../missing.md"])

    def test_outside_repo(self):
        self.assertEqual(self.run_repo({"x.md": "[a](../../etc/passwd)\n"}),
                         ["x.md:1: broken link ../../etc/passwd"])

    def test_dotdot_prefixed_name_inside_repo(self):
        self.assertEqual(self.run_repo({"..notes.md": "# N\n", "x.md": "[a](..notes.md)\n"}), [])

    def test_broken_link_after_code_span_comment_opener(self):
        self.assertEqual(self.run_repo({"x.md": "Use `<!--` here.\n\n[a](missing.md)\n"}),
                         ["x.md:3: broken link missing.md"])

    def test_good_links(self):
        files = {"guide/x.md": "[a](../docs/api.md) [b](/README.md) [c](<my file.md> \"t\") ![i](../README.md)\n[ref]: ./my%20file.md\n<img src=\"../docs/api.md\">\n[d](my%20file.md)\n",
                 "guide/my file.md": "# Hi\n"}
        self.assertEqual(self.run_repo(files), [])

    def test_broken_anchor(self):
        self.assertEqual(self.run_repo({"x.md": "# Top\n\n[a](docs/api.md#nope) [b](docs/api.md#API)\n"}),
                         ["x.md:3: broken anchor docs/api.md#nope"])

    def test_same_file_anchor(self):
        self.assertEqual(self.run_repo({"x.md": "# Top\n\n## Next step\n\n[a](#next-step) [b](#missing)\n"}),
                         ["x.md:5: broken anchor #missing"])

    def test_explicit_id_anchor(self):
        self.assertEqual(self.run_repo({"x.md": '<a id="pin"></a>\n\n[a](#pin)\n'}), [])

    def test_non_markdown_fragment_unchecked(self):
        self.assertEqual(self.run_repo({"spec.yaml": "x: 1\n", "x.md": "[a](spec.yaml#/paths/x)\n"}), [])


class RouteTableTest(unittest.TestCase):
    def test_missing_doc(self):
        repo = Repo({"docs/operate.md": "# Operate\n", "llms.txt": "x\n", "docs/recipes/curl.md": "# c\n",
                     "docs/sub/deep.md": "# not required\n"})
        self.addCleanup(repo.close)
        self.assertEqual(repo.check(), [
            'README.md: route table "Start here" does not name docs/operate.md',
            'README.md: route table "Start here" does not name docs/recipes/curl.md',
            'README.md: route table "Start here" does not name llms.txt',
            'AGENTS.md: route table "Route by task" does not name docs/operate.md',
            'AGENTS.md: route table "Route by task" does not name docs/recipes/curl.md',
            'AGENTS.md: route table "Route by task" does not name llms.txt',
        ])

    def test_missing_table(self):
        repo = Repo({"AGENTS.md": "# Agents\n\n## Route by task\n\nNo table.\n"})
        self.addCleanup(repo.close)
        self.assertEqual(repo.check(), ['AGENTS.md: no route table under "## Route by task"'])


if __name__ == "__main__":
    unittest.main()
