#!/usr/bin/env bash
# Run every replay under tests/harness/replays/ against a fresh harness.
#
# Boots the harness (up.sh + seed.sh), runs each `replays/*.sh` in
# alphabetical order, tracks pass/fail, and tears down on exit. Returns
# non-zero if any replay failed.
#
# Usage:
#   ./run-all-replays.sh                # boot, run, teardown
#   KEEP_UP=1 ./run-all-replays.sh      # leave harness running on exit (debug)
#   REBUILD=1 ./run-all-replays.sh      # rebuild images before booting
#
# CI usage: invoke without flags. The trap-on-EXIT teardown ensures we
# don't leak Docker resources when a replay fails partway through.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

REPLAYS_DIR="$HERE/replays"
if [ ! -d "$REPLAYS_DIR" ]; then
    echo "[run-all] no replays/ directory at $REPLAYS_DIR — nothing to run"
    exit 1
fi

shopt -s nullglob
REPLAYS=("$REPLAYS_DIR"/*.sh)
shopt -u nullglob
if [ ${#REPLAYS[@]} -eq 0 ]; then
    echo "[run-all] replays/ is empty — nothing to run"
    exit 1
fi

cleanup() {
    local exit_code=$?
    if [ "${KEEP_UP:-0}" = "1" ]; then
        echo ""
        echo "[run-all] KEEP_UP=1 — leaving harness up. Tear down manually with ./down.sh"
    else
        echo ""
        echo "[run-all] tearing down harness..."
        ./down.sh >/dev/null 2>&1 || echo "[run-all] WARN: ./down.sh exited non-zero"
    fi
    exit "$exit_code"
}
trap cleanup EXIT INT TERM

echo "[run-all] booting harness..."
if [ "${REBUILD:-0}" = "1" ]; then
    ./up.sh --rebuild
else
    ./up.sh
fi

echo "[run-all] seeding workspaces..."
./seed.sh

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
FAILED_NAMES=()

for replay in "${REPLAYS[@]}"; do
    name=$(basename "$replay" .sh)
    echo ""
    echo "[run-all] ━━━ $name ━━━"
    if bash "$replay"; then
        # Replays signal "skip" by exiting 0 with a __SKIP__ marker in stdout —
        # but we capture that as a pass here since the script exited 0. The
        # skip is documented in the script's own output. CI uses pass/fail.
        PASS_COUNT=$((PASS_COUNT + 1))
        echo "[run-all] PASS: $name"
    else
        FAIL_COUNT=$((FAIL_COUNT + 1))
        FAILED_NAMES+=("$name")
        echo "[run-all] FAIL: $name"
    fi
done

echo ""
echo "[run-all] ============================="
echo "[run-all] Replay summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed (of ${#REPLAYS[@]} total)"
if [ ${FAIL_COUNT} -gt 0 ]; then
    echo "[run-all] Failed:"
    for name in "${FAILED_NAMES[@]}"; do
        echo "[run-all]   - $name"
    done
    exit 1
fi
echo "[run-all] All replays passed."
