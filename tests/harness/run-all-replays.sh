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
    out=$(mktemp)
    rc=0
    bash "$replay" >"$out" 2>&1 || rc=$?
    # Stream the replay output so logs remain useful.
    cat "$out"
    if [ "$rc" -eq 0 ] && grep -qE '^\[replay\] __(SKIP|XFAIL)__' "$out"; then
        # Replays signal "skip" by exiting 0 with a __SKIP__ or __XFAIL__
        # marker in stdout. Count them as skips, not passes, so the harness
        # gate doesn't false-green on xfails that test nothing.
        SKIP_COUNT=$((SKIP_COUNT + 1))
        echo "[run-all] SKIP: $name"
    elif [ "$rc" -eq 0 ]; then
        PASS_COUNT=$((PASS_COUNT + 1))
        echo "[run-all] PASS: $name"
    else
        FAIL_COUNT=$((FAIL_COUNT + 1))
        FAILED_NAMES+=("$name")
        echo "[run-all] FAIL: $name"
    fi
    rm -f "$out"
done

echo ""
echo "[run-all] ============================="
echo "[run-all] Replay summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed, ${SKIP_COUNT} skipped (of ${#REPLAYS[@]} total)"
if [ ${FAIL_COUNT} -gt 0 ]; then
    echo "[run-all] Failed:"
    for name in "${FAILED_NAMES[@]}"; do
        echo "[run-all]   - $name"
    done
    exit 1
fi
echo "[run-all] All replays passed."
