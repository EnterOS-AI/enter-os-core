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
# This is a parallel-shape janitor to sweep-cf-tunnels.sh:
#   1. Query CP admin API to enumerate live org IDs (prod + staging)
#   2. Enumerate AWS Secrets Manager secrets matching the tenant prefix
#   3. For each secret matching `molecule/tenant/<org_id>/bootstrap`,
#      check if <org_id> appears in the live set
#   4. Defense-in-depth: skip secrets created in the last 24h
#      (window for a provision-in-progress that hasn't yet finished
#      its first heartbeat to CP)
#   5. Only delete secrets with NO live org counterpart AND outside
#      the 24h grace window
#
# Dry-run by default; must pass --execute to actually delete.
#
# Note on deletion semantics: --force-delete-without-recovery skips
# the 7-30 day recovery window. We accept this because (a) the grace
# window above already filters in-flight provisions, and (b) the
# bootstrap secret is regenerated on every reprovision — losing one
# is recoverable by re-running the provision flow.
#
# Env vars required:
#   AWS_REGION              — region the secrets live in (default: us-east-1)
#   CP_PROD_ADMIN_TOKEN     — CP admin bearer for api.moleculesai.app
#   CP_STAGING_ADMIN_TOKEN  — CP admin bearer for staging-api.moleculesai.app
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
#         tenant-shaped secrets; refusing)

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
need CP_PROD_ADMIN_TOKEN
need CP_STAGING_ADMIN_TOKEN
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

log "Fetching CP prod org ids..."
PROD_IDS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_PROD_ADMIN_TOKEN" \
  "https://api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['id'] for o in json.load(sys.stdin).get('orgs',[])))")
log "  prod orgs: $(echo "$PROD_IDS" | wc -w | tr -d ' ')"

log "Fetching CP staging org ids..."
STAGING_IDS=$(curl -sS -m 15 -H "Authorization: Bearer $CP_STAGING_ADMIN_TOKEN" \
  "https://staging-api.moleculesai.app/cp/admin/orgs?limit=500" \
  | python3 -c "import json,sys; print(' '.join(o['id'] for o in json.load(sys.stdin).get('orgs',[])))")
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
  if [ -z "$NEXT_TOKEN" ]; then
    aws secretsmanager list-secrets \
      --region "$AWS_REGION" \
      --filters Key=name,Values=molecule/tenant/ \
      --max-results 100 \
      --output json > "$page_file"
  else
    aws secretsmanager list-secrets \
      --region "$AWS_REGION" \
      --filters Key=name,Values=molecule/tenant/ \
      --max-results 100 \
      --next-token "$NEXT_TOKEN" \
      --output json > "$page_file"
  fi
  NEXT_TOKEN=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('NextToken') or '')" "$page_file")
  PAGE=$((PAGE + 1))
  if [ -z "$NEXT_TOKEN" ]; then break; fi
  if [ "$PAGE" -gt 50 ]; then
    log "::warning::stopping pagination at page 50 (5000 secrets) — re-run if more"
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
# Rules (in order):
#   1. Name doesn't match `molecule/tenant/<org_id>/bootstrap` → keep
#      (unknown — never sweep arbitrary secrets that might belong to
#      platform infra or other tenants of this AWS account).
#   2. CreatedDate within $GRACE_HOURS → keep (defense-in-depth: don't
#      kill a secret while its provision is still mid-flight).
#   3. org_id ∈ {prod_ids ∪ staging_ids} → keep (live tenant).
#   4. Otherwise → delete (orphan).

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

def parse_iso(s):
    if not s:
        return None
    # AWS returns ISO8601 with timezone (sometimes "+00:00", sometimes
    # numeric offset). datetime.fromisoformat handles both since 3.11.
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None

def decide(s, all_ids, grace, now):
    name = s.get("Name", "")
    arn = s.get("ARN", "")

    m = _TENANT_RE.match(name)
    if not m:
        return ("keep", "not-a-tenant-secret", arn, name)

    org_id = m.group(1)

    created = parse_iso(s.get("CreatedDate") or s.get("LastChangedDate"))
    if created is not None and (now - created) < grace:
        return ("keep", "in-grace-window", arn, name)

    if org_id in all_ids:
        return ("keep", "live-tenant", arn, name)

    return ("delete", "orphan-tenant", arn, name)

d = json.loads(sys.stdin.read())
for s in d.get("SecretList", []):
    action, reason, arn, name = decide(s, all_ids, grace, now)
    print(json.dumps({"action": action, "reason": reason, "arn": arn, "name": name}))
')

# --- Summarize + safety gate ----------------------------------------------

DELETE_COUNT=$(echo "$DECISIONS" | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)['action']=='delete'))")
KEEP_COUNT=$((TOTAL_SECRETS - DELETE_COUNT))
TENANT_SECRETS=$(echo "$DECISIONS" | python3 -c "
import json, sys
n = sum(1 for l in sys.stdin if json.loads(l)['reason'] != 'not-a-tenant-secret')
print(n)
")

log ""
log "== Sweep plan =="
log "  total secrets:          $TOTAL_SECRETS"
log "  tenant-shaped secrets:  $TENANT_SECRETS"
log "  would delete:           $DELETE_COUNT"
log "  would keep:             $KEEP_COUNT"
log ""

# Per-reason breakdown of deletes + keep-categories worth seeing
echo "$DECISIONS" | python3 -c "
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

# Safety gate operates against the tenant-shaped subset — same
# rationale as sweep-cf-tunnels: a miscount of platform-infra
# secrets shouldn't relax the gate.
if [ "$TENANT_SECRETS" -gt 0 ]; then
  PCT=$(( DELETE_COUNT * 100 / TENANT_SECRETS ))
  if [ "$PCT" -gt "$MAX_DELETE_PCT" ]; then
    log ""
    log "SAFETY: would delete $PCT% of tenant-shaped secrets (threshold $MAX_DELETE_PCT%) — refusing."
    log "  If this is expected (e.g. major cleanup after incident), rerun with"
    log "  MAX_DELETE_PCT=$((PCT+5)) $0 $*"
    exit 2
  fi
fi

if [ "$DRY_RUN" = "1" ]; then
  log ""
  log "Dry run complete. Pass --execute to actually delete $DELETE_COUNT secrets."
  log ""
  log "First 20 secrets that would be deleted:"
  echo "$DECISIONS" | python3 -c "
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
# --force-delete-without-recovery skips the 7-30 day recovery window.
# Acceptable here because (a) the GRACE_HOURS filter prevents touching
# in-flight provisions, and (b) the secret is regenerated on every
# fresh provision — losing one only matters for a tenant we're
# explicitly trying to forget.

CONCURRENCY="${SWEEP_CONCURRENCY:-8}"
DELETE_PLAN=$(mktemp -t aws-secrets-plan-XXXXXX)
NAME_MAP=$(mktemp -t aws-secrets-names-XXXXXX)
FAIL_LOG=$(mktemp -t aws-secrets-fail-XXXXXX)
RESULT_LOG=$(mktemp -t aws-secrets-result-XXXXXX)

# Build delete plan (one ARN per line) and id→name side-channel for
# failure-log readability. Use ARN rather than Name on the delete
# call because Name is mutable; ARN is the stable identifier.
echo "$DECISIONS" | python3 -c '
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
       --force-delete-without-recovery \
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
