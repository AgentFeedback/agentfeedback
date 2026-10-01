#!/usr/bin/env python3
"""Measure agentfeedback-triage cluster.py against labelled friction pairs. LIVE.

Sends report text (category, summary, details, suggested_fix) to TypeSafe.
Every labelled report's exact payload.context.git_remote must be approved with
--allow-repo, as in cluster.py; without --live it only lists what would be sent.

  python3 scripts/eval-cluster.py export.ndjson labels.json --allow-repo REMOTE ...
  TYPESAFE_API_KEY=... python3 scripts/eval-cluster.py export.ndjson labels.json \
      --allow-repo REMOTE ... --live [--out raw.json]

export.ndjson is `agentfeedback export --kind friction`. labels.json:
  {"chunks": [[id, ...], ...],        # every pair inside a chunk is evaluated
   "same": [[id, id], ...],           # labelled same defect; all other pairs are different
   "compound": [[id, id], ...],       # subset of "same" where one report bundles several defects
   "instructions": {"name": {"instructions": "...", "criteria": {...}}}}  # optional variants
The shipped instructions are always evaluated as "shipped". Each variant runs
per pair (one request each) and batched (one request per chunk).
"""
import argparse
import importlib.util
import itertools
import json
import os
from pathlib import Path
import sys

sys.dont_write_bytecode = True
SCRIPT = Path(__file__).resolve().parents[1] / "skills/agentfeedback-triage/scripts/cluster.py"
spec = importlib.util.spec_from_file_location("cluster", SCRIPT)
cluster = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cluster)
THRESHOLDS = (0.5, 0.6, 0.7, 0.8, 0.9)


def same_at(result, threshold):
    return (result.get("status") == "completed" and result["choice"] == "same"
            and result["confidence"] >= threshold and result["probabilities"]["same"] >= threshold)


def score(results, positives, exclude=frozenset()):
    rows = []
    for threshold in THRESHOLDS:
        tp = fp = fn = 0
        for pair, result in results.items():
            if pair in exclude:
                continue
            predicted, actual = same_at(result, threshold), pair in positives
            tp += predicted and actual
            fp += predicted and not actual
            fn += actual and not predicted
        rows.append({"threshold": threshold, "tp": tp, "fp": fp, "fn": fn,
                     "precision": round(tp / (tp + fp), 3) if tp + fp else None,
                     "recall": round(tp / (tp + fn), 3) if tp + fn else None})
    return rows


def p_same(result):
    return result["probabilities"]["same"] if result.get("status") == "completed" else None


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("export", type=Path)
    parser.add_argument("labels", type=Path)
    parser.add_argument("--allow-repo", action="append", default=[],
                        help="approve disclosure for an exact payload.context.git_remote; repeat per repository")
    parser.add_argument("--live", action="store_true", help="send requests; otherwise list the disclosure and exit")
    parser.add_argument("--out", type=Path, help="write every raw answer here")
    parser.add_argument("--timeout", type=int, default=30)
    args = parser.parse_args()
    labels = json.loads(args.labels.read_text())
    records = {}
    for line in args.export.read_text().splitlines():
        record = json.loads(line)
        if isinstance(record, dict) and record.get("family") == "friction" and "id" in record:
            records[record["id"]] = record
    absent = sorted({report_id for chunk in labels["chunks"] for report_id in chunk} - set(records))
    if absent:
        parser.error(f"labelled ids missing from the export: {absent}")
    chunks = [[records[report_id] for report_id in sorted(chunk)] for chunk in labels["chunks"]]
    remotes = {}
    for row in (row for chunk in chunks for row in chunk):
        remotes.setdefault(cluster.repository(row), []).append(row["id"])
    unapproved = {remote: ids for remote, ids in remotes.items() if not remote or remote not in args.allow_repo}
    if unapproved or not args.live:
        print(json.dumps({"disclosure": remotes, "unapproved": unapproved}, indent=2))
        if unapproved:
            parser.error("every labelled report's exact git_remote needs --allow-repo; nothing was sent")
        return
    key = os.environ.get("TYPESAFE_API_KEY", "")
    if not key:
        parser.error("TYPESAFE_API_KEY is not set")
    pair_key = lambda a, b: (min(a, b), max(a, b))
    positives = {pair_key(*pair) for pair in labels["same"]}
    compound = {pair_key(*pair) for pair in labels.get("compound", [])}
    pairs = sorted({(left["id"], right["id"]) for chunk in chunks for left, right in itertools.combinations(chunk, 2)})
    missing = positives - set(pairs)
    if missing:
        parser.error(f"labelled pairs outside every chunk: {sorted(missing)}")
    if "shipped" in labels.get("instructions", {}):
        parser.error('"shipped" is reserved for the instructions cluster.py ships')
    variants = {"shipped": {"instructions": cluster.INSTRUCTIONS, "criteria": dict(cluster.CRITERIA)},
                **labels.get("instructions", {})}
    report, raw = {"pairs": len(pairs), "positives": len(positives), "compound": len(compound),
                   "chunks": [len(chunk) for chunk in chunks], "conditions": {}}, {}
    for name, variant in variants.items():
        cluster.INSTRUCTIONS, cluster.CRITERIA = variant["instructions"], variant["criteria"]
        by_row = {row["id"]: row for chunk in chunks for row in chunk}
        single = {pair: cluster.compare(by_row[pair[0]], by_row[pair[1]], key, args.timeout) for pair in pairs}
        batched = {}
        for chunk in chunks:
            chunk_pairs = [(left["id"], right["id"]) for left, right in itertools.combinations(chunk, 2)]
            batched.update(cluster.compare_chunk(chunk, chunk_pairs, key, args.timeout))
        for mode, results in (("pair", single), ("batch", batched)):
            unassessed = sorted({result.get("reason") for result in results.values() if result.get("status") != "completed"})
            positive_p = [p_same(results[pair]) for pair in sorted(positives)]
            negative_p = [p_same(results[pair]) for pair in pairs if pair not in positives]
            report["conditions"][f"{name}/{mode}"] = {
                "unassessed": sum(result.get("status") != "completed" for result in results.values()),
                "unassessed_reasons": unassessed,
                "all": score(results, positives),
                "without_compound": score(results, positives - compound, exclude=compound),
                "positive_p_same": dict(zip(map(str, sorted(positives)), positive_p)),
                "max_negative_p_same": max((p for p in negative_p if p is not None), default=None),
                "negatives_p_same_ge_0.5": sum(p is not None and p >= 0.5 for p in negative_p),
            }
            raw[f"{name}/{mode}"] = {f"{a}-{b}": results[(a, b)] for a, b in pairs}
        both = [pair for pair in pairs if single[pair].get("status") == batched[pair].get("status") == "completed"]
        agree = [same_at(single[pair], cluster.THRESHOLD) == same_at(batched[pair], cluster.THRESHOLD) for pair in both]
        deltas = [abs(p_same(single[pair]) - p_same(batched[pair])) for pair in pairs
                  if p_same(single[pair]) is not None and p_same(batched[pair]) is not None]
        report["conditions"][f"{name}/agreement"] = {
            "pairs_completed_in_both": len(agree),
            "suggestion_agreement_at_threshold": round(sum(agree) / len(agree), 3) if agree else None,
            "mean_abs_delta_p_same": round(sum(deltas) / len(deltas), 3) if deltas else None,
            "max_abs_delta_p_same": round(max(deltas), 3) if deltas else None,
        }
    if args.out:
        args.out.write_text(json.dumps(raw, indent=1))
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
