"""Check client migration fixture inputs; the CLI consumer checks network behavior."""
from __future__ import annotations

import json
from pathlib import Path

from .decode import decode


def check(d: Path, schemas) -> list[str]:
    fixture = json.loads((d / "scenario.json").read_text(encoding="utf-8"))
    problems = []
    for index, body in enumerate(fixture["seed"]):
        if decode(json.dumps(body).encode(), schemas).status != 201:
            problems.append(f"seed {index}: reference rejects source body")
    for case in fixture["cases"]:
        if case["code"] != 0:
            continue  # HTTP fault injection is exercised by the CLI consumer.
        args = case["args"]
        include = any(args[i:i + 2] == ["--include-kind", "install-check"] for i in range(len(args)))
        limit = int(args[args.index("--limit") + 1]) if "--limit" in args else 0
        seeds = [fixture["seed"][i] for i in case["seed_order"]] if "seed_order" in case else fixture["seed"]
        selected = []
        excluded = 0
        for record in seeds:
            if not include and record["kind"] == "install-check":
                excluded += 1
                continue
            selected.append(record)
            if limit and len(selected) >= limit:
                break
        if "--dry-run" in args:
            expected = {"outcome": "dry_run", "sent": 0, "would_send": len(selected),
                        "excluded": excluded, "unredacted_tombstones": len(case.get("redact_after_migration", []))}
            summaries = [r["summary"] for r in selected] if case.get("redact_after_migration") else []
        else:
            expected = {"outcome": "migrated", "sent": len(selected), "imported": len(selected),
                        "skipped": 0, "conflicts": 0, "excluded": excluded}
            summaries = [r["summary"] for r in selected]
        if case["outcome"] != expected:
            problems.append(f"{case['name']}: migration counts disagree with selected records")
        if case["target_summaries"] != summaries:
            problems.append(f"{case['name']}: target differs from selected records")
    return problems
