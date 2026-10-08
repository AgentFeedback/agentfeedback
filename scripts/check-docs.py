#!/usr/bin/env python3
"""Check the Markdown files' links and the route tables.

    python3 scripts/check-docs.py

The files are every *.md file git tracks or would track (untracked but not
ignored). YAML front matter, fenced code blocks, inline code spans and HTML
comments (outside fences and code spans) are not read.

1. Every relative link target resolves: inline links and images, reference
   definitions and HTML href/src attributes. The path is URL-decoded and
   resolved against the linking file's directory (a leading / is the
   repository root); it must exist and stay inside the repository. Targets
   with a URL scheme (http:, https:, mailto:, ...) or a leading // are not
   checked.
2. Every fragment on a link to a Markdown file (a bare #fragment is the same
   file) names a heading slug, by GitHub's rules, or an explicit id/name
   attribute in that file. Fragments are compared lowercased. Fragments on
   other targets (docs/openapi.yaml#...) are not checked.
3. The route table under "## Start here" in README.md and the one under
   "## Route by task" in AGENTS.md each name every document: docs/*.md,
   docs/recipes/*.md, docs/openapi.yaml, AGENT-INSTALL.md,
   AGENT-INSTALL-STACK.md and llms.txt (those that exist).

Problems go to stderr, one per line, and the exit status is 1.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import unicodedata
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parents[1]

ROUTE_TABLES = (("README.md", "Start here"), ("AGENTS.md", "Route by task"))
REQUIRED_FIXED = ("docs/openapi.yaml", "AGENT-INSTALL.md", "AGENT-INSTALL-STACK.md", "llms.txt")

_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
_CODE_SPAN = re.compile(r"(`+)(.+?)(?<!`)\1(?!`)")
_REF_DEF = re.compile(r"^ {0,3}\[([^\]]+)\]:[ \t]*(<[^>]*>|\S+)")
_HTML_ATTR = re.compile(r"""\b(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')""", re.I)
_ID_ATTR = re.compile(r"""\b(?:id|name)\s*=\s*(?:"([^"]*)"|'([^']*)')""", re.I)
_ATX = re.compile(r"^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$")
_SETEXT = re.compile(r"^ {0,3}(=+|-+)[ \t]*$")
_SCHEME = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")


def git_files(root: Path, *pathspec: str) -> list[str]:
    out = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", *pathspec],
                         cwd=root, check=True, capture_output=True).stdout
    return sorted({p for p in out.decode("utf-8").split("\0") if p and (root / p).exists()})


def _front_matter_end(lines: list[str]) -> int:
    """Index of the line closing YAML front matter, or -1 when there is none."""
    if not lines or lines[0].rstrip() != "---":
        return -1
    for j in range(1, len(lines)):
        if lines[j].rstrip() in ("---", "..."):
            return j
    return -1


def _strip_line(line: str, in_comment: bool) -> tuple[str, bool]:
    """One line outside fences with inline code spans and HTML comments
    blanked, whichever starts first winning; returns the comment state."""
    out: list[str] = []
    i = 0
    while i < len(line):
        if in_comment:
            end = line.find("-->", i)
            if end == -1:
                out.append(" " * (len(line) - i))
                break
            out.append(" " * (end + 3 - i))
            i = end + 3
            in_comment = False
            continue
        c = line.find("<!--", i)
        m = _CODE_SPAN.search(line, i)
        if m and (c == -1 or m.start() < c):
            out.append(line[i:m.start()])
            out.append(" " * len(m.group(0)))
            i = m.end()
            continue
        if c == -1:
            out.append(line[i:])
            break
        out.append(line[i:c] + "    ")
        i = c + 4
        in_comment = True
    return "".join(out), in_comment


def strip_code(text: str) -> list[str]:
    """The file's lines with YAML front matter, fenced blocks, HTML comments
    and inline code blanked out; line numbers are preserved. Comments are
    only recognised outside fences and code spans."""
    lines = text.split("\n")
    out: list[str] = []
    fm = _front_matter_end(lines)
    fence: str | None = None
    in_comment = False
    for idx, line in enumerate(lines):
        if idx <= fm:
            out.append("")
            continue
        m = _FENCE.match(line)
        if fence is None:
            if not in_comment and m and not (m.group(1)[0] == "`" and "`" in m.group(2)):
                fence = m.group(1)
                out.append("")
                continue
            stripped, in_comment = _strip_line(line, in_comment)
            out.append(stripped)
        else:
            if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence) and not m.group(2).strip():
                fence = None
            out.append("")
    return out


def _inline_targets(line: str) -> list[str]:
    """Targets of [text](target) and ![alt](target) on one line."""
    targets = []
    i = line.find("](")
    while i != -1:
        j = i + 2
        while j < len(line) and line[j] in " \t":
            j += 1
        if j < len(line) and line[j] == "<":
            end = line.find(">", j)
            if end != -1:
                targets.append(line[j + 1:end])
        else:
            depth, k = 0, j
            while k < len(line):
                ch = line[k]
                if ch == "(":
                    depth += 1
                elif ch == ")":
                    if depth == 0:
                        break
                    depth -= 1
                elif ch in " \t":
                    break
                k += 1
            if k > j:
                targets.append(line[j:k])
        i = line.find("](", i + 2)
    return targets


def links(lines: list[str]) -> list[tuple[int, str]]:
    """(1-based line, target) for every link in code-stripped lines."""
    found = []
    for n, line in enumerate(lines, 1):
        for t in _inline_targets(line):
            found.append((n, t))
        m = _REF_DEF.match(line)
        if m:
            t = m.group(2)
            found.append((n, t[1:-1] if t.startswith("<") else t))
        for m in _HTML_ATTR.finditer(line):
            found.append((n, m.group(1) if m.group(1) is not None else m.group(2)))
    return found


def _strip_inline(text: str) -> str:
    """Heading text as GitHub renders it, before slugging."""
    parts = re.split(r"(`+[^`]*?`+)", text)
    out = []
    for p in parts:
        if p.startswith("`"):
            out.append(p.strip("`"))
            continue
        p = re.sub(r"!\[([^\]]*)\]\([^)]*\)", r"\1", p)
        p = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", p)
        p = re.sub(r"\[([^\]]*)\]\[[^\]]*\]", r"\1", p)
        p = re.sub(r"<[^>]+>", "", p)
        p = p.replace("*", "")
        p = re.sub(r"(?<![^\W_])_+|_+(?![^\W_])", "", p)
        out.append(p)
    return "".join(out)


def slug(text: str) -> str:
    """GitHub's heading anchor, without the duplicate suffix."""
    text = _strip_inline(text).strip().lower()
    kept = "".join(c for c in text if c in " -" or unicodedata.category(c)[0] in "LMN" or unicodedata.category(c) == "Pc")
    return kept.replace(" ", "-")


def headings(raw_lines: list[str], lines: list[str]) -> list[str]:
    """Heading texts in document order, from code-stripped lines (raw lines
    give the text, so code spans in a heading keep their content)."""
    found = []
    for i, line in enumerate(lines):
        m = _ATX.match(line)
        if m:
            found.append((m.group(2) or "", i))
            continue
        if i > 0 and _SETEXT.match(line) and lines[i - 1].strip() and not _ATX.match(lines[i - 1]) \
                and not lines[i - 1].lstrip().startswith(("|", "- ", "* ", "+ ", ">")):
            found.append((lines[i - 1].strip(), i - 1))
    out = []
    for text, i in found:
        m = _ATX.match(raw_lines[i])
        out.append((m.group(2) or "") if m else raw_lines[i].strip())
    return out


def anchors(text: str) -> set[str]:
    """Every fragment a link may name in this Markdown text, lowercased."""
    raw = text.split("\n")
    lines = strip_code(text)
    result: set[str] = set()
    slugs: set[str] = set()
    for h in headings(raw, lines):
        base = s = slug(h)
        n = 0
        while s in slugs:
            n += 1
            s = f"{base}-{n}"
        slugs.add(s)
        result.add(s)
    for line in lines:
        for m in _ID_ATTR.finditer(line):
            result.add((m.group(1) if m.group(1) is not None else m.group(2)).lower())
    return result


def _resolve(root: Path, src: str, path: str) -> str | None:
    """The target path relative to root, or None when it leaves the repository."""
    base = "" if path.startswith("/") else os.path.dirname(src)
    rel = os.path.normpath(os.path.join(base, path.lstrip("/")))
    if rel == ".":
        return ""
    if os.path.isabs(rel) or Path(rel).parts[0] == "..":
        return None
    return rel


def route_table(lines: list[str], heading: str) -> list[str] | None:
    """The table rows under '## heading' (code-stripped lines), or None."""
    start = None
    for i, line in enumerate(lines):
        if line.strip() == f"## {heading}":
            start = i + 1
            break
    if start is None:
        return None
    rows: list[str] = []
    for line in lines[start:]:
        if _ATX.match(line):
            break
        if line.lstrip().startswith("|"):
            rows.append(line)
        elif rows:
            break
    return rows or None


def required_docs(root: Path, all_files: list[str]) -> list[str]:
    req = [f for f in all_files if re.fullmatch(r"docs/[^/]+\.md|docs/recipes/[^/]+\.md", f)]
    req += [f for f in REQUIRED_FIXED if (root / f).exists()]
    return sorted(set(req))


def check(root: Path, md_files: list[str], all_files: list[str]) -> tuple[list[str], int]:
    problems: list[str] = []
    cache: dict[str, set[str]] = {}

    def anchors_of(rel: str) -> set[str]:
        if rel not in cache:
            cache[rel] = anchors((root / rel).read_text(encoding="utf-8"))
        return cache[rel]

    count = 0
    for src in md_files:
        text = (root / src).read_text(encoding="utf-8")
        lines = strip_code(text)
        for n, target in links(lines):
            target = target.strip()
            if not target or target.startswith("//") or _SCHEME.match(target):
                continue
            count += 1
            path, _, frag = target.partition("#")
            path = path.split("?", 1)[0]
            if path:
                rel = _resolve(root, src, unquote(path))
                if rel is None or not (root / rel).exists():
                    problems.append(f"{src}:{n}: broken link {target}")
                    continue
            else:
                rel = src
            if frag and rel.endswith(".md") and (root / rel).is_file():
                if unquote(frag).lower() not in anchors_of(rel):
                    problems.append(f"{src}:{n}: broken anchor {target}")

    required = required_docs(root, all_files)
    for rel_file, heading in ROUTE_TABLES:
        if not (root / rel_file).exists():
            problems.append(f"{rel_file}: missing (it carries the route table \"{heading}\")")
            continue
        rows = route_table(strip_code((root / rel_file).read_text(encoding="utf-8")), heading)
        if rows is None:
            problems.append(f"{rel_file}: no route table under \"## {heading}\"")
            continue
        named = set()
        for _, target in links(rows):
            path = target.partition("#")[0].split("?", 1)[0]
            if not path or path.startswith("//") or _SCHEME.match(path):
                continue
            rel = _resolve(root, rel_file, unquote(path))
            if rel is not None:
                named.add(rel)
        for doc in required:
            if doc not in named:
                problems.append(f"{rel_file}: route table \"{heading}\" does not name {doc}")
    return problems, count


def main() -> int:
    md_files = git_files(ROOT, "*.md")
    all_files = git_files(ROOT)
    problems, count = check(ROOT, md_files, all_files)
    if problems:
        for p in problems:
            print(p, file=sys.stderr)
        print(f"check-docs: {len(problems)} problem(s)", file=sys.stderr)
        return 1
    print(f"check-docs: {len(md_files)} files, {count} links, route tables complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())
