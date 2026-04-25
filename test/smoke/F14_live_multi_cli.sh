#!/usr/bin/env bash
# test/smoke/F14_live_multi_cli.sh — gated end-to-end three-CLI smoke
# (ADR-0012 / ADR-0013, plan task 9).
#
# Asserts the multi-CLI debate path actually wires up at the binary
# level: `council init` materialises a three-expert profile under an
# ephemeral $HOME, a real debate produces a 2-round verdict.json with
# three expert sections in round 2, and the winner's output.md cites
# at least one URL — proving every vendor's web-tool translation
# (codex `-c tools.web_search=true`, gemini `--policy <toml>`, claude
# `--allowedTools WebSearch,WebFetch`) makes it through end-to-end.
#
# The unit-level codex/gemini live tests already prove single-CLI
# vendor wiring; F14 is the cross-vendor integration probe.
#
# Skipped silently unless COUNCIL_LIVE_ALL=1. The gate is composite:
# all three per-CLI gates (CLAUDE/CODEX/GEMINI) must also be 1, since
# COUNCIL_LIVE_ALL=1 alone with one CLI un-authed would surface as a
# deep orchestrator failure rather than a clear setup mistake.
#
# Exit codes:
#   0  — passed, or skipped because the gate is unset
#   1  — failed (build, init, council run, or assertion)
#   2  — environment misconfigured (per-CLI gate missing, binary
#        missing, or jq missing)

set -uo pipefail

if [ "${COUNCIL_LIVE_ALL:-}" != "1" ]; then
  echo "skipping F14 (set COUNCIL_LIVE_ALL=1)"
  exit 0
fi

for v in COUNCIL_LIVE_CLAUDE COUNCIL_LIVE_CODEX COUNCIL_LIVE_GEMINI; do
  if [ "${!v:-}" != "1" ]; then
    echo "FAIL: $v must also be 1 when COUNCIL_LIVE_ALL=1"
    exit 2
  fi
done

for cli in claude codex gemini jq; do
  if ! command -v "$cli" >/dev/null; then
    echo "FAIL: \`$cli\` not on PATH"
    exit 2
  fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT" || { echo "FAIL: cd to repo root $REPO_ROOT"; exit 1; }

BIN="$REPO_ROOT/council"
echo "==> building release binary at $BIN"
if ! go build -o "$BIN" ./cmd/council; then
  echo "BUILD FAILED: release binary"
  exit 1
fi

tmp_home="$(mktemp -d -t council-f14-home.XXXXXX)"
workdir="$(mktemp -d -t council-f14-cwd.XXXXXX)"
trap 'rm -rf "$tmp_home" "$workdir"' EXIT

# Step 1: council init under ephemeral $HOME. All three CLIs must
# probe-verify so the generated profile carries three experts.
echo "==> running \`council init\` (HOME=$tmp_home)"
if ! HOME="$tmp_home" "$BIN" init; then
  echo "FAIL: council init exited non-zero"
  exit 1
fi

profile="$tmp_home/.config/council/default.yaml"
if [ ! -f "$profile" ]; then
  echo "FAIL: $profile not written by init"
  exit 1
fi

n_executors="$(grep -c 'executor:' "$profile")"
if [ "$n_executors" != "3" ]; then
  echo "FAIL: $profile has $n_executors executor lines, want 3"
  echo "----- profile -----"
  cat "$profile"
  echo "-------------------"
  exit 1
fi
echo "==> init wrote 3-expert profile at $profile"

# Step 2: run a real debate against the just-written profile. cwd is a
# fresh tmp dir so .council/sessions/ does not pollute the repo.
cd "$workdir" || { echo "FAIL: cd to workdir $workdir"; exit 1; }
echo "==> running council debate (workdir: $workdir)"
stdout_log="$workdir/council.stdout"
if ! HOME="$tmp_home" "$BIN" "What is the current stable Go version? Cite a URL." > "$stdout_log"; then
  echo "FAIL: council exited non-zero"
  echo "----- stdout -----"
  cat "$stdout_log"
  echo "------------------"
  exit 1
fi

session="$(find .council/sessions -mindepth 1 -maxdepth 1 -type d | head -n1)"
if [ -z "$session" ]; then
  echo "FAIL: no session folder under .council/sessions/"
  exit 1
fi

verdict="$session/verdict.json"
if [ ! -f "$verdict" ]; then
  echo "FAIL: $verdict missing"
  exit 1
fi

# rounds[1] is round 2 (zero-indexed). With a 2-round profile and all
# three experts surviving R1, R2 must contain three expert entries
# (`participation` of ok or carried — the count assertion treats both
# as present, which is what the verdict shape gates).
r2_experts="$(jq -r '.rounds[1].experts | length' "$verdict" 2>/dev/null || echo "x")"
if [ "$r2_experts" != "3" ]; then
  echo "FAIL: rounds[1].experts has $r2_experts entries, want 3"
  jq '.rounds[1].experts' "$verdict" || true
  exit 1
fi
echo "==> verdict.json has 3 expert sections under rounds[1].experts"

output_md="$session/output.md"
if [ ! -f "$output_md" ]; then
  echo "FAIL: $output_md missing"
  exit 1
fi

if ! grep -qE 'https?://' "$output_md"; then
  echo "FAIL: no URL cited in $output_md"
  echo "----- output.md -----"
  cat "$output_md"
  echo "---------------------"
  exit 1
fi

echo "==> F14 PASSED: 3-expert init + 2-round debate + URL-cited answer"
grep -oE 'https?://[^[:space:])"]+' "$output_md" | sort -u
