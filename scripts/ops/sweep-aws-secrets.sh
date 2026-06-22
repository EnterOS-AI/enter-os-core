#!/usr/bin/env bash
# sweep-aws-secrets.sh — safe, targeted sweep of AWS Secrets Manager
# secrets whose corresponding tenant no longer exists.
#
# Why this exists: CP's tenant-delete cascade calls
# Secrets.DeleteSecret() at deprovision time, but only when the
# deprovision flow runs to completion (provisioner/ec2.go:806). Crashed
# provisions, hard-failed E2E runs, and any tenant created without a
# matching deprovision (early-bail in provisioner, manual orchestration
# bugs) leak the per-tenant bootstrap secret. At ~$0.40/secret/month,
# 50 leaked secrets = $20/month — enough to show up on the cost
# dashboard.
#
# Observed 2026-05-03: AWS Secrets Manager line item ~$19/month with
# only one tenant currently provisioned, indicating ~45+ orphan
# secrets. The tenant_resources audit table (mig 024) tracks four
# resource kinds (Cloudflare Tunnel, Cloudflare DNS, EC2 Instance,
# Security Group) but NOT Secrets Manager — the long-term fix is to
# add KindSecretsManagerSecret + recorder hook + reconciler enumerator.
# Tracked separately as a controlplane issue.
#
# Sweeps TWO managed namespaces (both reduce to "owning org is gone"):
#   - molecule/tenant/<org_id>/bootstrap  — org_id is in the NAME.
#   - molecule/workspace/<ws_id>/config   — owning org is on the OrgID TAG
#     (cp#329 per-workspace config delivery). THIS prefix was the entire
#     ~$253/mo SM bill in June 2026 (2.4k orphan secrets from purged
#     ephemeral E2E orgs) and was NOT swept before — the old filter only
#     matched molecule/tenant/, so the janitor reported SUCCESS while
#     deleting nothing relevant. Per-workspace liveness INSIDE a still-live
#     org is owned by the CP auto-reap secrets reaper; this sweeper only
#     deletes a workspace secret whose whole org is gone (no race risk).
#
# Steps:
#   1. Query CP admin API to enumerate live org IDs (prod + staging)
#   2. Enumerate SM secrets matching either managed prefix
#   3. tenant/* → org_id from name; workspace/* → org_id from OrgID tag
#   4. Defense-in-depth: skip secrets created in the last GRACE_HOURS
#   5. Only delete secrets whose owning org is NOT in the live set AND are
#      outside the grace window
#
# Dry-run by default; must pass --execute to actually delete.
#
# Deletion semantics: RECOVERABLE 30-day delete (NOT force-delete). A
# mistaken sweep is reversible via `aws secretsmanager restore-secret`
# for 30 days. At thousands-of-secrets scale an unrecoverable bulk delete
# is an unacceptable blast radius; matches the CP provisioner + reaper.
#
# Bulk backlog: the MAX_DELETE_PCT gate will (correctly) block a genuine
# >50%-orphan backlog. To drain one deliberately, set SWEEP_ALLOW_BULK=1
# — the real safety is the live-org cross-reference + 30d recovery, not
# the percent gate.
#
# Env vars required:
#   AWS_REGION              — region the secrets live in (default: us-east-1)
#   CP_ADMIN_API_TOKEN     — CP admin bearer for api.moleculesai.app
#   CP_STAGING_ADMIN_API_TOKEN  — CP admin bearer for staging-api.moleculesai.app
#   AWS_ACCESS_KEY_ID,      — IAM principal with secretsmanager:ListSecrets
#   AWS_SECRET_ACCESS_KEY     and secretsmanager:DeleteSecret. Note: the
#                             prod molecule-cp principal does NOT have
#                             these permissions; the workflow uses a
#                             dedicated janitor principal.
#
# Exit codes:
#   0  — dry-run completed or sweep executed successfully
#   1  — missing required env, API failure, or unexpected state
#   2  — safety check failed (would delete >MAX_DELETE_PCT% of
#         managed-shaped secrets; refusing — set SWEEP_ALLOW_BULK=1 to
#         drain a deliberate backlog)

set -euo pipefail

DRY_RUN=1
# Tenant secrets are durable by design — they should track 1:1 with
# live tenants. The 50% default mirrors sweep-cf-orphans.sh (DNS
# records, also durable) rather than sweep-cf-tunnels.sh (90%, mostly
# orphan by design). If the live tenant count drops by more than half
# in one sweep window, that's an incident worth investigating before
# we erase the audit trail.
MAX_DELETE_PCT="${MAX_DELETE_PCT:-50}"
GRACE_HOURS="${GRACE_HOURS:-24}"
AWS_REGION="${AWS_REGION:-us-east-1}"

for arg in "$@"; do
  case "$arg" in
    --execute|--no-dry-run) DRY_RUN=0 ;;
    --help|-h)
      grep '^#' "$0" | head -55 | sed 's/^# \{0,1\}//'
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
need CP_ADMIN_API_TOKEN
need CP_STAGING_ADMIN_API_TOKEN
need AWS_ACCESS_KEY_ID
need AWS_SECRET_ACCESS_KEY

if ! command -v aws >/dev/null 2>&1; then
  echo "ERROR: aws cli is required" >&2
  exit 1
fi

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# --- Gather live sets ------------------------------------------------------
#
# Secret naming uses the tenant's UUID (org_id), not the slug — see
# awsapi.TenantSecretName in molecule-controlplane. The /cp/admin/orgs
# response includes both `id` and `slug`; we extract `id` here.

# Fetch org IDs from a CP admin API endpoint.
# Fail-closed: any non-2xx HTTP response, invalid JSON, or missing/invalid
# 'orgs' array aborts the sweep with a non-zero exit. This is critical under
# SWEEP_ALLOW_BULK=1, where an empty live-org set would classify every old
# managed secret as orphan.
fetch_cp_orgs() {
  local url="$1" token="$2" label="$3"
  local resp
  resp=$(curl -sS -f -m 15 -H "Authorization: Bearer $token" "$url" 2>&1) || {
    echo "ERROR: $label CP admin API request failed (non-2xx or network error)" >&2
    echo "$resp" >&2
    return 1
  }
  python3 -c "
import json, sys
try:
    d = json.loads(sys.stdin.read())
except json.JSONDecodeError as e:
    print('ERROR: $label CP admin API returned invalid JSON:', e, file=sys.stderr)
    sys.exit(1)
orgs = d.get('orgs')
if not isinstance(orgs, list):
    print('ERROR: $label CP admin API response missing or invalid \"orgs\" array', file=sys.stderr)
    sys.exit(1)
print(' '.join(o['id'] for o in orgs))
" <<< "$resp"
}

log "Fetching CP prod org ids..."
PROD_IDS=$(fetch_cp_orgs "https://api.moleculesai.app/cp/admin/orgs?limit=500" "$CP_ADMIN_API_TOKEN" "prod")
log "  prod orgs: $(echo "$PROD_IDS" | wc -w | tr -d ' ')"

log "Fetching CP staging org ids..."
STAGING_IDS=$(fetch_cp_orgs "https://staging-api.moleculesai.app/cp/admin/orgs?limit=500" "$CP_STAGING_ADMIN_API_TOKEN" "staging")
log "  staging orgs: $(echo "$STAGING_IDS" | wc -w | tr -d ' ')"

log "Fetching AWS Secrets Manager secrets (region=$AWS_REGION)..."
# list-secrets is paginated via NextToken. The aws cli auto-paginates
# unless --max-items is set, but explicit pagination keeps us safe
# from any sudden default change and lets us cap at a sane upper
# bound. ListSecrets returns up to 100 per page; we cap at 50 pages
# (5000 secrets) which is well past any plausible tenant count.
PAGES_DIR=$(mktemp -d -t aws-secrets-XXXXXX)
DELETE_PLAN=""
NAME_MAP=""
FAIL_LOG=""
RESULT_LOG=""
cleanup() {
  rm -rf "$PAGES_DIR"
  [ -n "$DELETE_PLAN" ] && rm -f "$DELETE_PLAN"
  [ -n "$NAME_MAP" ] && rm -f "$NAME_MAP"
  [ -n "$FAIL_LOG" ] && rm -f "$FAIL_LOG"
  [ -n "$RESULT_LOG" ] && rm -f "$RESULT_LOG"
  return 0
}
trap cleanup EXIT

NEXT_TOKEN=""
PAGE=1
while :; do
  page_file="$PAGES_DIR/page-$(printf '%05d' "$PAGE").json"
  # Sweep BOTH managed prefixes: molecule/tenant/* (per-org bootstrap) AND
  # molecule/workspace/* (per-workspace config, cp#329). The latter was the
  # entire ~$253/mo SM bill in June 2026 (2.4k orphan secrets) and this
  # filter never matched it before — the sweeper reported SUCCESS while
  # deleting nothing relevant. A name-filter Values list is OR-matched by
  # Secrets Manager, so this captures both namespaces in one paginated walk.
  if [ -z "$NEXT_TOKEN" ]; then
    aws secretsmanager list-secrets \
      --region "$AWS_REGION" \
      --filters Key=name,Values=molecule/tenant/,molecule/workspace/ \
      --max-results 100 \
      --output json > "$page_file"
  else
    aws secretsmanager list-secrets \
      --region "$AWS_REGION" \
      --filters Key=name,Values=molecule/tenant/,molecule/workspace/ \
      --max-results 100 \
      --next-token "$NEXT_TOKEN" \
      --output json > "$page_file"
  fi
  NEXT_TOKEN=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('NextToken') or '')" "$page_file")
  PAGE=$((PAGE + 1))
  if [ -z "$NEXT_TOKEN" ]; then break; fi
  if [ "$PAGE" -gt 100 ]; then
    log "::warning::stopping pagination at page 100 (10000 secrets) — re-run if more"
    break
  fi
done

SECRET_JSON=$(python3 -c '
import glob, json, os, sys
acc = {"SecretList": []}
for f in sorted(glob.glob(os.path.join(sys.argv[1], "page-*.json"))):
    with open(f) as fh:
        acc["SecretList"].extend(json.load(fh).get("SecretList") or [])
print(json.dumps(acc))
' "$PAGES_DIR")
TOTAL_SECRETS=$(echo "$SECRET_JSON" | python3 -c "import json,sys; print(len(json.load(sys.stdin)['SecretList']))")
log "  total tenant-prefixed secrets: $TOTAL_SECRETS"

# --- Compute orphans -------------------------------------------------------
#
# Two managed namespaces, cross-referenced against the SAME live ORG set
# (prod_ids ∪ staging_ids fetched from the CP admin API). Both reduce to
# "the owning org no longer exists ⇒ orphan", which is the safe, org-level
# signal the sweeper can establish without per-workspace liveness:
#
#   molecule/tenant/<org_id>/bootstrap  — org_id is in the NAME.
#   molecule/workspace/<ws_id>/config   — ws_id is in the name (NOT an org
#     id); the owning org is on the secret's OrgID TAG (set by the CP
#     provisioner's seedWorkspaceConfigSecret). A workspace secret whose
#     OrgID tag is not a live org is a guaranteed orphan: the whole tenant
#     is gone, so the workspace can't exist. (Per-workspace liveness inside
#     a STILL-LIVE org is owned by the CP auto-reap secrets reaper, which
#     calls the tenant /workspaces endpoint — this sweeper deliberately
#     does NOT delete a workspace secret whose org is still live, to avoid
#     racing a live tenant's in-flight workspace.)
#
# Rules (in order, per secret):
#   1. Name matches neither managed shape → keep (never sweep arbitrary
#      secrets that might belong to platform infra).
#   2. CreatedDate within $GRACE_HOURS → keep (provision-in-flight margin).
#   3. owning org ∈ {prod_ids ∪ staging_ids} → keep (live tenant).
#   4. Otherwise → delete (orphan) via 30-day RECOVERABLE delete.

export PROD_IDS STAGING_IDS GRACE_HOURS
DECISIONS=$(echo "$SECRET_JSON" | python3 -c '
import json, os, re, sys
from datetime import datetime, timezone, timedelta

prod_ids = set(os.environ["PROD_IDS"].split())
staging_ids = set(os.environ["STAGING_IDS"].split())
all_ids = prod_ids | staging_ids
grace = timedelta(hours=int(os.environ["GRACE_HOURS"]))
now = datetime.now(timezone.utc)

# molecule/tenant/<org_id>/bootstrap — org_id is a UUID.
_TENANT_RE = re.compile(r"^molecule/tenant/([0-9a-fA-F-]{36})/bootstrap$")
# molecule/workspace/<ws_id>/config — ws_id is a UUID; owning org is on the tag.
_WS_RE = re.compile(r"^molecule/workspace/([0-9a-fA-F-]{36})/config$")

def parse_iso(s):
    if not s:
        return None
    # AWS returns ISO8601 with timezone (sometimes "+00:00", sometimes
    # numeric offset). datetime.fromisoformat handles both since 3.11.
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None

def org_tag(s):
    for t in s.get("Tags") or []:
        if t.get("Key") == "OrgID":
            return t.get("Value") or ""
    return ""

def decide(s, all_ids, grace, now):
    name = s.get("Name", "")
    arn = s.get("ARN", "")

    mt = _TENANT_RE.match(name)
    mw = _WS_RE.match(name)
    if not mt and not mw:
        return ("keep", "not-a-managed-secret", arn, name)

    # Grace gate (both shapes): never touch a secret younger than the window.
    created = parse_iso(s.get("CreatedDate") or s.get("LastChangedDate"))
    if created is not None and (now - created) < grace:
        return ("keep", "in-grace-window", arn, name)

    if mt:
        org_id = mt.group(1)
        if org_id in all_ids:
            return ("keep", "live-tenant", arn, name)
        return ("delete", "orphan-tenant", arn, name)

    # workspace-config: owning org is on the OrgID tag.
    org_id = org_tag(s)
    if not org_id:
        # No OrgID tag (legacy / hand-created) — cannot establish ownership;
        # keep and let the CP reaper (which parses the live set) handle it.
        return ("keep", "workspace-no-org-tag", arn, name)
    if org_id in all_ids:
        # Org still live — defer to the CP auto-reap secrets reaper for
        # per-workspace liveness; do not race a live tenant here.
        return ("keep", "workspace-live-org", arn, name)
    return ("delete", "orphan-workspace", arn, name)

d = json.loads(sys.stdin.read())
for s in d.get("SecretList", []):
    action, reason, arn, name = decide(s, all_ids, grace, now)
    print(json.dumps({"action": action, "reason": reason, "arn": arn, "name": name}))
')

# --- Summarize + safety gate ----------------------------------------------

DELETE_COUNT=$(printf '%s' "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['action']=='delete'))")
KEEP_COUNT=$((TOTAL_SECRETS - DELETE_COUNT))
MANAGED_SECRETS=$(printf '%s' "$DECISIONS" | python3 -c "
import json, sys
n = sum(1 for l in sys.stdin if json.loads(l)['reason'] != 'not-a-managed-secret')
print(n)
")

log ""
log "== Sweep plan =="
log "  total secrets:          $TOTAL_SECRETS"
log "  managed-shaped secrets: $MANAGED_SECRETS"
log "  would delete:           $DELETE_COUNT"
log "  would keep:             $KEEP_COUNT"
log ""

# Per-reason breakdown of deletes + keep-categories worth seeing
printf '%s' "$DECISIONS" | python3 -c "
import json,sys,collections
delete_c = collections.Counter()
keep_c = collections.Counter()
for l in sys.stdin:
    d = json.loads(l)
    if d['action'] == 'delete':
        delete_c[d['reason']] += 1
    else:
        keep_c[d['reason']] += 1
for reason, n in delete_c.most_common():
    print(f'  delete/{reason}: {n}')
for reason, n in keep_c.most_common():
    print(f'  keep/{reason}: {n}')
"

# Safety gate operates against the managed-shaped subset — same
# rationale as sweep-cf-tunnels: a miscount of platform-infra
# secrets shouldn't relax the gate.
#
# IMPORTANT (the historical no-op trap): the REAL safety here is the
# per-secret live-ORG cross-reference above — a secret is only marked
# delete when its owning org provably no longer exists in the CP DB. The
# percent gate is a blunt second line that, on a large genuine backlog
# (e.g. the June-2026 2.4k-orphan workspace-config sprawl, ~99% orphan),
# would itself BLOCK the very cleanup it exists to allow. So:
#   - Normal steady state: <50% delete → gate passes, runs every hour.
#   - Genuine bulk backlog: set SWEEP_ALLOW_BULK=1 to bypass the percent
#     gate DELIBERATELY, trusting the live-org cross-reference. This is the
#     sanctioned way to drain a backlog — NOT a blind MAX_DELETE_PCT bump.
if [ "$MANAGED_SECRETS" -gt 0 ]; then
  PCT=$(( DELETE_COUNT * 100 / MANAGED_SECRETS ))
  if [ "$PCT" -gt "$MAX_DELETE_PCT" ]; then
    if [ "${SWEEP_ALLOW_BULK:-0}" = "1" ]; then
      log ""
      log "SAFETY: would delete $PCT% of managed-shaped secrets (>$MAX_DELETE_PCT%) — BULK override active (SWEEP_ALLOW_BULK=1)."
      log "  Proceeding: every delete is live-org-cross-referenced AND a 30-day RECOVERABLE delete (restorable)."
    else
      log ""
      log "SAFETY: would delete $PCT% of managed-shaped secrets (threshold $MAX_DELETE_PCT%) — refusing."
      log "  This is the expected gate on a genuine backlog. The deletes are"
      log "  live-org-cross-referenced and recoverable (30d). To drain a backlog"
      log "  deliberately, rerun with SWEEP_ALLOW_BULK=1 $0 $*"
      exit 2
    fi
  fi
fi

if [ "$DRY_RUN" = "1" ]; then
  log ""
  log "Dry run complete. Pass --execute to actually delete $DELETE_COUNT secrets."
  log ""
  log "First 20 secrets that would be deleted:"
  printf '%s' "$DECISIONS" | python3 -c "
import json, sys
shown = 0
for l in sys.stdin:
    d = json.loads(l)
    if d['action'] == 'delete':
        print(f\"  {d['reason']:25s}  {d['name']}\")
        shown += 1
        if shown >= 20: break
"
  exit 0
fi

# --- Execute deletes -------------------------------------------------------
#
# Parallel delete loop following sweep-cf-tunnels.sh's pattern.
# AWS Secrets Manager DeleteSecret is fast (~0.3s/call), so even a
# serial loop would handle 100s of secrets within the workflow's
# 30 min cap, but parallel-by-default keeps us symmetric with the
# other sweepers and gives headroom for a one-off backlog.
#
# Deletion is RECOVERABLE (30-day recovery window), NOT force-delete.
# Changed from --force-delete-without-recovery: a mistaken sweep must be
# reversible via `aws secretsmanager restore-secret` for 30 days. The
# GRACE_HOURS filter + live-org cross-reference make a mistake unlikely,
# but at this scale (thousands of secrets) an unrecoverable bulk delete is
# an unacceptable blast radius. The secret is also regenerated on every
# fresh provision, so the recovery window is belt-and-suspenders, not the
# only safety. Matches the CP provisioner + auto-reap reaper, which both
# use DeleteSecret + RecoveryWindowInDays=30.

CONCURRENCY="${SWEEP_CONCURRENCY:-8}"
DELETE_PLAN=$(mktemp -t aws-secrets-plan-XXXXXX)
NAME_MAP=$(mktemp -t aws-secrets-names-XXXXXX)
FAIL_LOG=$(mktemp -t aws-secrets-fail-XXXXXX)
RESULT_LOG=$(mktemp -t aws-secrets-result-XXXXXX)

# Build delete plan (one ARN per line) and id→name side-channel for
# failure-log readability. Use ARN rather than Name on the delete
# call because Name is mutable; ARN is the stable identifier.
printf '%s' "$DECISIONS" | python3 -c '
import json, sys
plan_path = sys.argv[1]
map_path = sys.argv[2]
with open(plan_path, "w") as plan, open(map_path, "w") as nmap:
    for line in sys.stdin:
        d = json.loads(line)
        if d.get("action") != "delete":
            continue
        arn = d["arn"]
        name = d.get("name", "")
        plan.write(arn + "\n")
        nmap.write(arn + "\t" + name + "\n")
' "$DELETE_PLAN" "$NAME_MAP"

log ""
log "Executing $DELETE_COUNT deletions ($CONCURRENCY-way parallel)..."

export AWS_REGION NAME_MAP FAIL_LOG

# shellcheck disable=SC2016
xargs -P "$CONCURRENCY" -L 1 -I {} bash -c '
  arn="$1"
  if aws secretsmanager delete-secret \
       --region "$AWS_REGION" \
       --secret-id "$arn" \
       --recovery-window-in-days 30 \
       --output json >/dev/null 2>&1; then
    echo OK
  else
    name=$(awk -F"\t" -v a="$arn" "\$1==a {print \$2; exit}" "$NAME_MAP")
    echo FAIL
    echo "FAIL $name $arn" >> "$FAIL_LOG"
  fi
' _ {} < "$DELETE_PLAN" > "$RESULT_LOG"

DELETED=$(grep -c '^OK$' "$RESULT_LOG" || true)
FAILED=$(grep -c '^FAIL$' "$RESULT_LOG" || true)

log ""
log "Done. deleted=$DELETED failed=$FAILED"
if [ "$FAILED" -ne 0 ]; then
  log "Failure detail (first 20):"
  head -20 "$FAIL_LOG" | while IFS= read -r fl; do log "  $fl"; done
fi
[ "$FAILED" -eq 0 ]
