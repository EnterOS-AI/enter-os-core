#!/usr/bin/env bash
# Check whether production tenants and canvas are running latest main.
#
# Usage:
#   ./scripts/ops/check-prod-versions.sh                # production
#   ENV=staging ./scripts/ops/check-prod-versions.sh    # staging tenants
#
# Outputs a table of {surface, current_sha, expected_sha, status}. Returns
# non-zero if any surface is stale so this can be wired into a periodic
# alert.
#
# Why this exists: every time someone hits a "is the fix live?" question,
# they have to remember the curl pattern + cross-reference with
# `git rev-parse origin/main`. This script does that check uniformly across
# every public surface (workspace tenants + canvas) and gives a one-line
# verdict instead of a stack of one-off curls.

set -euo pipefail

ENV="${ENV:-production}"
EXPECTED_REF="${EXPECTED_REF:-main}"

case "$ENV" in
    production)
        TENANT_DOMAIN="moleculesai.app"
        CANVAS_URL="https://canvas.moleculesai.app"
        # Default canary tenant for production. Override via TENANT_SLUGS=
        # to cover a custom set.
        DEFAULT_TENANTS="hongmingwang reno-stars"
        ;;
    staging)
        TENANT_DOMAIN="staging.moleculesai.app"
        CANVAS_URL="https://canvas-staging.moleculesai.app"
        DEFAULT_TENANTS=""  # staging tenants are ephemeral; user must specify
        ;;
    *)
        echo "Unknown ENV=$ENV (expected: production | staging)" >&2
        exit 2
        ;;
esac

TENANT_SLUGS="${TENANT_SLUGS:-$DEFAULT_TENANTS}"

# Pull EXPECTED_SHA from GitHub. Falls back to local git if gh isn't
# logged in — local main may lag origin but is usually close enough for
# debugging, and we still report the comparison clearly.
EXPECTED_SHA=""
if command -v gh >/dev/null 2>&1; then
    EXPECTED_SHA=$(gh api "repos/Molecule-AI/molecule-core/commits/${EXPECTED_REF}" --jq '.sha' 2>/dev/null || true)
fi
if [ -z "$EXPECTED_SHA" ]; then
    if git rev-parse "origin/${EXPECTED_REF}" >/dev/null 2>&1; then
        EXPECTED_SHA=$(git rev-parse "origin/${EXPECTED_REF}")
        echo "[check-prod-versions] WARN: gh unavailable, using local origin/${EXPECTED_REF}=${EXPECTED_SHA:0:7} (may lag)"
    else
        echo "[check-prod-versions] ERROR: cannot resolve expected SHA — gh not logged in and origin/${EXPECTED_REF} not fetched" >&2
        exit 2
    fi
fi
EXPECTED_SHORT="${EXPECTED_SHA:0:7}"

echo "Checking ${ENV} surfaces against ${EXPECTED_REF}=${EXPECTED_SHORT}"
echo ""
printf "%-25s  %-9s  %-9s  %s\n" "Surface" "Live" "Expected" "Status"
printf "%-25s  %-9s  %-9s  %s\n" "-------" "----" "--------" "------"

STALE_COUNT=0
UNREACHABLE_COUNT=0

# Tenant surfaces — workspace-server /buildinfo (added in PR #2398).
for slug in $TENANT_SLUGS; do
    URL="https://${slug}.${TENANT_DOMAIN}/buildinfo"
    BODY=$(curl -sS --max-time 15 "$URL" 2>/dev/null || echo "")
    ACTUAL_SHA=$(echo "$BODY" | jq -r '.git_sha // ""' 2>/dev/null || echo "")
    if [ -z "$ACTUAL_SHA" ]; then
        printf "%-25s  %-9s  %-9s  ⚠ unreachable\n" "tenant: $slug" "—" "$EXPECTED_SHORT"
        UNREACHABLE_COUNT=$((UNREACHABLE_COUNT + 1))
    elif [ "$ACTUAL_SHA" = "$EXPECTED_SHA" ]; then
        printf "%-25s  %-9s  %-9s  ✓ current\n" "tenant: $slug" "${ACTUAL_SHA:0:7}" "$EXPECTED_SHORT"
    else
        printf "%-25s  %-9s  %-9s  ✗ stale\n" "tenant: $slug" "${ACTUAL_SHA:0:7}" "$EXPECTED_SHORT"
        STALE_COUNT=$((STALE_COUNT + 1))
    fi
done

# Canvas — Next.js /api/buildinfo (PR #2407). Vercel injects
# VERCEL_GIT_COMMIT_SHA at build time so this reflects the deployed
# commit, not the request time.
CANVAS_BODY=$(curl -sS --max-time 15 "${CANVAS_URL}/api/buildinfo" 2>/dev/null || echo "")
CANVAS_SHA=$(echo "$CANVAS_BODY" | jq -r '.git_sha // ""' 2>/dev/null || echo "")
if [ -z "$CANVAS_SHA" ]; then
    printf "%-25s  %-9s  %-9s  ⚠ unreachable (route may not be deployed yet)\n" "canvas" "—" "$EXPECTED_SHORT"
    UNREACHABLE_COUNT=$((UNREACHABLE_COUNT + 1))
elif [ "$CANVAS_SHA" = "dev" ]; then
    printf "%-25s  %-9s  %-9s  ⚠ dev sentinel (Vercel env not injected — check VERCEL_GIT_COMMIT_SHA)\n" "canvas" "dev" "$EXPECTED_SHORT"
    UNREACHABLE_COUNT=$((UNREACHABLE_COUNT + 1))
elif [ "$CANVAS_SHA" = "$EXPECTED_SHA" ]; then
    printf "%-25s  %-9s  %-9s  ✓ current\n" "canvas" "${CANVAS_SHA:0:7}" "$EXPECTED_SHORT"
else
    printf "%-25s  %-9s  %-9s  ✗ stale\n" "canvas" "${CANVAS_SHA:0:7}" "$EXPECTED_SHORT"
    STALE_COUNT=$((STALE_COUNT + 1))
fi

echo ""
if [ $STALE_COUNT -eq 0 ] && [ $UNREACHABLE_COUNT -eq 0 ]; then
    echo "All surfaces current."
    exit 0
fi
echo "Summary: ${STALE_COUNT} stale, ${UNREACHABLE_COUNT} unreachable."
# Stale is a deploy gap; unreachable is operational (DNS, CF, route absent).
# Both are signal — exit non-zero so cron / CI can alert.
exit 1
