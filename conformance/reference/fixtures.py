"""Run every fixture under conformance/ through the reference implementation.

    python3 -m reference.fixtures            # from conformance/
    python3 conformance/reference/fixtures.py --write <fixture dir>   # (re)generate outputs

Exit status is non-zero when any fixture disagrees with the reference. The
``--write`` mode is for authoring: it prints what the reference produces for a
fixture's body so the author can check it by hand before committing it; it
never overwrites an existing expected.json or stored.json.
"""

from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
    from reference.canon import canonical_bytes  # type: ignore
    from reference.decode import decode, gen_body, load_kind_schemas, summarise  # type: ignore
else:
    from .canon import canonical_bytes
    from .decode import decode, gen_body, load_kind_schemas, summarise

ROOT = Path(__file__).resolve().parents[1]


def read_body(d: Path) -> bytes:
    body = d / "body.json"
    gen = d / "body.gen.json"
    if body.exists() == gen.exists():
        raise SystemExit(f"{d}: exactly one of body.json and body.gen.json is required")
    if body.exists():
        return body.read_bytes()
    return gen_body(json.loads(gen.read_text(encoding="utf-8")))


def check_decode(d: Path, schemas) -> list[str]:
    problems = []
    expected = json.loads((d / "expected.json").read_text(encoding="utf-8"))
    result = decode(read_body(d), schemas)
    got = summarise(result)
    if got.get("status") != expected.get("status") or got.get("error") != expected.get("error"):
        problems.append(f"status: expected {expected.get('status')} {expected.get('error') or ''}, reference gives {got.get('status')} {got.get('error') or ''}")
    if result.status == 201:
        want = sorted((w["code"], w["pointer"]) for w in expected.get("warnings", []))
        if want != result.warning_keys():
            problems.append(f"warnings: expected {want}, reference gives {result.warning_keys()}")
        stored = (d / "stored.json").read_bytes()
        mine = canonical_bytes(result.envelope)
        if stored != mine:
            problems.append(f"stored.json differs from the reference:\n  fixture:   {stored.decode('utf-8', 'replace')}\n  reference: {mine.decode('utf-8', 'replace')}")
    elif (d / "stored.json").exists():
        problems.append("stored.json present on a rejected body")
    return problems


def check_hash(d: Path, schemas) -> list[str]:
    problems = []
    expected = json.loads((d / "expected.json").read_text(encoding="utf-8"))
    result = decode(read_body(d), schemas)
    if result.status != 201:
        return [f"body rejected with {result.status}"]
    canonical = (d / "canonical.json").read_bytes()
    mine = canonical_bytes(result.hash_input)
    if canonical != mine:
        problems.append(f"canonical.json differs from the reference:\n  fixture:   {canonical.decode('utf-8', 'replace')}\n  reference: {mine.decode('utf-8', 'replace')}")
    digest = hashlib.sha256(canonical).hexdigest()
    if expected.get("sha256") != digest:
        problems.append(f"sha256: expected.json says {expected.get('sha256')}, canonical.json hashes to {digest}")
    if result.content_hash != digest:
        problems.append(f"reference hash {result.content_hash} differs from canonical.json hash {digest}")
    return problems


def run(root: Path = ROOT) -> int:
    schemas = load_kind_schemas()
    failures = 0
    count = 0
    for group, checker in (("decode/rows", check_decode), ("decode/interactions", check_decode), ("hash", check_hash)):
        dirs = sorted(p for p in (root / group).iterdir() if p.is_dir())
        if not dirs:
            print(f"conformance/{group}: no fixtures", file=sys.stderr)
            failures += 1
        for d in dirs:
            count += 1
            problems = checker(d, schemas)
            if problems:
                failures += 1
                print(f"FAIL conformance/{group}/{d.name}", file=sys.stderr)
                for p in problems:
                    print(f"  {p}", file=sys.stderr)
    print(f"conformance: {count} fixtures, {failures} failing")
    return 1 if failures else 0


def write_preview(d: Path) -> None:
    schemas = load_kind_schemas()
    result = decode(read_body(d), schemas)
    print("expected.json:", json.dumps(summarise(result), indent=2))
    if result.status == 201:
        print("stored.json:", canonical_bytes(result.envelope).decode("utf-8"))
        print("canonical (hash input):", canonical_bytes(result.hash_input).decode("utf-8"))
        print("sha256:", result.content_hash)
        for w in result.warnings:
            print(f"  {w.code} {w.pointer}: {w.message}")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--write":
        write_preview(Path(sys.argv[2]))
    else:
        sys.exit(run())
