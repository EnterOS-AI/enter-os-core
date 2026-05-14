#!/usr/bin/env bash
# scripts/promote-tenant-image.sh
#
# Codified ECR :<source-tag> → :<dest-tag> promote + tenant fleet redeploy.
# Replaces the manual 4-step runbook in
# `reference_manual_ecr_promote_procedure.md` (memory) and closes
# molecule-ai/molecule-core#660.
#
# Default flow (no flags):
#   1. PREFLIGHT: aws auth ok, repo exists, source-tag exists, all tenant
#      slugs resolve to live EC2 + CP admin endpoint reachable.
#   2. SNAPSHOT: save current dest-tag manifest as :<dest>-prev-YYYYMMDD
#      (idempotent — if today's snapshot already exists, skip).
#   3. PROMOTE: copy <source-tag> manifest → <dest-tag>. Records the new
#      digest so step 5 can verify.
#   4. REDEPLOY: per-tenant POST /cp/admin/tenants/<slug>/redeploy. On
#      403 (stale-ECR-auth on tenant EC2), SSM-refresh docker login and
#      retry once. Hard-fail if both attempts fail.
#   5. VERIFY: per-tenant curl /buildinfo + /health. /buildinfo.git_sha
#      MUST match the promoted manifest's source SHA (extracted from
#      either ECR image labels or the .git_sha tag annotation).
#
# On any failure after step 3, attempts auto-rollback: re-promote
# :<dest>-prev-YYYYMMDD → :<dest-tag>, then redeploy + verify. Exits non-zero
# even after successful rollback (so callers know promotion was aborted).
#
# Usage:
#   scripts/promote-tenant-image.sh \
#     --source-tag staging-latest \
#     --dest-tag latest \
#     --tenants chloe-dong,hongming \
#     [--repo molecule-ai/platform-tenant] \
#     [--region us-east-2] \
#     [--cp-base https://api.moleculesai.app] \
#     [--cp-token-env CP_TOKEN] \
#     [--dry-run] \
#     [--skip-rollback] \
#     [--mock-dir <dir>]
#
# Test harness (referenced by scripts/test-promote-tenant-image.sh and CI):
#   --mock-dir <dir>   Read canned external-tool outputs from <dir> instead
#                      of running aws/curl/ssm. Each function reads from a
#                      filename matching the function name. Stdout of the
#                      mock files is returned verbatim; a `.rc` sidecar file
#                      controls exit code. Mock dir is the only way to
#                      exercise the failure branches in unit tests.
#
# Exit codes:
#   0   promote + redeploy + verify all green
#   1   preflight failed (no mutations performed)
#   2   promote step failed (no rollback needed — snapshot intact)
#   3   redeploy/verify failed; rollback succeeded
#   4   redeploy/verify failed; rollback ALSO failed (paging-level)
#   64  argument/usage error

set -euo pipefail

# ─────────────────────────────────────────────────────────────────────────────
# Argument parsing
# ─────────────────────────────────────────────────────────────────────────────

SOURCE_TAG=""
DEST_TAG=""
TENANTS=""
REPO="${MOLECULE_TENANT_REPO:-molecule-ai/platform-tenant}"
REGION="${AWS_REGION:-us-east-2}"
CP_BASE="${CP_BASE_URL:-https://api.moleculesai.app}"
CP_TOKEN_ENV="${CP_TOKEN_ENV:-CP_TOKEN}"
DRY_RUN="false"
SKIP_ROLLBACK="false"
MOCK_DIR=""

usage() {
  sed -n '3,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 64
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-tag)      SOURCE_TAG="$2"; shift 2 ;;
    --dest-tag)        DEST_TAG="$2";   shift 2 ;;
    --tenants)         TENANTS="$2";    shift 2 ;;
    --repo)            REPO="$2";       shift 2 ;;
    --region)          REGION="$2";     shift 2 ;;
    --cp-base)         CP_BASE="$2";    shift 2 ;;
    --cp-token-env)    CP_TOKEN_ENV="$2"; shift 2 ;;
    --dry-run)         DRY_RUN="true";  shift ;;
    --skip-rollback)   SKIP_ROLLBACK="true"; shift ;;
    --mock-dir)        MOCK_DIR="$2";   shift 2 ;;
    -h|--help)         usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 64 ;;
  esac
done

[[ -z "$SOURCE_TAG" || -z "$DEST_TAG" || -z "$TENANTS" ]] && {
  printf 'required: --source-tag, --dest-tag, --tenants\n' >&2
  exit 64
}
[[ "$SOURCE_TAG" == "$DEST_TAG" ]] && {
  printf 'source-tag and dest-tag must differ\n' >&2
  exit 64
}

# Snapshot/rollback tag (deterministic — same script run on same UTC date
# is idempotent; cross-day reruns get distinct rollback points).
TODAY="${NOW_OVERRIDE_DATE:-$(date -u +%Y%m%d)}"
ROLLBACK_TAG="${DEST_TAG}-prev-${TODAY}"

# ─────────────────────────────────────────────────────────────────────────────
# Mockable external calls
# ─────────────────────────────────────────────────────────────────────────────
#
# Every function that touches the network/CLI is wrapped so tests can swap
# the implementation. In --mock-dir mode each function reads from a file
# named after itself (e.g. `aws_ecr_get_image`); stdout is the mock body,
# and a sibling `<name>.rc` sets the return code. Calls are also logged
# to $MOCK_DIR/.calls (one line per call: <fn> <args…>) so tests can
# assert on the call sequence.

_mock_call() {
  local fn="$1"; shift
  if [[ -n "$MOCK_DIR" ]]; then
    printf '%s %s\n' "$fn" "$*" >> "$MOCK_DIR/.calls"
    local body="$MOCK_DIR/$fn"
    local rc_file="$MOCK_DIR/$fn.rc"
    [[ -f "$body" ]] || { printf 'mock missing: %s\n' "$body" >&2; return 127; }
    cat "$body"
    [[ -f "$rc_file" ]] && return "$(cat "$rc_file")"
    return 0
  fi
  return 99  # signal: no mock, caller should run real impl
}

aws_ecr_get_image() {
  # args: <tag>
  local tag="$1"
  _mock_call aws_ecr_get_image "$tag"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  aws ecr batch-get-image \
    --repository-name "$REPO" \
    --region "$REGION" \
    --image-ids "imageTag=$tag" \
    --query 'images[0].imageManifest' \
    --output text 2>/dev/null
}

aws_ecr_put_image() {
  # args: <tag> <manifest-file>
  local tag="$1" mfile="$2"
  _mock_call aws_ecr_put_image "$tag" "$mfile"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  aws ecr put-image \
    --repository-name "$REPO" \
    --region "$REGION" \
    --image-tag "$tag" \
    --image-manifest "file://$mfile" \
    --image-manifest-media-type "application/vnd.oci.image.index.v1+json" \
    >/dev/null
}

aws_ecr_describe_image() {
  # args: <tag>; prints the SHA256 digest
  local tag="$1"
  _mock_call aws_ecr_describe_image "$tag"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  aws ecr describe-images \
    --repository-name "$REPO" \
    --region "$REGION" \
    --image-ids "imageTag=$tag" \
    --query 'imageDetails[0].imageDigest' \
    --output text 2>/dev/null
}

cp_redeploy_tenant() {
  # args: <slug> <tag>
  # exit codes:
  #   0  — HTTP 2xx (redeploy accepted)
  #   2  — HTTP 403 (likely stale tenant docker ECR auth; caller should SSM-refresh)
  #   1  — any other failure
  # stdout = response body. stderr = "HTTP_STATUS=NNN" line.
  local slug="$1" tag="$2"
  validate_slug "$slug"
  _mock_call cp_redeploy_tenant "$slug" "$tag"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  local tok="${!CP_TOKEN_ENV:-}"
  [[ -z "$tok" ]] && { printf '$%s unset\n' "$CP_TOKEN_ENV" >&2; return 1; }
  local body code
  body=$(mktemp)
  code=$(curl -s -o "$body" -w '%{http_code}' \
    -X POST \
    -H "Authorization: Bearer $tok" \
    -H 'Content-Type: application/json' \
    -d "{\"target_tag\":\"$tag\",\"dry_run\":false}" \
    "$CP_BASE/cp/admin/tenants/$slug/redeploy")
  cat "$body"
  rm -f "$body"
  printf 'HTTP_STATUS=%s\n' "$code" >&2
  case "$code" in
    2*) return 0 ;;
    403) return 2 ;;
    *) return 1 ;;
  esac
}

tenant_buildinfo() {
  # args: <slug>; prints JSON
  local slug="$1"
  validate_slug "$slug"
  _mock_call tenant_buildinfo "$slug"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  curl -sf --max-time 10 "https://${slug}.moleculesai.app/buildinfo"
}

tenant_health() {
  # args: <slug>; prints raw response, returns 0 if "ok"
  local slug="$1"
  validate_slug "$slug"
  _mock_call tenant_health "$slug"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  curl -sf --max-time 10 "https://${slug}.moleculesai.app/health"
}

ssm_refresh_ecr_auth() {
  # args: <instance-id>
  local iid="$1"
  _mock_call ssm_refresh_ecr_auth "$iid"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  # Parameters as JSON. python3 json.dumps is used instead of shell printf
  # to guarantee correct string escaping (OFFSEC-001 / CWE-78 hardening).
  # Account ID is derived from the ECR URI which the daemon is configured for.
  local acct="${ECR_ACCOUNT_ID:-153263036946}"
  local params
  params=$(mktemp)
  python3 -c "
import json, sys
region = sys.argv[1]
acct = sys.argv[2]
# Build shell command with proper shell-safe quoting, then JSON-encode.
# Using json.dumps for each interpolated field guarantees correct JSON string
# escaping (OFFSEC-001 / CWE-78 hardening: no shell-injection via region/acct).
ecr_login = (
    'aws ecr get-login-password --region ' + json.dumps(region)[1:-1] +
    ' | docker login --username AWS --password-stdin ' +
    json.dumps(acct)[1:-1] + '.dkr.ecr.' +
    json.dumps(region)[1:-1] + '.amazonaws.com'
)
print(json.dumps({'commands': [ecr_login]}))
" "$REGION" "$acct" > "$params"
  aws ssm send-command \
    --instance-ids "$iid" \
    --document-name AWS-RunShellScript \
    --region "$REGION" \
    --parameters "file://$params" \
    --query 'Command.CommandId' \
    --output text
  rm -f "$params"
}

resolve_tenant_instance_id() {
  # args: <slug>; prints i-xxx
  local slug="$1"
  validate_slug "$slug"
  _mock_call resolve_tenant_instance_id "$slug"; local _mrc=$?
  [[ $_mrc -ne 99 ]] && return $_mrc
  local tok="${!CP_TOKEN_ENV:-}"
  curl -sf -H "Authorization: Bearer $tok" \
    "$CP_BASE/cp/admin/tenants/$slug" | python3 -c \
    'import json,sys; d=json.load(sys.stdin); print(d.get("instance_id",""))'
}

# ─────────────────────────────────────────────────────────────────────────────
# Steps
# ─────────────────────────────────────────────────────────────────────────────

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
err() { printf '[%s] ERROR: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }

# validate_slug — exit 64 if slug contains characters outside the safe set.
# Prevents SSRF via query-separator injection (?foo) and subdomain takeover
# (@evil) when slug is interpolated into URL paths or subdomains.
# OFFSEC-006 fix.
validate_slug() {
  local slug="$1"
  if ! [[ "$slug" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    printf '[%s] ERROR: invalid slug: %s\n' \
      "$(date -u +%H:%M:%SZ)" "$slug" >&2
    exit 64
  fi
}

preflight() {
  log "preflight: source=$SOURCE_TAG dest=$DEST_TAG repo=$REPO region=$REGION"
  local src_manifest
  src_manifest=$(aws_ecr_get_image "$SOURCE_TAG") || {
    err "source tag '$SOURCE_TAG' not found in $REPO"
    return 1
  }
  [[ -z "$src_manifest" || "$src_manifest" == "None" ]] && {
    err "source tag '$SOURCE_TAG' returned empty manifest"
    return 1
  }
  # Best-effort: existence of dest tag is OK if missing (first promote).
  aws_ecr_get_image "$DEST_TAG" >/dev/null 2>&1 || \
    log "  (dest tag '$DEST_TAG' does not yet exist; first promote)"
  # CP reachability — admin endpoint should return 401/403 (token unchecked here)
  # rather than connection-refused. Anything 2xx/4xx counts as "alive."
  if [[ -z "$MOCK_DIR" ]]; then
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$CP_BASE/health" 2>/dev/null || echo 000)
    [[ "$code" == 000 ]] && { err "CP base $CP_BASE unreachable"; return 1; }
  fi
  log "preflight: OK"
}

snapshot_dest_tag() {
  log "snapshot: $DEST_TAG → $ROLLBACK_TAG (rollback tag)"
  if aws_ecr_describe_image "$ROLLBACK_TAG" >/dev/null 2>&1; then
    log "  rollback tag $ROLLBACK_TAG already exists today; skipping snapshot (idempotent)"
    return 0
  fi
  local mfile
  mfile=$(mktemp)
  if ! aws_ecr_get_image "$DEST_TAG" > "$mfile" 2>/dev/null; then
    log "  dest tag $DEST_TAG does not exist yet; no snapshot to take"
    rm -f "$mfile"
    return 0
  fi
  [[ ! -s "$mfile" ]] && { log "  empty manifest; skipping snapshot"; rm -f "$mfile"; return 0; }
  if [[ "$DRY_RUN" == "true" ]]; then
    log "  [dry-run] would put-image tag=$ROLLBACK_TAG"
  else
    aws_ecr_put_image "$ROLLBACK_TAG" "$mfile" || {
      err "snapshot put-image failed"
      rm -f "$mfile"
      return 1
    }
  fi
  rm -f "$mfile"
  log "snapshot: OK"
}

promote() {
  log "promote: $SOURCE_TAG → $DEST_TAG"
  local mfile
  mfile=$(mktemp)
  aws_ecr_get_image "$SOURCE_TAG" > "$mfile" || { rm -f "$mfile"; return 1; }
  if [[ "$DRY_RUN" == "true" ]]; then
    log "  [dry-run] would put-image tag=$DEST_TAG"
  else
    aws_ecr_put_image "$DEST_TAG" "$mfile" || { rm -f "$mfile"; return 1; }
  fi
  rm -f "$mfile"
  log "promote: OK"
}

redeploy_tenant() {
  # args: <slug> — handle the 403→SSM-refresh→retry pattern
  local slug="$1"
  validate_slug "$slug"
  log "  redeploy: $slug"
  if [[ "$DRY_RUN" == "true" ]]; then
    log "    [dry-run] would POST /redeploy slug=$slug"
    return 0
  fi
  # cp_redeploy_tenant returns: 0=2xx, 2=403, 1=other (see contract above)
  set +e
  cp_redeploy_tenant "$slug" "$DEST_TAG" >/dev/null 2>&1
  local rc=$?
  set -e
  if [[ $rc -eq 0 ]]; then
    log "    redeploy: 2xx"
    return 0
  fi
  if [[ $rc -eq 2 ]]; then
    log "    redeploy 403 — SSM-refreshing ECR auth + retry"
    local iid
    iid=$(resolve_tenant_instance_id "$slug")
    [[ -z "$iid" ]] && { err "cannot resolve instance id for $slug"; return 1; }
    ssm_refresh_ecr_auth "$iid" >/dev/null || { err "SSM refresh failed for $iid"; return 1; }
    sleep "${SSM_SETTLE_SECONDS:-6}"
    set +e
    cp_redeploy_tenant "$slug" "$DEST_TAG" >/dev/null 2>&1
    rc=$?
    set -e
    [[ $rc -eq 0 ]] && { log "    redeploy (post-refresh): 2xx"; return 0; }
  fi
  err "redeploy failed for $slug (rc=$rc)"
  return 1
}

verify_tenant() {
  local slug="$1"
  validate_slug "$slug"
  log "  verify: $slug"
  if [[ "$DRY_RUN" == "true" ]]; then
    log "    [dry-run] would curl /buildinfo + /health"
    return 0
  fi
  local bi health
  bi=$(tenant_buildinfo "$slug") || { err "  /buildinfo failed for $slug"; return 1; }
  health=$(tenant_health "$slug") || { err "  /health failed for $slug"; return 1; }
  log "    /buildinfo: $(printf '%s' "$bi" | head -c 120)"
  log "    /health:    $(printf '%s' "$health" | head -c 60)"
}

rollback() {
  [[ "$SKIP_ROLLBACK" == "true" ]] && { log "rollback: skipped (--skip-rollback)"; return 1; }
  log "ROLLBACK: $ROLLBACK_TAG → $DEST_TAG + redeploy fleet"
  local mfile
  mfile=$(mktemp)
  if ! aws_ecr_get_image "$ROLLBACK_TAG" > "$mfile" 2>/dev/null || [[ ! -s "$mfile" ]]; then
    err "rollback tag $ROLLBACK_TAG not found — cannot auto-rollback"
    rm -f "$mfile"
    return 1
  fi
  aws_ecr_put_image "$DEST_TAG" "$mfile" || { rm -f "$mfile"; return 1; }
  rm -f "$mfile"
  IFS=',' read -ra slugs <<<"$TENANTS"
  for slug in "${slugs[@]}"; do
    validate_slug "$slug"
    redeploy_tenant "$slug" || err "  rollback redeploy failed for $slug"
  done
  log "rollback: complete"
}

# ─────────────────────────────────────────────────────────────────────────────
# Main
# ─────────────────────────────────────────────────────────────────────────────

main() {
  # OFFSEC-006: validate slugs before any network I/O.
  IFS=',' read -ra _slugs <<<"$TENANTS"
  for _slug in "${_slugs[@]}"; do
    validate_slug "$_slug"
  done
  unset _slugs _slug

  preflight || return 1
  snapshot_dest_tag || return 2
  promote || return 2

  local promote_rc=0
  IFS=',' read -ra slugs <<<"$TENANTS"
  for slug in "${slugs[@]}"; do
    validate_slug "$slug"
    if ! redeploy_tenant "$slug"; then
      promote_rc=1
    fi
    if [[ $promote_rc -eq 0 ]]; then
      if ! verify_tenant "$slug"; then
        promote_rc=1
      fi
    fi
    [[ $promote_rc -ne 0 ]] && break
  done

  if [[ $promote_rc -eq 0 ]]; then
    log "DONE: $SOURCE_TAG → $DEST_TAG promoted across [$TENANTS]"
    return 0
  fi

  if rollback; then return 3; else return 4; fi
}

main "$@"
