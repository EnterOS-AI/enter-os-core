#!/usr/bin/env bash
# demo-freeze.sh — disable the runtime + template image publish cascades
# during a demo-prep window so a stray staging merge can't auto-rebuild
# `:latest` for the 8 workspace-template images mid-demo.
#
# Demo prep typically runs T-48h to T+1h. During that window:
#
#   PATH 1: any merge to molecule-core/staging that touches workspace/**
#           → publish-runtime.yml fires
#           → PyPI auto-bumps molecule-ai-workspace-runtime patch version
#           → repository_dispatch fans out to 8 workspace-template-* repos
#           → each template repo rebuilds and re-tags
#             153263036946.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/workspace-template-<runtime>:latest
#
#   PATH 2: any merge to a workspace-template-* repo's main branch
#           → that repo's publish-image.yml fires
#           → 153263036946.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/workspace-template-<runtime>:latest
#             gets re-tagged
#
#   provisioner.go:296 RuntimeImages[runtime] reads `:latest` at every
#   workspace boot. A new workspace provision during demo pulls whatever
#   `:latest` resolved to seconds earlier — so a bad merge minutes
#   before the demo can break a tenant the funder is about to see.
#
# This script captures the current good `:latest` digests for all 8
# templates and disables both cascade vectors. The complementary
# demo-thaw.sh re-enables them.
#
# Usage:
#   scripts/demo-freeze.sh                # dry run — print what would happen
#   scripts/demo-freeze.sh --execute      # actually disable workflows + snapshot
#
# Prereqs:
#   - gh CLI authenticated with workflow:write scope on Molecule-AI org
#   - curl + jq (for digest snapshot via GHCR anonymous registry API)
#
# Output:
#   <snapshot dir>/digests-YYYYMMDD-HHMMSS.txt
#     One line per template: "<runtime>: <digest>"
#   <snapshot dir>/disabled-workflows-YYYYMMDD-HHMMSS.txt
#     One line per disabled workflow: "<repo>: <workflow>"
#
# Exit codes:
#   0 — freeze complete (or dry-run successful)
#   1 — pre-flight failure (missing tooling, missing auth, etc.)
#   2 — partial freeze (some workflows did not disable cleanly; see log)

set -euo pipefail

usage() {
  cat <<'USAGE'
demo-freeze.sh — disable the runtime + template image publish cascades
during a demo-prep window.

Captures current :latest digests for all 8 workspace-template-* images
and disables the workflows that would otherwise re-tag them.

Usage:
  scripts/demo-freeze.sh                # dry run — print what would happen
  scripts/demo-freeze.sh --execute      # actually disable workflows + snapshot

See the comment block at the top of this script for the full procedure.
USAGE
}

EXECUTE=0
case "${1:-}" in
  --execute)
    EXECUTE=1
    ;;
  --help|-h)
    usage
    exit 0
    ;;
  "")
    ;;
  *)
    echo "unknown arg: $1" >&2
    usage >&2
    exit 2
    ;;
esac

# Templates and their GHCR repository slugs. Source of truth for the
# runtime → image map is workspace-server/internal/provisioner/provisioner.go
# RuntimeImages — keep this list in sync if a runtime is added.
TEMPLATES=(
  "claude-code"
  "hermes"
  "openclaw"
  "langgraph"
  "deepagents"
  "crewai"
  "autogen"
  "gemini-cli"
)

# Pre-flight: required tooling.
need() {
  command -v "$1" >/dev/null || { echo "ERROR: missing required tool: $1" >&2; exit 1; }
}
need gh
need curl
need jq

# Pre-flight: gh auth. Snapshot via anonymous GHCR token works without
# org auth, but workflow disable needs an authenticated gh.
if ! gh auth status >/dev/null 2>&1; then
  echo "ERROR: gh not authenticated. Run 'gh auth login' first." >&2
  exit 1
fi

# Snapshot location relative to this script. Keeping it under scripts/
# rather than a temp dir means freeze receipts are easy to find again
# during the actual demo.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SNAPSHOT_DIR="${SCRIPT_DIR}/demo-freeze-snapshots"
mkdir -p "$SNAPSHOT_DIR"
TS="$(date -u +%Y%m%d-%H%M%S)"
DIGESTS_FILE="${SNAPSHOT_DIR}/digests-${TS}.txt"
WORKFLOWS_FILE="${SNAPSHOT_DIR}/disabled-workflows-${TS}.txt"

if [ $EXECUTE -eq 0 ]; then
  echo "=== DRY RUN (no changes will be made; pass --execute to apply) ==="
else
  echo "=== EXECUTING FREEZE — workflows will be disabled ==="
fi
echo "Snapshot timestamp: $TS"
echo "Digest log:    $DIGESTS_FILE"
echo "Workflow log:  $WORKFLOWS_FILE"
echo

# Step 1: capture current :latest digest for each template.
echo "→ Capturing current :latest digests"
for tpl in "${TEMPLATES[@]}"; do
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:molecule-ai/workspace-template-${tpl}:pull" | jq -r .token 2>/dev/null || true)
  if [ -z "$token" ] || [ "$token" = "null" ]; then
    echo "  WARN: token fetch failed for $tpl — skipping digest capture"
    continue
  fi
  digest=$(curl -fsSI \
    -H "Authorization: Bearer $token" \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
    "https://ghcr.io/v2/molecule-ai/workspace-template-${tpl}/manifests/latest" 2>/dev/null \
    | grep -i 'docker-content-digest' \
    | awk '{print $2}' \
    | tr -d '\r')
  if [ -z "$digest" ]; then
    echo "  WARN: digest fetch failed for $tpl"
    continue
  fi
  echo "  $tpl: $digest"
  if [ $EXECUTE -eq 1 ]; then
    echo "$tpl: $digest" >> "$DIGESTS_FILE"
  fi
done
echo

# Step 2: disable publish-runtime.yml in molecule-core (PATH 1 source).
echo "→ Disabling publish-runtime.yml in molecule-core (kills runtime → 8-template cascade)"
if [ $EXECUTE -eq 1 ]; then
  if gh workflow disable publish-runtime.yml -R Molecule-AI/molecule-core 2>/tmp/freeze.err; then
    echo "  OK   molecule-core/publish-runtime.yml disabled"
    echo "Molecule-AI/molecule-core: publish-runtime.yml" >> "$WORKFLOWS_FILE"
  else
    echo "  FAIL molecule-core/publish-runtime.yml: $(cat /tmp/freeze.err)" >&2
  fi
else
  echo "  (dry-run) would disable: gh workflow disable publish-runtime.yml -R Molecule-AI/molecule-core"
fi
echo

# Step 3: disable publish-image.yml in each of the 8 template repos (PATH 2 sources).
echo "→ Disabling publish-image.yml in each workspace-template-* repo"
PARTIAL_FAIL=0
for tpl in "${TEMPLATES[@]}"; do
  repo="Molecule-AI/molecule-ai-workspace-template-${tpl}"
  if [ $EXECUTE -eq 1 ]; then
    if gh workflow disable publish-image.yml -R "$repo" 2>/tmp/freeze.err; then
      echo "  OK   $repo/publish-image.yml disabled"
      echo "${repo}: publish-image.yml" >> "$WORKFLOWS_FILE"
    else
      echo "  FAIL $repo/publish-image.yml: $(cat /tmp/freeze.err)" >&2
      PARTIAL_FAIL=1
    fi
  else
    echo "  (dry-run) would disable: gh workflow disable publish-image.yml -R $repo"
  fi
done
echo

if [ $EXECUTE -eq 0 ]; then
  echo "=== DRY RUN COMPLETE ==="
  echo "Re-run with --execute to apply the freeze."
  exit 0
fi

echo "=== FREEZE COMPLETE ==="
echo "Receipts: $DIGESTS_FILE"
echo "          $WORKFLOWS_FILE"
echo
echo "Next steps:"
echo "  - Verify by running: gh workflow list -R Molecule-AI/molecule-core | grep publish-runtime"
echo "    Status should be 'disabled_manually'."
echo "  - Demo proceeds; new workspaces pull the snapshotted :latest digests."
echo "  - Post-demo, run: scripts/demo-thaw.sh ${TS}"
echo "    to re-enable every workflow this freeze disabled."
echo
if [ $PARTIAL_FAIL -ne 0 ]; then
  echo "WARNING: one or more workflows did not disable cleanly. Re-run after fixing." >&2
  exit 2
fi
exit 0
