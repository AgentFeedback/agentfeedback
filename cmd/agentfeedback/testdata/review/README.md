# Review fixtures

- `base/`: a runner cache base, `scorecards.tsv` beside the run dir `20260730-101010-4242/` (meta.json, summary.tsv, gpt56.md, prompt.md). Tests copy `base/` whole, keeping the run dir basename, which is part of the key.
- `*.v1.json`: golden v1 bodies of `agentfeedback submit review <run_dir> --dry-run [--include-outputs]` with run_ts read as UTC; `context` is removed because os, arch and client vary.
- `*.v3.json`: the request bodies `submit-review.sh <run_dir> [--include-outputs]` of the v3 bash client (since removed) sent to a mock server for a copy of `base/`, captured once from its request log with HOME, the URL and the key set to throwaway values and formatted with `jq .`.
