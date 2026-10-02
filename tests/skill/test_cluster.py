#!/usr/bin/env python3
"""Disclosure boundaries and advisory failure behavior; no external requests."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
SCRIPT = Path(__file__).resolve().parents[2] / "skills/agentfeedback-triage/scripts/cluster.py"
spec = importlib.util.spec_from_file_location("cluster", SCRIPT)
cluster = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cluster)


def report(report_id, remote="repo-a"):
    return {"id": report_id, "uid": "PRIVATE_UID", "kind": "friction", "schema_version": 1,
            "summary": "broken flag", "machine": "PRIVATE_MACHINE", "project": "PRIVATE_PROJECT",
            "context": {"git_remote": remote, "repo_root": "PRIVATE_PATH"},
            "payload": {"category": "tooling", "details": "specific failure", "suggested_fix": "fix it",
                        "severity": "PRIVATE_SEVERITY"},
            "content_hash": "PRIVATE_HASH"}


def response(choice="same", confidence=0.9):
    return {"answers": {"mechanism": {
        "type": "choice", "choice": choice, "confidence": confidence,
        "probabilities": {key: 0.9 if key == choice else 0.05 for key in cluster.CRITERIA},
    }}}


def completed(choice="same", confidence=0.9):
    answer = response(choice, confidence)["answers"]["mechanism"]
    return {"status": "completed", **answer, "suggestion": choice if confidence >= 0.8 else "uncertain"}


class Clustering(unittest.TestCase):
    def invoke(self, rows, allowed, key="testkey", limit=200, preview=False, batch=1):
        return cluster.run(rows, "digest-sha", set(allowed), key, limit, 1, preview, batch)

    def test_no_implicit_disclosure(self):
        rows = [report(1), report(2), report(3, "repo-b")]
        with patch.object(cluster, "compare") as compare:
            for allowed, key, limit in [((), "key", 200), (("repo-a",), "", 200),
                                        (("repo-a", "repo-b"), "key", 1)]:
                result = self.invoke(rows, allowed, key, limit)
                self.assertEqual(result["status"], "skipped")
                self.assertEqual(result["groups"], [[1], [2], [3]])
            preview = self.invoke(rows, ("repo-a",), preview=True)
            self.assertEqual([row["id"] for row in preview["disclosure"]], [1, 2])
            self.assertNotIn("PRIVATE_", json.dumps(preview["disclosure"]))
            compare.assert_not_called()

    def test_cross_repository_pairs_require_both_approvals(self):
        rows = [report(1), report(2, "repo-b"), report(3, ""), report(4, "repo-a.git")]
        original = copy.deepcopy(rows)
        with patch.object(cluster, "compare", return_value=completed()) as compare:
            self.assertEqual(self.invoke(rows, ("repo-a",))["candidate_pairs"], 0)
            compare.assert_not_called()
            result = self.invoke(rows, ("repo-a", "repo-b"))
            self.assertEqual(result["groups"], [[1, 2], [3], [4]])
            self.assertEqual([(call.args[0]["id"], call.args[1]["id"]) for call in compare.call_args_list], [(1, 2)])
        self.assertEqual(rows, original)

    def test_bridge_is_not_transitive_consensus(self):
        with patch.object(cluster, "compare", side_effect=[completed(), completed("different"), completed()]):
            result = self.invoke([report(1), report(2), report(3)], ("repo-a",))
        self.assertEqual(result["groups"], [[1, 2], [3]])
        self.assertEqual(result["comparisons"][2]["suggestion"], "same")

    def test_partial_failure_keeps_all_reports_and_completed_advice(self):
        with patch.object(cluster, "compare", side_effect=[completed(), {"status": "unassessed", "reason": "transport_failed"}]) as compare:
            result = self.invoke([report(i) for i in range(1, 5)], ("repo-a",))
        self.assertEqual(result["status"], "partial")
        self.assertEqual(result["evaluated_pairs"], 1)
        self.assertEqual(result["groups"], [[1, 2], [3], [4]])
        self.assertEqual(compare.call_count, 2)

    def test_transport_sends_only_allowlisted_fields_and_protects_key(self):
        with patch.object(cluster.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(response()).encode(), b"")) as transport:
            result = cluster.compare(report(1), report(2), "sensitive-key", 1)
        self.assertEqual(result["suggestion"], "same")
        args, kwargs = transport.call_args
        self.assertNotIn("sensitive-key", " ".join(args[0]))
        config = kwargs["input"].decode().splitlines()
        request = json.loads(json.loads(config[1].split(" = ", 1)[1]))
        self.assertEqual(request["state"]["reports"], [cluster.evidence(report(1)), cluster.evidence(report(2))])
        self.assertNotIn("PRIVATE_", json.dumps(request))

    def test_uncertain_and_invalid_answers_never_group(self):
        cases = [response("uncertain"), response(confidence=0.2)]
        invalid = []
        for field, value in [("confidence", True), ("confidence", float("nan")),
                             ("choice", []), ("type", "noul"), ("probabilities", {"same": 1})]:
            item = response()
            item["answers"]["mechanism"][field] = value
            invalid.append(item)
        for payload in cases + invalid + [{}, []]:
            with self.subTest(payload=payload), patch.object(cluster.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(payload).encode(), b"")):
                result = cluster.compare(report(1), report(2), "key", 1)
                self.assertNotEqual(result.get("suggestion"), "same")

    def test_failed_transport_does_not_echo_private_errors(self):
        outcomes = [subprocess.CompletedProcess([], 22, b"", b"PRIVATE_RESPONSE"),
                    subprocess.TimeoutExpired("curl", 1, output=b"PRIVATE_RESPONSE")]
        for outcome in outcomes:
            with patch.object(cluster.subprocess, "run", side_effect=[outcome]):
                result = cluster.compare(report(1), report(2), "key", 1)
            self.assertEqual(result["status"], "unassessed")
            self.assertNotIn("PRIVATE", json.dumps(result))

    def test_rounded_probabilities_within_half_cent_per_option(self):
        for values, ok in [((0.33, 0.33, 0.33), True), ((0.34, 0.34, 0.33), True),
                           ((0.9, 0.05, 0.06), True), ((0.9, 0.05, 0.03), False), ((0.9, 0.1, 0.02), False)]:
            item = response()
            item["answers"]["mechanism"]["probabilities"] = dict(zip(cluster.CRITERIA, values))
            item["answers"]["mechanism"]["choice"] = "same"
            with self.subTest(values=values):
                if ok:
                    cluster.validate_answer(item)
                else:
                    self.assertRaises(ValueError, cluster.validate_answer, item)

    def test_plan_chunks_cover_every_pair_once_within_size(self):
        for count, size in [(1, 8), (2, 8), (7, 8), (8, 8), (9, 8), (24, 8), (13, 6), (5, 2)]:
            rows = [report(i) for i in range(1, count + 1)]
            chunks = cluster.plan_chunks(rows, size)
            asked = [pair for _, pairs in chunks for pair in pairs]
            with self.subTest(count=count, size=size):
                self.assertEqual(sorted(asked), [(a, b) for a in range(1, count + 1) for b in range(a + 1, count + 1)])
                for members, pairs in chunks:
                    self.assertLessEqual(len(members), size)
                    ids = {row["id"] for row in members}
                    self.assertTrue(all(a in ids and b in ids for a, b in pairs))

    def test_chunk_request_names_each_pair_and_maps_answers_back(self):
        rows = [report(1), report(2), report(3)]
        pairs = [(1, 2), (1, 3), (2, 3)]
        answers = {"pair_1_2": response()["answers"]["mechanism"],
                   "pair_1_3": response("different")["answers"]["mechanism"],
                   "pair_2_3": {"type": "noul"}}
        with patch.object(cluster.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps({"answers": answers}).encode(), b"")) as transport:
            results = cluster.compare_chunk(rows, pairs, "sensitive-key", 1)
        self.assertEqual({pair: result.get("suggestion") for pair, result in results.items()},
                         {(1, 2): "same", (1, 3): "different", (2, 3): None})
        self.assertEqual(results[(2, 3)]["status"], "unassessed")
        args, kwargs = transport.call_args
        self.assertNotIn("sensitive-key", " ".join(args[0]))
        request = json.loads(json.loads(kwargs["input"].decode().splitlines()[1].split(" = ", 1)[1]))
        self.assertEqual(request["state"]["reports"], [cluster.evidence(row) for row in rows])
        self.assertNotIn("PRIVATE_", json.dumps(request))
        self.assertIn("`reports[0]` (id 1) with `reports[2]` (id 3)", request["questions"]["pair_1_3"]["instructions"])

    def test_chunk_transport_failure_marks_every_pair_unassessed(self):
        with patch.object(cluster.subprocess, "run", return_value=subprocess.CompletedProcess([], 22, b"", b"PRIVATE_RESPONSE")):
            results = cluster.compare_chunk([report(1), report(2)], [(1, 2)], "key", 1)
        self.assertEqual(results, {(1, 2): {"status": "unassessed", "reason": "transport_failed"}})

    def test_batched_run_uses_chunks_and_keeps_complete_link(self):
        rows = [report(i) for i in range(1, 6)]
        def fake(members, pairs, key, timeout):
            return {pair: completed("same" if set(pair) <= {1, 2, 3} and pair != (1, 3) else "different") for pair in pairs}
        with patch.object(cluster, "compare_chunk", side_effect=fake) as chunk, patch.object(cluster, "compare") as single:
            result = self.invoke(rows, ("repo-a",), batch=4)
        single.assert_not_called()
        self.assertEqual(result["planned_requests"], chunk.call_count)
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["evaluated_pairs"], 10)
        self.assertEqual(sorted(tuple(pair["ids"]) for pair in result["comparisons"]),
                         [(a, b) for a in range(1, 6) for b in range(a + 1, 6)])
        self.assertEqual(result["groups"], [[1, 2], [3], [4], [5]])

    def test_request_limit_counts_requests_not_pairs(self):
        rows = [report(i) for i in range(1, 25)]
        with patch.object(cluster, "compare_chunk") as chunk, patch.object(cluster, "compare") as single:
            batched = self.invoke(rows, ("repo-a",), limit=15, preview=False, key="", batch=8)
            self.assertEqual((batched["candidate_pairs"], batched["planned_requests"]), (276, 15))
            self.assertEqual(batched["reason"], "missing_key")
            over = self.invoke(rows, ("repo-a",), limit=14, batch=8)
            self.assertEqual((over["status"], over["reason"]), ("skipped", "request_limit"))
            single_path = self.invoke(rows, ("repo-a",), limit=200, batch=1)
            self.assertEqual((single_path["planned_requests"], single_path["reason"]), (276, "request_limit"))
            chunk.assert_not_called()
            single.assert_not_called()

    def test_oversized_chunk_falls_back_to_single_pairs(self):
        # Each report is about a quarter of the state budget: four exceed it, two fit.
        rows = [report(i) for i in range(1, 5)]
        for row in rows:
            row["payload"]["details"] = "x" * (cluster.MAX_STATE_TOKENS * 2 * 5 // 2 // 7)
        requests = cluster.plan_requests(rows, 4)
        self.assertEqual(len(requests), 6)
        self.assertTrue(all(members is not None and len(members) == 2 and len(pairs) == 1 for members, pairs in requests))
        self.assertEqual(len(cluster.plan_requests(rows[1:], 4)), 1)

    def test_oversized_pair_is_unassessed_without_a_request(self):
        rows = [report(i) for i in range(1, 4)]
        rows[0]["payload"]["details"] = "x" * (cluster.MAX_STATE_TOKENS * 3)
        with patch.object(cluster, "compare", return_value=completed()) as compare:
            result = self.invoke(rows, ("repo-a",))
        self.assertEqual(result["planned_requests"], 1)
        self.assertEqual(compare.call_count, 1)
        self.assertEqual([pair.get("reason") for pair in result["comparisons"]], ["request_too_large"] * 2 + [None])
        self.assertEqual(result["groups"], [[1], [2, 3]])

    def test_small_batch_sizes_mean_one_request_per_pair(self):
        rows = [report(i) for i in range(1, 7)]
        for size in (1, 2, 3):
            self.assertTrue(all(len(pairs) == 1 for _, pairs in cluster.plan_requests(rows, size)))
        self.assertEqual(max(len(members) for members, _ in cluster.plan_requests(rows, 5)), 4)

    def test_batched_requests_contain_only_approved_reports(self):
        rows = [report(i, "repo-a" if i % 3 else "repo-b") for i in range(1, 13)] + [report(13, "")]
        seen = []
        def fake(members, pairs, key, timeout):
            seen.append({row["id"] for row in members})
            return {pair: completed("different") for pair in pairs}
        with patch.object(cluster, "compare_chunk", side_effect=fake), patch.object(cluster, "compare", return_value=completed("different")):
            result = self.invoke(rows, ("repo-a",), batch=8)
        approved = {row["id"] for row in rows if row["context"]["git_remote"] == "repo-a"}
        self.assertTrue(seen)
        self.assertTrue(all(ids <= approved for ids in seen))
        self.assertTrue(all(set(pair["ids"]) <= approved for pair in result["comparisons"]))
        self.assertEqual(result["evaluated_pairs"], len(approved) * (len(approved) - 1) // 2)

    def test_one_bad_answer_in_a_batch_does_not_stop_the_run(self):
        rows = [report(i) for i in range(1, 9)]
        def fake(members, pairs, key, timeout):
            return {pair: {"status": "unassessed", "reason": "invalid_response"} if index == 0 else completed("different")
                    for index, pair in enumerate(pairs)}
        with patch.object(cluster, "compare_chunk", side_effect=fake) as chunk:
            result = self.invoke(rows, ("repo-a",), batch=4)
        self.assertEqual(chunk.call_count, result["planned_requests"])
        self.assertEqual(result["status"], "partial")
        self.assertEqual(result["evaluated_pairs"], 28 - chunk.call_count)

    def test_failed_chunk_stops_further_requests(self):
        rows = [report(i) for i in range(1, 9)]
        failure = {"status": "unassessed", "reason": "transport_failed"}
        with patch.object(cluster, "compare_chunk", side_effect=lambda members, pairs, *_: {pair: failure for pair in pairs}) as chunk:
            result = self.invoke(rows, ("repo-a",), batch=4)
        self.assertEqual(chunk.call_count, 1)
        self.assertEqual(result["status"], "partial")
        self.assertEqual(result["evaluated_pairs"], 0)
        self.assertEqual(result["groups"], [[i] for i in range(1, 9)])

    def test_evidence_is_exactly_the_four_text_fields_in_order(self):
        row = report(7)
        row["summary"] = "top-level summary"
        row["payload"]["summary"] = "PRIVATE_PAYLOAD_SUMMARY"
        result = cluster.evidence(row)
        self.assertEqual(list(result), ["id", "category", "summary", "details", "suggested_fix"])
        self.assertEqual(result, {"id": 7, "category": "tooling", "summary": "top-level summary",
                                  "details": "specific failure", "suggested_fix": "fix it"})
        self.assertNotIn("PRIVATE_", json.dumps(result))

    def test_repository_reads_top_level_context(self):
        self.assertEqual(cluster.repository(report(1, "git@example.com:o/r.git")), "git@example.com:o/r.git")
        self.assertEqual(cluster.repository({k: v for k, v in report(1).items() if k != "context"}), "")

    def test_cli_rejects_contaminated_or_duplicate_digest_without_network(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "index.json"
            path.write_text(json.dumps([report(1), report(2)]))
            proc = subprocess.run([sys.executable, str(SCRIPT), str(path)], capture_output=True, text=True,
                                  env={**os.environ, "TYPESAFE_API_KEY": ""})
            self.assertEqual(proc.returncode, 0)
            self.assertEqual((json.loads(proc.stdout)["status"], json.loads(proc.stdout)["reason"]), ("skipped", "no_opt_in"))
            for rows in [[report(1), report(1)], [{**report(1), "processed_at": "already-done"}],
                         [{**report(1), "kind": "event"}], [{**report(1), "context": "repo-a"}],
                         [{**report(1), "context": {"git_remote": 1}}]]:
                path.write_text(json.dumps(rows))
                proc = subprocess.run([sys.executable, str(SCRIPT), str(path)], capture_output=True, text=True,
                                      env={**os.environ, "TYPESAFE_API_KEY": ""})
                self.assertEqual(proc.returncode, 1)
                self.assertEqual(json.loads(proc.stdout)["status"], "error")


if __name__ == "__main__":
    unittest.main()
