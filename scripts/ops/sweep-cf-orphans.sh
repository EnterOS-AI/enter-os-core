#!/usr/bin/env bash
# sweep-cf-orphans.sh — safe, targeted sweep of Cloudflare DNS records whose
# corresponding workspace/tenant no longer exists.
#
# Why this exists: tenant.Delete + workspace.Delete don't currently clean
# their CF records — see #1976. Until that lands, records accumulate at
# ~10/hour under normal E2E cadence. The old "sweep when >65" approach
# (deletes every record matching a pattern, regardless of liveness) was a
# panic button that would nuke live workspaces too.
#
# This script is the do-it-right version:
#   1. Query CP admin API to enumerate live org slugs
#   2. Query AWS EC2 to enumerate live workspace Name tags
#   3. For each CF record matching the sweep patterns, check if the
#      corresponding slug / ws-id appears in the live sets
#   4. Only delete records with NO live counterpart
#
# Dry-run by default; must pass --execute to actually delete.
#
# Env vars required:
#   CF_API_TOKEN        — Cloudflare token with zone:dns:edit
#   CF_ZONE_ID          — the zone (moleculesai.app)
#   CP_ADMIN_API_TOKEN — CP admin bearer for api.moleculesai.app
#   CP_STAGING_ADMIN_API_TOKEN — CP admin bearer for staging-api.moleculesai.app
#   AWS_*               — standard AWS creds (default region us-east-2)
#
# Exit codes:
#   0  — dry-run completed or sweep executed successfully
#   1  — missing required env, API failure, or unexpected state
#   2  — safety check failed (would delete >50% of records; refusing)

set -euo pipefail

DRY_RUN=1
MAX_DELETE_PCT="${MAX_DELETE_PCT:-50}"   # refuse to delete more than this pct of records in one run; caller can override via env
REGION="${AWS_DEFAULT_REGION:-us-east-2}"

for arg in "$@"; do
  case "$arg" in
    --execute|--no-dry-run) DRY_RUN=0 ;;
    --help|-h)
      grep '^#' "$0" | head -35 | sed 's/^# \{0,1\}//'
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
need CF_ZONE_ID
need CP_ADMIN_API_TOKEN
need CP_STAGING_ADMIN_API_TOKEN

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# --- Gather live sets ------------------------------------------------------

log "Fetching CP prod org slugs..."
PROD_SLUGS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  "https://api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['slug'] for o in json.load(sys.stdin).get('orgs',[])))")
log "  prod orgs: $(echo "$PROD_SLUGS" | wc -w | tr -d ' ')"

log "Fetching CP staging org slugs..."
STAGING_SLUGS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_STAGING_ADMIN_API_TOKEN" \
  "https://staging-api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['slug'] for o in json.load(sys.stdin).get('orgs',[])))")
log "  staging orgs: $(echo "$STAGING_SLUGS" | wc -w | tr -d ' ')"

log "Fetching live EC2 Name tags (region=$REGION)..."
# Use JSON output + python — AWS CLI's --query with nested filters has
# surprising flattening behavior that dropped tags silently on first attempt.
EC2_NAMES=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=instance-state-name,Values=running,pending" \
  --output json 2>/dev/null | python3 -c '
import json, sys
out = []
for r in json.load(sys.stdin).get("Reservations", []):
    for inst in r.get("Instances", []):
        for t in inst.get("Tags", []):
            if t.get("Key") == "Name" and t.get("Value"):
                out.append(t["Value"])
print(" ".join(out))
')
log "  live EC2s: $(echo "$EC2_NAMES" | wc -w | tr -d ' ')"

log "Fetching Cloudflare DNS records..."
CF_JSON=$(curl -sS -m 15 -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records?per_page=500")
if ! echo "$CF_JSON" | python3 -c '
import json, sys

try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"ERROR: Cloudflare returned non-JSON response: {exc}", file=sys.stderr)
    raise SystemExit(1)

if not payload.get("success", False) or not isinstance(payload.get("result"), list):
    errors = payload.get("errors") or []
    if errors:
        detail = "; ".join(
            "{code}: {message}".format(
                code=err.get("code", "unknown"),
                message=err.get("message", "unknown error"),
            )
            for err in errors
        )
    else:
        detail = "unexpected result type {}".format(type(payload.get("result")).__name__)
    print(f"ERROR: Cloudflare DNS list failed: {detail}", file=sys.stderr)
    raise SystemExit(1)
'; then
  log "Cloudflare DNS list failed; verify CF_API_TOKEN has Zone:DNS:Edit and CF_ZONE_ID is the moleculesai.app zone."
  exit 1
fi
TOTAL_CF=$(echo "$CF_JSON" | python3 -c "import json,sys; print(len(json.load(sys.stdin)['result']))")
log "  CF records: $TOTAL_CF"

# --- Compute orphans -------------------------------------------------------

# We emit NDJSON so downstream can pipe into jq etc. Each line is one decision.
# Fields: action=keep|delete, reason, id, name, type.
#
# Rules (in order of priority — first match wins):
#   1. Platform-core (api, app, doc, apex, www, _vercel, _domainkey, _railway-verify,
#      send, status, MX root) → always keep.
#   2. Tenant subdomain `<slug>.moleculesai.app` or `<slug>.staging.moleculesai.app`
#      → keep if <slug> ∈ {prod_slugs ∪ staging_slugs}, else delete.
#   3. ws-<id8>.moleculesai.app / ws-<id8>.staging.moleculesai.app
#      → keep if ws-<id8>* matches any live EC2 Name (prefix match), else delete.
#   4. e2e-<slug>.staging.moleculesai.app (or canary/canvas variants)
#      → keep if <slug> ∈ {prod_slugs ∪ staging_slugs}, else delete.
#   5. Anything else → keep (we only sweep patterns we understand).

export PROD_SLUGS STAGING_SLUGS EC2_NAMES TOTAL_CF
# Edits inside the CANONICAL DECIDE block below must mirror
# scripts/ops/sweep_cf_decide.py — the parity test in
# test_sweep_cf_decide.py asserts they match byte-for-byte.
DECISIONS=$(echo "$CF_JSON" | python3 -c '
import json, os, re, sys
d = json.load(sys.stdin)
prod_slugs = set(os.environ["PROD_SLUGS"].split())
staging_slugs = set(os.environ["STAGING_SLUGS"].split())
all_slugs = prod_slugs | staging_slugs
ec2_names = set(n for n in os.environ["EC2_NAMES"].split() if n)

_PLATFORM_CORE_NAMES = {
    "api.moleculesai.app", "app.moleculesai.app", "doc.moleculesai.app",
    "send.moleculesai.app", "status.moleculesai.app", "www.moleculesai.app",
    "staging-api.moleculesai.app",
}
_WS_RE = re.compile(r"^(ws-[a-f0-9]{8}-[a-f0-9]+)(?:\.staging)?\.moleculesai\.app$")
_E2E_RE = re.compile(r"^(e2e-[^.]+)(?:\.staging)?\.moleculesai\.app$")
_TENANT_RE = re.compile(r"^([a-z0-9][a-z0-9-]*)(?:\.staging)?\.moleculesai\.app$")

# CANONICAL DECIDE BEGIN
def decide(r, all_slugs, ec2_names):
    n = r["name"]
    rid = r["id"]
    typ = r["type"]

    if n == "moleculesai.app":
        return ("keep", "apex", rid, n, typ)
    if n.startswith("_") or n.endswith("._domainkey.moleculesai.app"):
        return ("keep", "verification/key", rid, n, typ)
    if n in _PLATFORM_CORE_NAMES:
        return ("keep", "platform-core", rid, n, typ)

    m = _WS_RE.match(n)
    if m:
        prefix = m.group(1)
        # Live EC2 names share the ws-<hex8>-<rest> shape with the DNS subdomain.
        for ename in ec2_names:
            if ename.startswith(prefix):
                return ("keep", "live-ec2", rid, n, typ)
        return ("delete", "orphan-ws", rid, n, typ)

    m = _E2E_RE.match(n)
    if m:
        slug = m.group(1)
        if slug in all_slugs:
            return ("keep", "live-e2e-tenant", rid, n, typ)
        return ("delete", "orphan-e2e-tenant", rid, n, typ)

    m = _TENANT_RE.match(n)
    if m:
        slug = m.group(1)
        if slug in all_slugs:
            return ("keep", "live-tenant", rid, n, typ)
        # KEEP unknown tenant-shaped names — avoid false-positive nukes on
        # ad-hoc records (e.g. hermes-final-*) that do not match a known slug.
        return ("keep", "unknown-subdomain-kept-for-safety", rid, n, typ)

    return ("keep", "not-a-pattern-we-sweep", rid, n, typ)
# CANONICAL DECIDE END

for r in d["result"]:
    action, reason, rid, name, typ = decide(r, all_slugs, ec2_names)
    print(json.dumps({"action": action, "reason": reason, "id": rid, "name": name, "type": typ}))
')

# --- Summarize + safety gate ----------------------------------------------

DELETE_COUNT=$(echo "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['action']=='delete'))")
KEEP_COUNT=$((TOTAL_CF - DELETE_COUNT))

log ""
log "== Sweep plan =="
log "  total CF records: $TOTAL_CF"
log "  would delete:     $DELETE_COUNT"
log "  would keep:       $KEEP_COUNT"
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

# Safety gate: refuse to delete more than MAX_DELETE_PCT of records. If we
# hit this, something is wrong — probably CP admin API returned no orgs,
# making every tenant look orphan. Bail before nuking production.
if [ "$TOTAL_CF" -gt 0 ]; then
  PCT=$(( DELETE_COUNT * 100 / TOTAL_CF ))
  if [ "$PCT" -gt "$MAX_DELETE_PCT" ]; then
    log ""
    log "SAFETY: would delete $PCT% of records (threshold $MAX_DELETE_PCT%) — refusing."
    log "  If this is expected (e.g. major cleanup after incident), rerun with"
    log "  MAX_DELETE_PCT=$((PCT+5)) $0 $*"
    exit 2
  fi
fi

if [ "$DRY_RUN" = "1" ]; then
  log ""
  log "Dry run complete. Pass --execute to actually delete $DELETE_COUNT records."
  log ""
  log "First 20 records that would be deleted:"
  echo "$DECISIONS" | python3 -c "
import json, sys
for i, l in enumerate(sys.stdin):
    d = json.loads(l)
    if d['action'] == 'delete':
        print(f\"  {d['reason']:25s}  {d['name']}\")
        if i > 50: break
" | head -20
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
  rid=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['id'])")
  name=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['name'])")
  if curl -sS -m 10 -X DELETE \
      -H "Authorization: Bearer $CF_API_TOKEN" \
      "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$rid" \
      | grep -q '"success":true'; then
    DELETED=$((DELETED+1))
  else
    FAILED=$((FAILED+1))
    log "  FAILED: $name ($rid)"
  fi
done <<< "$DECISIONS"

log ""
log "Done. deleted=$DELETED failed=$FAILED"
[ "$FAILED" -eq 0 ]
