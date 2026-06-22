#!/usr/bin/env bash
# Template DELIVERY e2e (RFC #2843) — the regression gate for the
# 2026-06-15 incident family (#2919 + #2843 rollout). It provisions a FRESH
# tenant + a FRESH seo-agent workspace and asserts the TWO delivery channels
# each deliver what they own, end to end.
#
# TWO CHANNELS (RFC #2843 #32, post-decoupling):
#   1. TEMPLATE-ASSET channel (provisioning-time): config.yaml + prompts +
#      model. Delivered during provisioning, present the moment the box is
#      online.
#   2. DYNAMIC PLUGIN channel (post-online): agent-skills are PLUGINS. They are
#      NO LONGER delivered via the template-asset / provisioning channel. The
#      seo-agent DECLARES the seo-all plugin (source
#      gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main),
#      and the post-online reconcile (registry heartbeat transition-to-online →
#      ReconcileWorkspacePlugins) installs it through the existing plugin
#      install pipeline. It lands at /configs/plugins/seo-all/ (SKILL.md
#      present) after the online→reconcile→install→restart cycle.
#
# WHY THIS EXISTS — the bugs that shipped green because nothing asserted this:
#   1. concierge booted with no MODEL  (MISSING_MODEL fail-closed) — #2966
#   2. concierge booted with no IDENTITY (generic Claude Code)     — #2955
#   3. seo-agent booted but the seo-all skill never installed       — #32
# The unit/drift tests did not catch any of these because none provision a
# fresh agent end-to-end and inspect the DELIVERED /configs. This does.
#
# Assertions (each maps to a real incident):
#   A. seo-agent reaches online                       (catches MISSING_MODEL)
#   B. GET /model == the template's declared model    (catches model drop)
#   C. config.yaml delivered + REAL (> 1 KiB) via the ASSET channel
#   D. prompts/ delivered (identity prompt) via the ASSET channel
#   E. plugins/seo-all/SKILL.md installed via the post-online PLUGIN reconcile
#      (catches skill drop, #32) — NOT the asset channel. Polled, because the
#      install fires AFTER online and triggers a container restart.
#
# Auth model + org-provision/teardown shape mirror test_staging_concierge_e2e.sh.
#
# Required env:
#   MOLECULE_CP_URL        default https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer (Railway CP_ADMIN_API_TOKEN)
# Optional:
#   E2E_SEO_TEMPLATE       default seo-agent
#   E2E_EXPECTED_MODEL     default moonshot/kimi-k2.6
#   E2E_EXPECTED_PLUGIN    default seo-all  (install name = last subpath segment)
#   E2E_PROVISION_TIMEOUT_SECS  default 900
#   E2E_PLUGIN_INSTALL_TIMEOUT_SECS default 600 (online→reconcile→install→restart)
#   E2E_KEEP_ORG           1 → skip teardown (debug)
#
# Exit: 0 pass | 1 assertion fail | 2 missing env | 3 provision timeout
set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway CP_ADMIN_API_TOKEN}"
SEO_TEMPLATE="${E2E_SEO_TEMPLATE:-seo-agent}"
EXPECTED_MODEL="${E2E_EXPECTED_MODEL:-moonshot/kimi-k2.6}"
EXPECTED_PLUGIN="${E2E_EXPECTED_PLUGIN:-seo-all}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
PLUGIN_INSTALL_TIMEOUT_SECS="${E2E_PLUGIN_INSTALL_TIMEOUT_SECS:-600}"
# Settle budget for the ASSET-channel assertions (C config.yaml, D prompts).
# A freshly-online tenant's /configs inspection endpoint (execs into the
# container) can be transiently slow or time out the first read — observed:
# `curl: (28) ... 0 bytes` on a just-online box → config.yaml read as size 0 →
# false "stub" failure. We now poll the config endpoint DIRECTLY with bounded
# exponential backoff and STOP re-fetching once we see a real (>1 KiB) file,
# so a late timeout while we wait for prompts cannot reset the observed size
# and flake the assertion. We also distinguish "workspace still provisioning"
# from "genuine missing config" when the settle budget expires (core#3062).
ASSET_SETTLE_SECS="${E2E_ASSET_SETTLE_SECS:-180}"

# Collision-proof slug (random suffix), same convention as the sibling harness.
RAND=$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')
SLUG="e2e-tmpl-${RAND}"
CURL_COMMON=(-sS --max-time 30)

log()  { echo "[$(date +%H:%M:%S)] $*" >&2; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*" >&2; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }

admin_call() { local m="$1" p="$2"; shift 2; curl "${CURL_COMMON[@]}" -X "$m" "$CP_URL$p" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"; }

# ─── teardown trap (org delete) ──────────────────────────────────────────────
cleanup_org() {
  local rc=$?
  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — leaving $SLUG; delete manually."; return $rc
  fi
  log "teardown: DELETE tenant $SLUG"
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" >/dev/null 2>&1 || true
  return $rc
}
trap cleanup_org EXIT INT TERM

# ─── 0. preflight ────────────────────────────────────────────────────────────
log "═══ Template-asset delivery E2E ═══  CP=$CP_URL  slug=$SLUG  template=$SEO_TEMPLATE"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1. create org ───────────────────────────────────────────────────────────
log "1/6 creating org $SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -z "$ORG_ID" ] && fail "org create missing id: $CREATE"
ok "org created id=$ORG_ID"

# ─── 2. wait for tenant running ──────────────────────────────────────────────
log "2/6 waiting for tenant provisioning (≤${PROVISION_TIMEOUT_SECS}s)"
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS )); LAST=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && exit 3
  S=$(admin_call GET /cp/admin/orgs 2>/dev/null | python3 -c "
import json,sys
for o in json.load(sys.stdin).get('orgs',[]):
    if o.get('slug')=='$SLUG': print(o.get('instance_status','')); break
" 2>/dev/null || echo "")
  [ "$S" != "$LAST" ] && { log "    status → $S"; LAST="$S"; }
  case "$S" in running) break;; failed) fail "tenant provisioning failed";; *) sleep 15;; esac
done
ok "tenant running"

# tenant URL + token
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DOM="${CP_HOST#api.}" ;;
  staging-api.*) DOM="staging.${CP_HOST#staging-api.}" ;;
  *)             DOM="$CP_HOST" ;;
esac
TENANT_URL="https://$SLUG.${MOLECULE_TENANT_DOMAIN:-$DOM}"
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "no per-tenant admin token"
log "3/6 tenant token ok; waiting for TLS at $TENANT_URL"
TLS_DEADLINE=$(( $(date +%s) + 900 ))
until curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; do
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "tenant /health never 2xx in 15m"; sleep 5
done
ok "tenant reachable"

tenant_call() { local m="$1" p="$2"; shift 2; curl "${CURL_COMMON[@]}" -X "$m" "$TENANT_URL$p" \
  -H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" -H "Origin: $TENANT_URL" "$@"; }

# ─── 4. provision a fresh seo-agent workspace ────────────────────────────────
log "4/6 provisioning a fresh '$SEO_TEMPLATE' workspace"
WS=$(tenant_call POST /workspaces --max-time 90 \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"seo-delivery-e2e\",\"runtime\":\"claude-code\",\"template\":\"$SEO_TEMPLATE\"}")
WID=$(echo "$WS" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -z "$WID" ] && fail "workspace create missing id: $WS"
ok "seo-agent workspace id=$WID"

# poll online — ASSERTION A (catches MISSING_MODEL fail-closed)
log "5/6 waiting for seo-agent online (≤${PROVISION_TIMEOUT_SECS}s)"
WDEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS )); WLAST=""
while true; do
  [ "$(date +%s)" -gt "$WDEADLINE" ] && fail "seo-agent never reached online (Assertion A)"
  WS_S=$(tenant_call GET "/workspaces/$WID" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$WS_S" != "$WLAST" ] && { log "    ws status → $WS_S"; WLAST="$WS_S"; }
  case "$WS_S" in online|running) break;; failed) fail "seo-agent provision FAILED (Assertion A — likely MISSING_MODEL)";; *) sleep 15;; esac
done
ok "A: seo-agent online"

# ─── 6. assert DELIVERY (the load-bearing checks) ────────────────────────────
log "6/6 asserting template-asset delivery on /configs"

# B. model delivered
MODEL=$(tenant_call GET "/workspaces/$WID/model" | python3 -c "import json,sys;print(json.load(sys.stdin).get('model',''))" 2>/dev/null || echo "")
[ "$MODEL" = "$EXPECTED_MODEL" ] || fail "B: model=$MODEL, want $EXPECTED_MODEL"
ok "B: model=$MODEL"

# C. config.yaml delivered + REAL (not the 218 B default stub)
# D. prompts/ delivered (identity prompt)
#
# Deterministic fetch: the /configs inspection endpoint can transiently time out
# (curl 28) on a just-online box and fall back to a 218 B default stub, giving a
# false "config not delivered" failure. We poll the config endpoint DIRECTLY
# with bounded exponential backoff until it returns a real (>1 KiB) config OR
# the settle budget expires. Once config is confirmed real we STOP re-fetching
# it, so a late timeout while we wait for prompts cannot reset the observed
# size to 0 and flake assertion C. We also distinguish a workspace that is still
# provisioning from a genuine missing-config failure (core#3062).
ASSET_DEADLINE=$(( $(date +%s) + ASSET_SETTLE_SECS ))
CFG_SIZE=0; PROMPTS=0; WS_STATUS=""; CFG_ATTEMPTS=0; BACKOFF=2

workspace_status() {
  tenant_call GET "/workspaces/$WID" 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('status',''))
" 2>/dev/null || echo ""
}

config_size() {
  # Direct fetch of /configs/config.yaml. size_download tells us how many bytes
  # the endpoint actually returned. A curl error (e.g. 28) is reported as -1.
  local size
  size=$(tenant_call GET "/workspaces/$WID/files/config.yaml" -o /dev/null -w "%{size_download}" 2>/dev/null) || { echo -1; return; }
  echo "${size:-0}"
}

while true; do
  [ "$(date +%s)" -gt "$ASSET_DEADLINE" ] && break

  WS_STATUS=$(workspace_status)

  # Only keep polling config until we have seen a real file. Freezing the value
  # prevents a late curl timeout (while prompts are still settling) from wiping
  # out an earlier successful read and causing a false stub failure.
  if [ "${CFG_SIZE:-0}" -le 1024 ]; then
    CFG_ATTEMPTS=$((CFG_ATTEMPTS + 1))
    CFG_SIZE=$(config_size)
    if [ "${CFG_SIZE:-0}" -gt 1024 ]; then
      log "    config.yaml ready after $CFG_ATTEMPTS attempt(s) ($CFG_SIZE B)"
    elif [ "${CFG_SIZE:-0}" -lt 0 ]; then
      log "    config fetch transient error (attempt $CFG_ATTEMPTS) — still provisioning?"
      CFG_SIZE=0
    fi
  fi

  if [ "${PROMPTS:-0}" -le 0 ]; then
    PROMPTS=$(tenant_call GET "/workspaces/$WID/files?path=prompts" | python3 -c "
import json,sys
d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo 0)
  fi

  { [ "${CFG_SIZE:-0}" -gt 1024 ] && [ "${PROMPTS:-0}" -gt 0 ]; } && break

  # Bounded exponential backoff: 2,4,8,16,30,30,... up to the deadline.
  [ "$BACKOFF" -lt 30 ] && BACKOFF=$((BACKOFF * 2))
  [ "$BACKOFF" -gt 30 ] && BACKOFF=30
  sleep "$BACKOFF"
done

# Distinguish a genuine missing config from a workspace that was still
# provisioning when the settle budget expired.
if [ "${CFG_SIZE:-0}" -le 1024 ]; then
  case "$WS_STATUS" in
    online|running)
      fail "C: config.yaml size=$CFG_SIZE B after ${ASSET_SETTLE_SECS}s and workspace is $WS_STATUS (≤1KiB ⇒ default stub, template config NOT delivered — genuine missing config)"
      ;;
    *)
      fail "C: config.yaml size=$CFG_SIZE B after ${ASSET_SETTLE_SECS}s while workspace status='$WS_STATUS' — workspace still provisioning; config delivery NOT YET VERIFIABLE (not a genuine missing-config failure)"
      ;;
  esac
fi
ok "C: config.yaml delivered ($CFG_SIZE B)"
[ "${PROMPTS:-0}" -gt 0 ] || fail "D: prompts/ empty after ${ASSET_SETTLE_SECS}s — identity prompt NOT delivered"
ok "D: prompts/ delivered ($PROMPTS file(s))"

# E. plugins/seo-all/SKILL.md installed via the post-online PLUGIN reconcile —
#    THE #32 regression assertion, under the NEW (RFC #2843) contract.
#
# The seo-all skill is NO LONGER on /configs via the asset channel. The
# seo-agent template DECLARES it as a plugin; the registry heartbeat's
# transition-to-online fires ReconcileWorkspacePlugins, which installs the
# declared plugin through the standard pipeline. It lands at
# /configs/plugins/seo-all/ with SKILL.md (+ a .complete marker), and a
# first-time install triggers a container restart — so it is NOT present at
# the instant of online. We POLL the asset channel paths are already asserted
# (C/D above); here we wait for the plugin to land.
#
# Negative control: the skill MUST NOT arrive via the OLD asset path. If
# agent-skills/seo-all/SKILL.md shows up, the decoupling regressed (the
# provisioning channel is still smuggling skills) — fail loudly.
PLUGIN_PATH="plugins/$EXPECTED_PLUGIN"
log "E: polling for plugin '$EXPECTED_PLUGIN' at /configs/$PLUGIN_PATH (≤${PLUGIN_INSTALL_TIMEOUT_SECS}s; online→reconcile→install→restart)"

# Negative control — old asset-channel path must stay EMPTY.
OLD_ASSET=$(tenant_call GET "/workspaces/$WID/files?path=agent-skills/$EXPECTED_PLUGIN" | python3 -c "
import json,sys
d=json.load(sys.stdin)
paths=[f.get('path','') for f in d] if isinstance(d,list) else []
print('SKILL_MD' if any(p.endswith('SKILL.md') for p in paths) else 'EMPTY')
" 2>/dev/null || echo EMPTY)
[ "$OLD_ASSET" = "SKILL_MD" ] && fail "E(neg): agent-skills/$EXPECTED_PLUGIN/SKILL.md present on the OLD asset channel — RFC#2843 decoupling REGRESSED; the provisioning channel is still delivering skills (it must not)."
ok "E(neg): old asset-channel agent-skills/$EXPECTED_PLUGIN empty (decoupling holds)"

PDEADLINE=$(( $(date +%s) + PLUGIN_INSTALL_TIMEOUT_SECS )); PLAST=""
SKILLS=EMPTY
while true; do
  if [ "$(date +%s)" -gt "$PDEADLINE" ]; then
    fail "E: plugin '$EXPECTED_PLUGIN' never installed at /configs/$PLUGIN_PATH within ${PLUGIN_INSTALL_TIMEOUT_SECS}s — the post-online reconcile did NOT install the declared seo-all plugin (#32 regression under the new dynamic-plugin contract). last=$PLAST"
  fi
  SKILLS=$(tenant_call GET "/workspaces/$WID/files?path=$PLUGIN_PATH" | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: d=None
paths=[f.get('path','') for f in d] if isinstance(d,list) else []
# SKILL.md = the skill landed; .complete = atomic install finished; else partial/empty.
if any(p.endswith('SKILL.md') for p in paths): print('SKILL_MD')
elif any(p.endswith('.complete') for p in paths): print('COMPLETE_NO_SKILL')
elif paths: print('NONEMPTY')
else: print('EMPTY')
" 2>/dev/null || echo EMPTY)
  [ "$SKILLS" != "$PLAST" ] && { log "    plugin status → $SKILLS"; PLAST="$SKILLS"; }
  case "$SKILLS" in
    SKILL_MD) break;;            # the load-bearing success: the skill is on disk
    *) sleep 15;;                # EMPTY / NONEMPTY / COMPLETE_NO_SKILL → keep polling (restart in flight)
  esac
done
ok "E: plugin '$EXPECTED_PLUGIN' installed via post-online reconcile — /configs/$PLUGIN_PATH/SKILL.md present"

ok "ALL DELIVERY ASSERTIONS PASSED — asset channel: config+prompts+model; plugin channel: $EXPECTED_PLUGIN"
echo "PASS template-delivery-e2e: slug=$SLUG ws=$WID model=$MODEL config=${CFG_SIZE}B plugin=$EXPECTED_PLUGIN($SKILLS)" >&2
exit 0
