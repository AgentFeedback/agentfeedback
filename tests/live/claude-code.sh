#!/usr/bin/env bash
# Live Claude Code check, run by scripts/live-harness.sh inside the toolchain
# image with no network. A server on loopback; then, with CLAUDE_CONFIG_DIR
# unset and set to ~/.claude, each in a fresh HOME holding an unrelated MCP
# entry and a settings.json with an unrelated key and Stop hook: install
# --mcp, `claude mcp list` connects to it, a second install changes nothing,
# uninstall removes only it; the skill and Stop hook install, reinstall
# unchanged, the hook command runs, and uninstall restores settings.json byte
# for byte. Claude loading the skill needs a model session and is not checked.
set -euo pipefail

KEY=live-harness-key
URL=http://127.0.0.1:18080
OTHER=http://127.0.0.1:1/other
SETTINGS='{"theme": "dark", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "true"}]}]}}'
export AGENT_FEEDBACK_API_KEY="$KEY"

fail() { echo "live-harness: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

mkdir -p /tmp/server
API_KEY="$KEY" DATABASE_PATH=/tmp/server/agentfeedback.db HTTP_LISTEN_ADDR=127.0.0.1:18080 \
  agentfeedback serve >/tmp/server/log 2>&1 &
for _ in $(seq 50); do
  curl -fsS "$URL/ready" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "$URL/ready" >/dev/null 2>&1 || fail "the server did not become ready: $(cat /tmp/server/log)"

# expect <status> <agentfeedback args...>: the run's JSON result has that
# status, or the run's output is printed.
expect() {
  local want=$1 got
  shift
  got=$(timeout 60 agentfeedback "$@" 2>/tmp/af.err | tail -n 1 | jq -r .status) || true
  [ "$got" = "$want" ] || fail "$mode: agentfeedback $* gave status '$got', want $want: $(cat /tmp/af.err)"
}
claude_() { timeout 60 claude "$@"; }

check() {
  mode=$1
  local home dir json list hook
  home=/tmp/home-$mode
  dir="$home/.claude"
  mkdir -p "$dir"
  export HOME="$home" XDG_CONFIG_HOME="$home/.config" XDG_CACHE_HOME="$home/.cache" XDG_DATA_HOME="$home/.local/share"
  if [ "$mode" = set ]; then
    export CLAUDE_CONFIG_DIR="$dir"
    json="$dir/.claude.json"
  else
    unset CLAUDE_CONFIG_DIR
    json="$home/.claude.json"
  fi
  claude_ mcp add-json other '{"type":"http","url":"'"$OTHER"'"}' --scope user >/dev/null
  printf '%s\n' "$SETTINGS" >"$dir/settings.json"
  cp "$dir/settings.json" /tmp/settings.before

  expect installed install claude-code --mcp --server "$URL"
  jq -e --arg u "$URL/mcp" '.mcpServers.agentfeedback.url == $u' "$json" >/dev/null || fail "$mode: no agentfeedback entry in $json"
  list=$(claude_ mcp list 2>&1)
  grep -F "agentfeedback: $URL/mcp (HTTP) - " <<<"$list" | grep -q ' Connected$' || fail "$mode: claude mcp list does not connect to agentfeedback: $list"
  grep -qF "other: $OTHER" <<<"$list" || fail "$mode: the unrelated entry is gone: $list"
  pass "$mode: install --mcp writes $json and claude mcp list connects"
  expect unchanged install claude-code --mcp --server "$URL"
  pass "$mode: a second install --mcp changes nothing"
  expect uninstalled uninstall claude-code
  list=$(claude_ mcp list 2>&1)
  if grep -qF agentfeedback <<<"$list"; then fail "$mode: claude mcp list still lists agentfeedback: $list"; fi
  jq -e --arg u "$OTHER" '.mcpServers.other.url == $u' "$json" >/dev/null || fail "$mode: uninstall changed the unrelated entry"
  if [ "$mode" = set ] && [ -e "$home/.claude.json" ]; then fail "set: ~/.claude.json was written"; fi
  pass "$mode: uninstall removes only the agentfeedback entry"

  expect installed install claude-code --server "$URL"
  [ -f "$dir/skills/agentfeedback/SKILL.md" ] || fail "$mode: no skill in $dir/skills"
  jq -e '.theme == "dark" and ([.hooks.Stop[].hooks[].command] | index("true") != null)' "$dir/settings.json" >/dev/null ||
    fail "$mode: install changed the unrelated settings: $(cat "$dir/settings.json")"
  hook=$(jq -r '[.hooks.Stop[].hooks[].command | select(test("agentfeedback"))][0] // empty' "$dir/settings.json")
  [ -n "$hook" ] || fail "$mode: no agentfeedback Stop hook in $dir/settings.json"
  (cd /tmp && timeout 30 bash -c "$hook" </dev/null) || fail "$mode: the Stop hook command failed: $hook"
  expect unchanged install claude-code --server "$URL"
  expect uninstalled uninstall claude-code
  [ ! -e "$dir/skills/agentfeedback" ] || fail "$mode: the skill is left in $dir/skills"
  cmp -s /tmp/settings.before "$dir/settings.json" || fail "$mode: uninstall did not restore settings.json: $(cat "$dir/settings.json")"
  [ ! -e "$XDG_CONFIG_HOME/agentfeedback/install.json" ] || fail "$mode: the install manifest is left"
  pass "$mode: the skill and Stop hook install, run and uninstall, leaving settings.json as it was"
}

check unset
check set
