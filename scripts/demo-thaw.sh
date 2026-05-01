#!/usr/bin/env bash
# demo-thaw.sh — re-enable workflows that demo-freeze.sh disabled.
#
# Usage:
#   scripts/demo-thaw.sh <freeze-timestamp>
#   scripts/demo-thaw.sh 20260503-180000
#
# Reads disabled-workflows-<ts>.txt produced by demo-freeze.sh and
# runs `gh workflow enable` for each entry. Idempotent — re-enabling
# an already-enabled workflow is a no-op.
#
# Defaults to executing (the inverse of freeze, which defaults to
# dry-run). Pass --dry-run to print without executing.
#
# Prereqs:
#   - gh CLI authenticated with workflow:write scope on Molecule-AI org
#
# Exit codes:
#   0 — all workflows re-enabled
#   1 — pre-flight failure (missing receipt file, missing tooling)
#   2 — partial thaw (some workflows did not enable; check output)

set -euo pipefail

usage() {
  cat <<'USAGE'
demo-thaw.sh — re-enable workflows that demo-freeze.sh disabled.

Usage:
  scripts/demo-thaw.sh <freeze-timestamp>            # apply
  scripts/demo-thaw.sh <freeze-timestamp> --dry-run  # print without applying

ts is the YYYYMMDD-HHMMSS suffix on
scripts/demo-freeze-snapshots/disabled-workflows-*.txt produced by
demo-freeze.sh.
USAGE
}

DRY_RUN=0
TS=""
for arg in "$@"; do
  case "$arg" in
    --dry-run)
      DRY_RUN=1
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      if [ -z "$TS" ]; then
        TS="$arg"
      else
        echo "unknown arg: $arg" >&2
        usage >&2
        exit 2
      fi
      ;;
  esac
done

if [ -z "$TS" ]; then
  echo "usage: $0 <freeze-timestamp> [--dry-run]" >&2
  echo "  e.g. $0 20260503-180000" >&2
  echo "  ts is the YYYYMMDD-HHMMSS suffix on demo-freeze-snapshots/disabled-workflows-*.txt" >&2
  exit 2
fi

command -v gh >/dev/null || { echo "ERROR: gh CLI required" >&2; exit 1; }
if ! gh auth status >/dev/null 2>&1; then
  echo "ERROR: gh not authenticated. Run 'gh auth login' first." >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKFLOWS_FILE="${SCRIPT_DIR}/demo-freeze-snapshots/disabled-workflows-${TS}.txt"

if [ ! -f "$WORKFLOWS_FILE" ]; then
  echo "ERROR: receipt not found: $WORKFLOWS_FILE" >&2
  echo "Available receipts:" >&2
  ls "${SCRIPT_DIR}/demo-freeze-snapshots/" 2>/dev/null | grep '^disabled-workflows-' >&2 || echo "  (none)" >&2
  exit 1
fi

if [ $DRY_RUN -eq 1 ]; then
  echo "=== DRY RUN (no changes will be made) ==="
else
  echo "=== THAWING — re-enabling workflows ==="
fi
echo "Reading: $WORKFLOWS_FILE"
echo

PARTIAL_FAIL=0
while IFS=': ' read -r repo workflow; do
  [ -z "$repo" ] && continue
  if [ $DRY_RUN -eq 1 ]; then
    echo "  (dry-run) would enable: gh workflow enable $workflow -R $repo"
  else
    if gh workflow enable "$workflow" -R "$repo" 2>/tmp/thaw.err; then
      echo "  OK   $repo/$workflow re-enabled"
    else
      echo "  FAIL $repo/$workflow: $(cat /tmp/thaw.err)" >&2
      PARTIAL_FAIL=1
    fi
  fi
done < "$WORKFLOWS_FILE"

echo
if [ $DRY_RUN -eq 1 ]; then
  echo "=== DRY RUN COMPLETE ==="
  echo "Re-run without --dry-run to apply."
  exit 0
fi

echo "=== THAW COMPLETE ==="
echo "Cascades restored. Next workspace/** push to molecule-core/staging will"
echo "auto-publish the runtime wheel and fan out to template rebuilds as normal."
if [ $PARTIAL_FAIL -ne 0 ]; then
  echo
  echo "WARNING: one or more workflows did not re-enable cleanly. Re-run or enable manually:" >&2
  echo "  gh workflow list -R <repo>" >&2
  exit 2
fi
exit 0
