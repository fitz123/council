#!/usr/bin/env bash
# test/smoke/run.sh — execute the v2 debate-engine fitness functions
# (F1–F9, F12 plus the resume-after-SIGINT flow).
#
# Builds two binaries up front:
#   ./council        (release; used by F1/F2 only when COUNCIL_LIVE_CLAUDE=1)
#   ./council-test   (built with -tags testbinary; substitutes mock executors
#                     for "claude-code" so F3–F12 do not need the real CLI)
#
# Then runs `go test -tags smoke ./test/smoke/...` and prints a per-F# summary
# parsed from `go test -v` output. Exits 0 iff every smoke test passes; exits
# non-zero otherwise so CI / loop runners can gate on it.
#
# Env vars honored:
#   COUNCIL_LIVE_CLAUDE=1   — opt in to F1/F2 (real claude CLI required)
#   COUNCIL_KEEP_BINARIES=1 — leave ./council and ./council-test in place
#                             after the run (default: clean up)

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

RELEASE_BIN="$REPO_ROOT/council"
TEST_BIN="$REPO_ROOT/council-test"

echo "==> building release binary at $RELEASE_BIN"
if ! go build -o "$RELEASE_BIN" ./cmd/council; then
  echo "BUILD FAILED: release binary"
  exit 1
fi

echo "==> building testbinary at $TEST_BIN (tags: testbinary)"
if ! go build -tags testbinary -o "$TEST_BIN" ./cmd/council; then
  echo "BUILD FAILED: testbinary"
  exit 1
fi

export COUNCIL_RELEASE_BINARY="$RELEASE_BIN"
export COUNCIL_TEST_BINARY="$TEST_BIN"

LOG="$(mktemp -t council-smoke.XXXXXX.log)"
trap 'rm -f "$LOG"' EXIT

echo "==> running smoke tests (log: $LOG)"
go test -tags smoke -v -count=1 -timeout 5m ./test/smoke/... | tee "$LOG"
TEST_EXIT=${PIPESTATUS[0]}

echo
echo "==> per-F# summary"
# Match go test -v lines like:
#   "--- PASS: TestF1_LiveHappyPath (0.00s)"
#   "--- FAIL: TestF4_RetryRecorded (0.50s)"
#   "--- SKIP: TestF1_LiveHappyPath (0.00s)"
awk '
  /^    --- (PASS|FAIL|SKIP):/ { next }  # subtest lines — roll up to parent
  /^--- (PASS|FAIL|SKIP): TestF/ {
    status=$2; name=$3; dur=$4
    sub(/:$/, "", status)
    printf "  %-6s %-40s %s\n", status, name, dur
  }
' "$LOG"

if [ "${COUNCIL_KEEP_BINARIES:-}" != "1" ]; then
  rm -f "$RELEASE_BIN" "$TEST_BIN"
fi

if [ "$TEST_EXIT" -ne 0 ]; then
  echo
  echo "==> SMOKE FAILED (go test exit $TEST_EXIT)"
  exit "$TEST_EXIT"
fi
echo
echo "==> SMOKE PASSED"
