#!/usr/bin/env bash
# prune_cf_e2e_dns.sh — targeted, fail-closed cleanup of disposable E2E DNS
# records that accumulate under the moleculesai.app zone (typically the
# staging.moleculesai.app subdomain) and exhaust the Cloudflare DNS record
# quota (code 81045).
#
# Why this exists: staging E2E harnesses create DNS records for slugs like
# e2e-smoke-<date>-<run>-<uuid> and e2e-tmpl-<rand> (see
# tests/e2e/test_staging_full_saas.sh and tests/e2e/test_template_delivery_e2e.sh).
# When teardown is skipped (CI cancellation, runner crash, transient CP error),
# these records leak. Cloudflare caps DNS records per zone; once the cap is
# hit, new tenant provisioning fails with CF code 81045. This script is the
# immediate unblock tool: it deletes clearly-ephemeral test records by pattern
# + age, independent of CP state.
#
# Scope (conservative):
#   - Records whose full name matches
#       e2e-smoke-*.<zone-domain>
#       e2e-tmpl-*.<zone-domain>
#   - Multiple zone domains may be supplied as a comma-separated list
#     (e.g. "moleculesai.app,staging.moleculesai.app").
#   - Records older than --min-age-hours / PRUNE_MIN_AGE_HOURS (default 24)
#     so in-flight runs are not touched.
#   - Anything else is kept untouched.
#
# Dry-run by default; must pass --apply (or set PRUNE_APPLY=1) to delete.
#
# Required env:
#   CF_API_TOKEN   — Cloudflare API token with Zone:DNS:Edit on the target zone.
#                    Falls back to CLOUDFLARE_API_TOKEN.
#   CF_ZONE_ID     — Cloudflare zone id for moleculesai.app (or staging zone).
#                    Falls back to CLOUDFLARE_ZONE_ID.
#
# Optional env:
#   PRUNE_APPLY=1            — same as --apply (both accepted).
#   PRUNE_MIN_AGE_HOURS=<int> — default minimum age in hours (default: 24).
#   MAX_DELETE_PCT=<int>     — refuse to delete more than this percentage of
#                              matched ephemeral records (default: 50).
#   PRUNE_ZONE_DOMAIN=<domain> — comma-separated zone domain(s) to anchor
#                                matches (default: staging.moleculesai.app).
#
# Exit codes:
#   0  — dry-run completed or prune executed successfully
#   1  — missing required env, API failure, or unexpected state
#   2  — safety gate refused the prune

set -euo pipefail

DRY_RUN=1
MIN_AGE_HOURS="${PRUNE_MIN_AGE_HOURS:-24}"
MAX_DELETE_PCT="${MAX_DELETE_PCT:-50}"
ZONE_DOMAIN="${PRUNE_ZONE_DOMAIN:-}"
if [ -z "$ZONE_DOMAIN" ]; then
  ZONE_DOMAIN="staging.moleculesai.app"
fi

while [ $# -gt 0 ]; do
  case "$1" in
    --apply|--execute|--no-dry-run) DRY_RUN=0; shift ;;
    --min-age-hours)
      shift
      MIN_AGE_HOURS="${1:-}"
      if ! [[ "$MIN_AGE_HOURS" =~ ^[0-9]+$ ]]; then
        echo "ERROR: --min-age-hours requires a non-negative integer" >&2
        exit 1
      fi
      shift
      ;;
    --help|-h)
      sed -n '1,/^set -euo pipefail$/p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    --*)
      echo "unknown arg: $1 (use --help)" >&2
      exit 1
      ;;
    *)
      echo "unknown arg: $1 (use --help)" >&2
      exit 1
      ;;
  esac
done

if [ "${PRUNE_APPLY:-0}" = "1" ]; then
  DRY_RUN=0
fi

need() {
  local var="$1"
  if [ -z "${!var:-}" ]; then
    echo "ERROR: $var is required" >&2
    exit 1
  fi
}

# Accept canonical operator-host names OR CI-scoped names.
CF_API_TOKEN="${CF_API_TOKEN:-${CLOUDFLARE_API_TOKEN:-}}"
CF_ZONE_ID="${CF_ZONE_ID:-${CLOUDFLARE_ZONE_ID:-}}"

need CF_API_TOKEN
need CF_ZONE_ID

if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl is required" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required" >&2
  exit 1
fi

if ! [[ "$MIN_AGE_HOURS" =~ ^[0-9]+$ ]]; then
  echo "ERROR: PRUNE_MIN_AGE_HOURS/--min-age-hours must be a non-negative integer" >&2
  exit 1
fi

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# --- Preflight: verify CF token + zone BEFORE any list/delete work ---------
log "Preflight: verifying CF token + zone..."
PF_TOKEN_JSON=$(curl -sS -m 10 -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/user/tokens/verify")
if ! echo "$PF_TOKEN_JSON" | python3 -c '
import json, sys
try:
    p = json.load(sys.stdin)
except Exception as exc:
    print(f"ERROR: non-JSON from /user/tokens/verify: {exc}", file=sys.stderr)
    raise SystemExit(1)
if not p.get("success"):
    errs = p.get("errors") or []
    detail = "; ".join(
        "{code}: {msg}".format(code=e.get("code", "?"), msg=e.get("message", "?"))
        for e in errs
    ) or "unknown"
    print(f"ERROR: CF token verify returned success=false: {detail}", file=sys.stderr)
    raise SystemExit(1)
status = (p.get("result") or {}).get("status", "?")
if status != "active":
    print(f"ERROR: CF token is not active (status={status})", file=sys.stderr)
    raise SystemExit(1)
'; then
  log "  CF token preflight FAILED — verify CF_API_TOKEN/CLOUDFLARE_API_TOKEN is active."
  exit 1
fi
log "  CF token active ✓"

PF_ZONE_JSON=$(curl -sS -m 10 -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID")
if ! echo "$PF_ZONE_JSON" | CF_ZONE_ID="$CF_ZONE_ID" python3 -c '
import json, os, sys
try:
    p = json.load(sys.stdin)
except Exception as exc:
    print(f"ERROR: non-JSON from /zones/{os.environ['CF_ZONE_ID']}: {exc}", file=sys.stderr)
    raise SystemExit(1)
if not p.get("success"):
    errs = p.get("errors") or []
    detail = "; ".join(
        "{code}: {msg}".format(code=e.get("code", "?"), msg=e.get("message", "?"))
        for e in errs
    ) or "unknown"
    print(f"ERROR: zone lookup returned success=false: {detail}", file=sys.stderr)
    raise SystemExit(1)
res = p.get("result") or {}
if res.get("id") != os.environ["CF_ZONE_ID"]:
    print("ERROR: zone id mismatch", file=sys.stderr)
    raise SystemExit(1)
'; then
  log "  CF zone preflight FAILED — verify CF_ZONE_ID/CLOUDFLARE_ZONE_ID and Zone:Read permission."
  exit 1
fi
log "  zone $CF_ZONE_ID reachable ✓"

# --- Gather DNS records with explicit pagination ----------------------------
log "Fetching DNS records from zone $CF_ZONE_ID (paginated)..."
PAGES_DIR=$(mktemp -d -t cf-dns-XXXXXX)
PLAN_FILE=""
FAIL_LOG=""
cleanup() {
  rm -rf "$PAGES_DIR"
  [ -n "$PLAN_FILE" ] && rm -f "$PLAN_FILE"
  [ -n "$FAIL_LOG" ] && rm -f "$FAIL_LOG"
  return 0
}
trap cleanup EXIT

PAGE=1
NEXT_PAGE=1
while [ -n "$NEXT_PAGE" ]; do
  page_file="$PAGES_DIR/page-$(printf '%05d' "$PAGE").json"
  curl -sS -m 30 -f -H "Authorization: Bearer $CF_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records?per_page=100&page=$NEXT_PAGE" \
    > "$page_file" || {
      log "ERROR: CF DNS list page $NEXT_PAGE failed (non-2xx or network error)."
      exit 1
    }

  if ! python3 -c '
import json, sys
try:
    p = json.load(open(sys.argv[1]))
except Exception as exc:
    print(f"ERROR: non-JSON list response: {exc}", file=sys.stderr)
    raise SystemExit(1)
if not p.get("success"):
    errs = p.get("errors") or []
    detail = "; ".join("{code}: {msg}".format(code=e.get("code","?"), msg=e.get("message","?")) for e in errs) or "unknown"
    print(f"ERROR: CF DNS list returned success=false: {detail}", file=sys.stderr)
    raise SystemExit(1)
if not isinstance(p.get("result"), list):
    print("ERROR: CF DNS list result is not a list", file=sys.stderr)
    raise SystemExit(1)
' "$page_file"; then
    log "ERROR: CF DNS list page $NEXT_PAGE returned errors or malformed JSON"
    exit 1
  fi

  HAS_MORE=$(python3 -c '
import json, sys
p = json.load(open(sys.argv[1]))
ri = p.get("result_info") or {}
print(1 if ri.get("page", 0) < ri.get("total_pages", 0) else "")
' "$page_file")
  PAGE=$((PAGE + 1))
  if [ -z "$HAS_MORE" ]; then
    NEXT_PAGE=""
  else
    NEXT_PAGE=$PAGE
  fi
  if [ "$PAGE" -gt 500 ]; then
    log "::warning::stopping pagination at page 500 (50k records) — re-run if more"
    break
  fi
done

CF_JSON=$(python3 -c '
import glob, json, os, sys
acc = {"result": []}
for f in sorted(glob.glob(os.path.join(sys.argv[1], "page-*.json"))):
    with open(f) as fh:
        acc["result"].extend(json.load(fh).get("result") or [])
print(json.dumps(acc))
' "$PAGES_DIR")
TOTAL_CF=$(echo "$CF_JSON" | python3 -c "import json,sys; print(len(json.load(sys.stdin)['result']))")
log "  total CF records: $TOTAL_CF"

# --- Compute targets ---------------------------------------------------------
export MIN_AGE_HOURS ZONE_DOMAIN
DECISIONS=$(echo "$CF_JSON" | python3 -c '
import json, os, re, sys
from datetime import datetime, timezone, timedelta

min_age = timedelta(hours=int(os.environ["MIN_AGE_HOURS"]))
zone_domain = os.environ["ZONE_DOMAIN"]
zone_domains = [d.strip() for d in zone_domain.split(",") if d.strip()]
now = datetime.now(timezone.utc)

# Conservative: only the two known disposable E2E prefixes, anchored to one
# of the configured zone domains so similarly-named records in other zones
# never match. Multiple zone domains may be supplied as a comma-separated
# list (e.g. "moleculesai.app,staging.moleculesai.app").
# The prefix MUST be followed immediately by a hyphen and then at least one
# suffix character, so names like e2e-smokeprod, e2e-smoketest-keep, or
# prod-e2e-smoke-x are NEVER matched.
zone_part = r"(?:" + r"|".join(re.escape(d) for d in zone_domains) + r")"
EPHEMERAL_RE = re.compile(
    r"^(e2e-smoke-[a-zA-Z0-9_-]+|e2e-tmpl-[a-zA-Z0-9_-]+)\." + zone_part + r"$"
)

def parse_iso(s):
    if not s:
        return None
    s = s.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None

def decide(r):
    rid = r.get("id", "")
    name = r.get("name", "")
    typ = r.get("type", "")
    created = parse_iso(r.get("created_on"))

    if not EPHEMERAL_RE.match(name):
        return ("keep", "not-ephemeral-pattern", rid, name, typ)

    if created is None:
        return ("keep", "missing-created_on", rid, name, typ)

    if (now - created) < min_age:
        return ("keep", "too-new", rid, name, typ)

    return ("delete", "stale-ephemeral", rid, name, typ)

d = json.load(sys.stdin)
for r in d.get("result", []):
    action, reason, rid, name, typ = decide(r)
    print(json.dumps({
        "action": action,
        "reason": reason,
        "id": rid,
        "name": name,
        "type": typ,
        "created_on": r.get("created_on", ""),
    }))
')

MATCHED_COUNT=$(printf '%s' "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['reason'] != 'not-ephemeral-pattern'))")
DELETE_COUNT=$(printf '%s' "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['action']=='delete'))")
KEEP_COUNT=$((MATCHED_COUNT - DELETE_COUNT))

log ""
log "== Prune plan =="
log "  zone domain:             $ZONE_DOMAIN"
log "  total CF records:        $TOTAL_CF"
log "  matched ephemeral shape: $MATCHED_COUNT"
log "  would delete:            $DELETE_COUNT"
log "  would keep (in scope):   $KEEP_COUNT"
log "  min-age-hours:           $MIN_AGE_HOURS"
log ""

printf '%s' "$DECISIONS" | python3 -c "
import json, sys, collections
c = collections.Counter()
for l in sys.stdin:
    d = json.loads(l)
    c[d['reason']] += 1
for reason, n in c.most_common():
    print(f'  {reason}: {n}')
"

# --- Safety gate -------------------------------------------------------------
if [ "$MATCHED_COUNT" -gt 0 ]; then
  PCT=$(( DELETE_COUNT * 100 / MATCHED_COUNT ))
  if [ "$PCT" -gt "$MAX_DELETE_PCT" ]; then
    log ""
    log "SAFETY: would delete $PCT% of matched ephemeral records (threshold $MAX_DELETE_PCT%) — refusing."
    log "  If this is expected, rerun with MAX_DELETE_PCT=$((PCT+5)) $0 $*"
    exit 2
  fi
fi

if [ "$DRY_RUN" = "1" ]; then
  log ""
  log "Dry run complete. Pass --apply (or PRUNE_APPLY=1) to delete $DELETE_COUNT records."
  log ""
  log "First 20 records that would be deleted:"
  printf '%s' "$DECISIONS" | python3 -c "
import json, sys
shown = 0
for l in sys.stdin:
    d = json.loads(l)
    if d['action'] == 'delete':
        print(f\"  {d['created_on'][:19]:20s}  {d['name']}\")
        shown += 1
        if shown >= 20: break
"
  exit 0
fi

# --- Execute deletes ---------------------------------------------------------
PLAN_FILE=$(mktemp -t cf-dns-plan-XXXXXX)
FAIL_LOG=$(mktemp -t cf-dns-fail-XXXXXX)

printf '%s' "$DECISIONS" | python3 -c '
import json, sys
with open(sys.argv[1], "w") as plan:
    for line in sys.stdin:
        d = json.loads(line)
        if d.get("action") == "delete":
            plan.write(d["id"] + "\t" + d["name"] + "\n")
' "$PLAN_FILE"

log ""
log "Executing $DELETE_COUNT deletions..."

DELETED=0
FAILED=0
while IFS=$'\t' read -r rid name; do
  [ -n "$rid" ] || continue
  if curl -sS -m 15 -f -X DELETE \
       -H "Authorization: Bearer $CF_API_TOKEN" \
       "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$rid" \
       >/dev/null 2>&1; then
    DELETED=$((DELETED + 1))
  else
    FAILED=$((FAILED + 1))
    echo "FAIL $name $rid" >> "$FAIL_LOG"
  fi
done < "$PLAN_FILE"

log ""
log "Done. deleted=$DELETED failed=$FAILED"
if [ "$FAILED" -ne 0 ]; then
  log "Failure detail (first 20):"
  head -20 "$FAIL_LOG" | while IFS= read -r fl; do log "  $fl"; done
fi
[ "$FAILED" -eq 0 ]
