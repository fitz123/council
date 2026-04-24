#!/usr/bin/env bash
# test/smoke/run-web-tools.sh — gated live-Claude smoke that proves a real
# debate run actually invokes WebSearch / WebFetch and surfaces a URL in
# the operator-facing output. Skipped unless COUNCIL_LIVE_CLAUDE=1 because
# CI lacks the `claude` CLI (and we do not want to burn tokens by default).
#
# Pairs with the unit-level F13/F17 assertions in pkg/debate and
# test/smoke/smoke_test.go: those prove the executor REQUEST carries the
# tool allow-list; this script proves the spawned subprocess actually
# uses the granted tools end-to-end. Per docs/design/v2-web-tools.md §7.
#
# Signal design (Codex review): the URL grep alone is a weak signal —
# Claude can cite https://go.dev/... from memory without actually calling
# a tool. So the smoke runs TWO probes:
#
#   1) a direct `claude -p` invocation with `--output-format stream-json
#      --verbose` and a prompt that forces a WebFetch on a specific URL.
#      The JSONL output exposes `tool_use` events (ADR-0010 §Verification
#      point 2) — grepping for `"type":"tool_use"` with `"name":"WebFetch"`
#      is the BULLETPROOF signal that the --allowedTools / --permission-mode
#      flag plumbing at the CLI layer actually fires the tool.
#
#   2) a full `council` run with the same prompt shape; its `output.md`
#      is then grepped for a URL citation. This is the weaker end-to-end
#      signal, but together with probe 1 it confirms the CLI flags that
#      pkg/debate sets make it through to a real tool invocation.
#
# Exit codes:
#   0  — passed, or skipped because the gate is unset
#   1  — failed (no tool_use event / no URL cited / build failed /
#        council exited non-zero)

set -uo pipefail

if [ "${COUNCIL_LIVE_CLAUDE:-}" != "1" ]; then
  echo "COUNCIL_LIVE_CLAUDE != 1, skipping live web-tools smoke"
  exit 0
fi

if ! command -v claude >/dev/null; then
  echo "FAIL: COUNCIL_LIVE_CLAUDE=1 but \`claude\` binary not on PATH"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT" || { echo "FAIL: cd to repo root $REPO_ROOT"; exit 1; }

BIN="$REPO_ROOT/council"
echo "==> building release binary at $BIN"
if ! go build -o "$BIN" ./cmd/council; then
  echo "BUILD FAILED: release binary"
  exit 1
fi

WORKDIR="$(mktemp -d -t council-web-tools.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT

# Probe 1: direct claude stream-json probe. Forces a WebFetch on a specific
# endpoint (https://go.dev/VERSION?m=text) so the prompt cannot be answered
# from memory and the JSONL transcript will contain an unambiguous tool_use
# event. This is the signal that actually proves the CLI honors
# --allowedTools + --permission-mode at subprocess level; council's
# text-format path cannot see tool_use directly.
PROBE_LOG="$WORKDIR/probe.jsonl"
echo "==> probe 1: direct claude --output-format stream-json (tool_use proof)"
if ! claude -p \
    --model sonnet \
    --output-format stream-json --verbose \
    --allowedTools WebSearch,WebFetch \
    --permission-mode bypassPermissions \
    "Use WebFetch to retrieve https://go.dev/VERSION?m=text and quote the first line verbatim." \
    > "$PROBE_LOG"; then
  echo "FAIL: direct claude probe exited non-zero"
  echo "----- tail of probe.jsonl -----"
  tail -n 40 "$PROBE_LOG" || true
  echo "-------------------------------"
  exit 1
fi

# Single regex that matches both fields on the same JSONL line (in either
# order). Two separate greps would false-positive on a tool_use(WebSearch)
# line plus an unrelated line that mentions WebFetch.
if ! grep -Eq '"type":"tool_use".*"name":"WebFetch"|"name":"WebFetch".*"type":"tool_use"' "$PROBE_LOG"; then
  echo "FAIL: no tool_use(WebFetch) event in direct claude stream-json output"
  echo "(if --allowedTools were silently dropped, or the CLI fired a different tool, this surfaces here)"
  echo "----- probe.jsonl -----"
  cat "$PROBE_LOG"
  echo "-----------------------"
  exit 1
fi
echo "==> probe 1 PASSED: tool_use(WebFetch) observed in direct claude run"

# Probe 2: full council run. Even with probe 1 passing, this confirms the
# orchestrator's end-to-end pipeline (rounds → experts → aggregate →
# output.md) surfaces the tool-derived URL to the operator.
echo "==> probe 2: running real council session (workdir: $WORKDIR)"
cd "$WORKDIR" || { echo "FAIL: cd to workdir $WORKDIR"; exit 1; }
if ! "$BIN" "What is the latest stable Go version? Cite the URL where you found it."; then
  echo "FAIL: council exited non-zero"
  exit 1
fi

# Find the single session folder under .council/sessions/ and check its
# operator-facing output for a URL. The R1/R2 prompts (independent.md /
# peer-aware.md) explicitly tell experts to cite URLs when they used
# WebFetch — so a missing URL means either no expert reached for the web
# or the citation discipline broke.
SESSION="$(find .council/sessions -mindepth 1 -maxdepth 1 -type d | head -n1)"
if [ -z "$SESSION" ]; then
  echo "FAIL: no session folder under .council/sessions/"
  exit 1
fi

OUT="$SESSION/output.md"
if [ ! -f "$OUT" ]; then
  echo "FAIL: $OUT missing (winner path expected)"
  ls -la "$SESSION" || true
  exit 1
fi

if ! grep -qE 'https?://' "$OUT"; then
  echo "FAIL: no URL cited in $OUT"
  echo "----- output.md -----"
  cat "$OUT"
  echo "---------------------"
  exit 1
fi

echo "==> probe 2 PASSED: $OUT contains at least one URL citation"
grep -oE 'https?://[^[:space:])"]+' "$OUT" | sort -u
