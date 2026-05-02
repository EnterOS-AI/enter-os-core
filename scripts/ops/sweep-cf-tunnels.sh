#!/usr/bin/env bash
# sweep-cf-tunnels.sh — safe, targeted sweep of Cloudflare Tunnels
# whose corresponding tenant no longer exists.
#
# Why this exists: CP's tenant-delete cascade removes the DNS record
# (caught by sweep-cf-orphans.sh as a backstop) but does NOT delete
# the underlying Cloudflare Tunnel. Each E2E provision creates one
# Tunnel named `tenant-<slug>`; without cleanup these accumulate
# indefinitely on the account, consuming the account's tunnel quota
# and cluttering the Cloudflare dashboard.
#
# Observed 2026-04-30: dozens of `tenant-e2e-canvas-*` tunnels in
# Down state with zero replicas, weeks past their tenant's deletion.
#
# This script is a parallel-shape janitor to sweep-cf-orphans.sh:
#   1. Query CP admin API to enumerate live org slugs (prod + staging)
#   2. Enumerate Cloudflare Tunnels via the account-scoped API
#   3. For each tunnel matching `tenant-<slug>`, check if <slug>
#      appears in the live set
#   4. Skip tunnels with active connections (defense-in-depth — never
#      delete a healthy tunnel even if CP claims the org is gone)
#   5. Only delete tunnels with NO live counterpart AND NO active
#      connections
#
# Dry-run by default; must pass --execute to actually delete.
#
# Env vars required:
#   CF_API_TOKEN        — Cloudflare token with
#                          account:cloudflare_tunnel:edit scope.
#                          (Same secret as sweep-cf-orphans, but the
#                          token must include the tunnel scope.)
#   CF_ACCOUNT_ID       — the account that owns the tunnels (visible
#                          in dash.cloudflare.com URL path)
#   CP_PROD_ADMIN_TOKEN — CP admin bearer for api.moleculesai.app
#   CP_STAGING_ADMIN_TOKEN — CP admin bearer for staging-api.moleculesai.app
#
# Exit codes:
#   0  — dry-run completed or sweep executed successfully
#   1  — missing required env, API failure, or unexpected state
#   2  — safety check failed (would delete >MAX_DELETE_PCT% of
#         tenant-shaped tunnels; refusing)

set -euo pipefail

DRY_RUN=1
# Tenant tunnels are short-lived by design — most of them at any
# given moment are orphans from finished E2E runs. The default is
# tuned higher than sweep-cf-orphans (50%) to reflect that the
# steady-state for tenant-* tunnels is mostly-orphan, not mostly-live.
MAX_DELETE_PCT="${MAX_DELETE_PCT:-90}"

for arg in "$@"; do
  case "$arg" in
    --execute|--no-dry-run) DRY_RUN=0 ;;
    --help|-h)
      grep '^#' "$0" | head -45 | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown arg: $arg (use --help)" >&2
      exit 1
      ;;
  esac
done

need() {
  local var="$1"
  if [ -z "${!var:-}" ]; then
    echo "ERROR: $var is required" >&2
    exit 1
  fi
}
need CF_API_TOKEN
need CF_ACCOUNT_ID
need CP_PROD_ADMIN_TOKEN
need CP_STAGING_ADMIN_TOKEN

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# --- Gather live sets ------------------------------------------------------

log "Fetching CP prod org slugs..."
PROD_SLUGS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_PROD_ADMIN_TOKEN" \
  "https://api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['slug'] for o in json.load(sys.stdin).get('orgs',[])))")
log "  prod orgs: $(echo "$PROD_SLUGS" | wc -w | tr -d ' ')"

log "Fetching CP staging org slugs..."
STAGING_SLUGS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_STAGING_ADMIN_TOKEN" \
  "https://staging-api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['slug'] for o in json.load(sys.stdin).get('orgs',[])))")
log "  staging orgs: $(echo "$STAGING_SLUGS" | wc -w | tr -d ' ')"

log "Fetching Cloudflare tunnels..."
# The cfd_tunnel list endpoint is paginated; per_page max is 50.
# Walk all pages so we don't silently miss orphans on busy accounts.
#
# Pages are buffered to a temp dir and merged at the end. The earlier
# shape passed the accumulating JSON on argv every iteration, which on
# a busy account (700+ tunnels = 14+ pages) blows past Linux ARG_MAX
# (~128 KB combined argv+envp on the GH Ubuntu runner) and dies with
# `python3: Argument list too long`. Disk-buffering also makes the
# accumulator O(n) instead of O(n^2).
PAGES_DIR=$(mktemp -d -t cf-tunnels-XXXXXX)
trap 'rm -rf "$PAGES_DIR"' EXIT
PAGE=1
while :; do
  page_file="$PAGES_DIR/page-$(printf '%05d' "$PAGE").json"
  curl -sS -m 15 -H "Authorization: Bearer $CF_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CF_ACCOUNT_ID/cfd_tunnel?per_page=50&page=$PAGE&is_deleted=false" \
    > "$page_file"
  page_count=$(python3 -c "import json,sys; print(len(json.load(open(sys.argv[1])).get('result') or []))" "$page_file")
  if [ "$page_count" = "0" ]; then rm -f "$page_file"; break; fi
  PAGE=$((PAGE + 1))
  if [ "$PAGE" -gt 40 ]; then
    log "::warning::stopping pagination at page 40 (2000 tunnels) — re-run if more"
    break
  fi
done
TUNNEL_JSON=$(python3 -c '
import glob, json, os, sys
acc = {"result": []}
for f in sorted(glob.glob(os.path.join(sys.argv[1], "page-*.json"))):
    with open(f) as fh:
        acc["result"].extend(json.load(fh).get("result") or [])
print(json.dumps(acc))
' "$PAGES_DIR")
TOTAL_TUNNELS=$(echo "$TUNNEL_JSON" | python3 -c "import json,sys; print(len(json.load(sys.stdin)['result']))")
log "  total tunnels: $TOTAL_TUNNELS"

# --- Compute orphans -------------------------------------------------------
#
# Rules (in order):
#   1. Name doesn't match `tenant-<slug>` → keep (unknown — never sweep
#      arbitrary tunnels that might belong to platform infra).
#   2. Tunnel has active connections (status=healthy or non-empty
#      connections array) → keep (defense-in-depth: don't kill a live
#      tunnel even if CP forgot the org).
#   3. Slug ∈ {prod_slugs ∪ staging_slugs} → keep (live tenant).
#   4. Otherwise → delete (orphan).

export PROD_SLUGS STAGING_SLUGS
DECISIONS=$(echo "$TUNNEL_JSON" | python3 -c '
import json, os, re, sys

prod_slugs = set(os.environ["PROD_SLUGS"].split())
staging_slugs = set(os.environ["STAGING_SLUGS"].split())
all_slugs = prod_slugs | staging_slugs

_TENANT_RE = re.compile(r"^tenant-(.+)$")

def decide(t, all_slugs):
    name = t.get("name", "")
    tid = t.get("id", "")
    status = t.get("status", "")
    conns = t.get("connections") or []

    m = _TENANT_RE.match(name)
    if not m:
        return ("keep", "not-a-tenant-tunnel", tid, name, status)

    slug = m.group(1)

    # Defense-in-depth: never delete a tunnel with live connectors.
    # The CF tunnel "status" field is one of inactive/degraded/healthy/down.
    # "down" with empty connections is the orphan state we sweep.
    if status == "healthy" or len(conns) > 0:
        return ("keep", "active-connections", tid, name, status)

    if slug in all_slugs:
        return ("keep", "live-tenant", tid, name, status)

    return ("delete", "orphan-tenant", tid, name, status)

d = json.loads(sys.stdin.read())
for t in d.get("result", []):
    action, reason, tid, name, status = decide(t, all_slugs)
    print(json.dumps({"action": action, "reason": reason, "id": tid, "name": name, "status": status}))
')

# --- Summarize + safety gate ----------------------------------------------

DELETE_COUNT=$(echo "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['action']=='delete'))")
KEEP_COUNT=$((TOTAL_TUNNELS - DELETE_COUNT))
TENANT_TUNNELS=$(echo "$DECISIONS" | python3 -c "
import json, sys
n = sum(1 for l in sys.stdin if json.loads(l)['reason'] != 'not-a-tenant-tunnel')
print(n)
")

log ""
log "== Sweep plan =="
log "  total tunnels:          $TOTAL_TUNNELS"
log "  tenant-shaped tunnels:  $TENANT_TUNNELS"
log "  would delete:           $DELETE_COUNT"
log "  would keep:             $KEEP_COUNT"
log ""

# Per-reason breakdown of deletes
echo "$DECISIONS" | python3 -c "
import json,sys,collections
c = collections.Counter()
for l in sys.stdin:
    d = json.loads(l)
    if d['action'] == 'delete':
        c[d['reason']] += 1
for reason, n in c.most_common():
    print(f'  delete/{reason}: {n}')
"

# Safety gate operates against the tenant-shaped subset (the reasonable
# "all of these could conceivably be ours" denominator), not the total.
# A miscount of platform-infra tunnels shouldn't relax the gate.
if [ "$TENANT_TUNNELS" -gt 0 ]; then
  PCT=$(( DELETE_COUNT * 100 / TENANT_TUNNELS ))
  if [ "$PCT" -gt "$MAX_DELETE_PCT" ]; then
    log ""
    log "SAFETY: would delete $PCT% of tenant-shaped tunnels (threshold $MAX_DELETE_PCT%) — refusing."
    log "  If this is expected (e.g. major cleanup after incident), rerun with"
    log "  MAX_DELETE_PCT=$((PCT+5)) $0 $*"
    exit 2
  fi
fi

if [ "$DRY_RUN" = "1" ]; then
  log ""
  log "Dry run complete. Pass --execute to actually delete $DELETE_COUNT tunnels."
  log ""
  log "First 20 tunnels that would be deleted:"
  echo "$DECISIONS" | python3 -c "
import json, sys
shown = 0
for l in sys.stdin:
    d = json.loads(l)
    if d['action'] == 'delete':
        print(f\"  {d['reason']:25s}  {d['name']:40s}  status={d['status']}\")
        shown += 1
        if shown >= 20: break
"
  exit 0
fi

# --- Execute deletes -------------------------------------------------------

log ""
log "Executing $DELETE_COUNT deletions..."
DELETED=0
FAILED=0
while IFS= read -r line; do
  action=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['action'])")
  [ "$action" = "delete" ] || continue
  tid=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['id'])")
  name=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['name'])")
  if curl -sS -m 10 -X DELETE \
      -H "Authorization: Bearer $CF_API_TOKEN" \
      "https://api.cloudflare.com/client/v4/accounts/$CF_ACCOUNT_ID/cfd_tunnel/$tid" \
      | grep -q '"success":true'; then
    DELETED=$((DELETED+1))
  else
    FAILED=$((FAILED+1))
    log "  FAILED: $name ($tid)"
  fi
done <<< "$DECISIONS"

log ""
log "Done. deleted=$DELETED failed=$FAILED"
[ "$FAILED" -eq 0 ]
