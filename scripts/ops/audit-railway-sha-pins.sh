#!/usr/bin/env bash
# Audit Railway env vars for drift-prone image-tag pins.
#
# Background (#2001): on 2026-04-24 a stale `:staging-a14cf86` SHA pin
# in CP's TENANT_IMAGE caused 3+ hours of E2E failure with the
# appearance that "every fix didn't propagate" — really the tenant
# image was so old it didn't read the env vars those fixes produced.
# This script flags anywhere we've re-introduced that pattern.
#
# Pattern matched: any env-var value ending in `<branch>-<hex>` (e.g.
# `staging-a14cf86`) or `:vN.M.P` semver tag, OR containing such a
# substring (catches embedded refs like `repo/img:staging-abc1234`).
# Floating tags (`:staging-latest`, `:main`, `:latest`) and other
# values pass through untouched.
#
# Usage:
#   bash scripts/ops/audit-railway-sha-pins.sh                 # both envs
#   bash scripts/ops/audit-railway-sha-pins.sh production      # one env
#   bash scripts/ops/audit-railway-sha-pins.sh staging
#
# Exit codes:
#   0 — no drift-prone pins
#   1 — drift detected, list printed
#   2 — railway CLI unauthenticated / project unlinked
#
# Pre-req: run from a directory linked to a Railway project
# (e.g. molecule-controlplane). The script does not chdir for you
# because the linked project's identity matters.
set -euo pipefail

ENV_FILTER="${1:-}"
ENVS=()
case "$ENV_FILTER" in
  "") ENVS=(production staging) ;;
  production|staging) ENVS=("$ENV_FILTER") ;;
  *) echo "usage: $0 [production|staging]" >&2; exit 2 ;;
esac

# All services in the linked Railway project. Discovery isn't worth
# the complexity — list them explicitly and add new services here.
SERVICES=(controlplane)

# A single regex that matches:
#   - `<branch>-<hex>` at end of value
#   - `:vN.M.P` semver tag at end
#   - either pattern as a substring
# Drift-prone patterns — same class as the 2026-04-24 TENANT_IMAGE
# incident. Matched against full env-var lines (KEY=VALUE).
#
#  branch-SHA  (e.g. `staging-a14cf86`):
#      anchored by branch-name prefix + 6+ hex chars, so a UUID hex
#      run that happens to look hex-shaped doesn't trip the audit
#      (UUIDs use dashes, ARNs use colons).
#
#  semver pin (`:v1.2.3`, `=v0.1.16`):
#      requires `:` or `=` immediately before, so prose like
#      "version 1.2.3 of the api" is NOT flagged. The trailing
#      negated-class ensures we don't fold patches like 1.2.34
#      into 1.2.3.
DRIFT_REGEX='(staging|main|prod|production)-[a-f0-9]{6,}|[:=]v?[0-9]+\.[0-9]+\.[0-9]+([^a-z0-9]|$)'

drift_count=0
for env in "${ENVS[@]}"; do
  for svc in "${SERVICES[@]}"; do
    echo "─── env=$env  service=$svc ───"
    if ! out=$(railway variables --service "$svc" --environment "$env" --kv 2>&1); then
      # Detect "not authenticated" / "no linked project" vs "service not found"
      if echo "$out" | grep -qiE 'not (authenticated|logged in)|unlinked|no project'; then
        echo "  ❌ railway CLI not authenticated or project not linked" >&2
        exit 2
      fi
      echo "  (skipped: $out)" >&2
      continue
    fi
    matched=$(echo "$out" | grep -nE "=.*($DRIFT_REGEX)" || true)
    if [ -z "$matched" ]; then
      total=$(echo "$out" | grep -c '=' || echo 0)
      echo "  ✓ $total env vars audited, no drift-prone pins"
    else
      lines=$(echo "$matched" | wc -l | tr -d ' ')
      drift_count=$((drift_count + lines))
      echo "  ⚠ $lines drift-prone pin(s):"
      # Truncate values past 80 chars so a tokenful one-liner doesn't
      # hide the relevant suffix off-screen.
      echo "$matched" | sed -E 's/(.{80}).+/\1.../' | sed 's/^/    /'
    fi
  done
done

if [ "$drift_count" -gt 0 ]; then
  echo
  echo "Total drift-prone pins: $drift_count"
  echo "Replace with floating tags (e.g. :staging-latest, :main) unless"
  echo "intentional and documented in the ops runbook."
  exit 1
fi
echo
echo "✓ Clean — no drift-prone image pins in any audited env."
exit 0
