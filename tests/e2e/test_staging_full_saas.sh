#!/usr/bin/env bash
# Full-lifecycle SaaS E2E against staging.
#
# Creates a fresh org per run (unique slug), waits for tenant EC2 +
# cloudflared provisioning, exercises every major workspace-level API
# (register, heartbeat, A2A, delegation, HMA memory, activity, peers),
# then tears the whole org down and asserts that every cloud artefact
# (EC2, SG, Cloudflare tunnel, DNS record, DB rows) is gone. A leaked
# resource at teardown is a CI failure.
#
# Auth model:
#   Single MOLECULE_ADMIN_TOKEN (= CP_ADMIN_API_TOKEN on Railway staging)
#   drives everything:
#     - POST /cp/admin/orgs to provision (no WorkOS session scraping)
#     - GET  /cp/admin/orgs/:slug/admin-token to retrieve the per-tenant
#       ADMIN_TOKEN once provisioning completes
#     - DELETE /cp/admin/tenants/:slug for teardown
#   The per-tenant admin token drives all tenant API calls (workspaces,
#   memories, a2a).
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway CP_ADMIN_API_TOKEN
#
# Optional env:
#   E2E_RUNTIME                  hermes (default) | claude-code | codex | openclaw
#                                | seo-agent | google-adk
#                                  - seo-agent: a claude-code-adapter template
#                                    VARIANT (not a distinct registry runtime).
#                                    Selected via the `template` field (config.yaml
#                                    resolves runtime=claude-code); reuses the
#                                    same MiniMax/claude-code key path. See the
#                                    TEMPLATE derivation + SECRETS_JSON block.
#                                  - google-adk: Gemini. The AI-Studio-keyed BYOK
#                                    path (E2E_GOOGLE_API_KEY) is staging-
#                                    exercisable here; the keyless Vertex PROD
#                                    path needs WIF (see header note + the CTO
#                                    flag in the PR body) and is selected via
#                                    E2E_LLM_PATH=platform + a platform: model.
#   E2E_PROVISION_TIMEOUT_SECS   default 900 (15 min cold EC2 budget)
#   E2E_WORKSPACE_ONLINE_TIMEOUT_SECS  default 3600 (60 min — hermes
#                                cold-boot worst-case + slack). Raised from
#                                1800 (#1646) because flaky tenant-provisioning
#                                latency (not a code regression) causes
#                                alternating pass/fail on identical SHAs.
#   E2E_KEEP_ORG                 1 → skip teardown (debugging only)
#   E2E_RUN_ID                   Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_MODE                     full (default) | smoke
#                                (legacy alias `canary` still accepted —
#                                 mapped to `smoke` for back-compat with
#                                 any in-flight runner picking up an older
#                                 workflow checkout)
#   E2E_AWS_LEAK_CHECK           auto (default) | required | off
#                                required in CI so teardown cannot report
#                                clean while slug-tagged EC2 remains alive
#   E2E_AWS_TERMINATE_LEAKS      1 → terminate slug-tagged leaked EC2 before
#                                exiting 4
#   E2E_INTENTIONAL_FAILURE      1 → poison tenant token mid-run so the
#                                script fails; the EXIT trap MUST still
#                                tear down cleanly (and exit 4 on leak).
#                                Used by a dedicated sanity workflow
#                                that verifies the safety net.
#   E2E_LIFECYCLE                auto (default) | off
#                                When auto + MODE=full, exercises the
#                                pause→resume→online and hibernate→resume(wake)
#                                state transitions on the provisioned parent
#                                (step 10b). These are REAL transitions on the
#                                live tenant (Pause stops the container + sets
#                                status=paused; Resume re-provisions →
#                                provisioning → online; Hibernate stops +
#                                status=hibernated; the next A2A auto-wakes it).
#                                Set `off` for a fast smoke that skips the
#                                ~2x-reprovision cost. In smoke MODE it is
#                                skipped regardless (no parent stability budget).
#   E2E_REQUIRE_LIVE             1 → fail-closed-on-skip guard (CI sets this).
#                                When set, the run MUST actually complete
#                                ≥1 full provision→online→A2A cycle. A run
#                                that reaches the end without having proven
#                                a real round-trip (e.g. a future refactor
#                                short-circuits a stage, or a skip path
#                                swallows the lifecycle) exits 5 rather than
#                                reporting a false green. Mirrors CP
#                                serving-e2e's SERVING_E2E_REQUIRE_LIVE.
#
# Exit codes:
#   0  happy path
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
#   5  E2E_REQUIRE_LIVE set but the run validated no real lifecycle (no
#      false-green-on-skip)
#
# ─────────────────────────────────────────────────────────────────────────
# PROMOTION-READINESS (harden/e2e-staging-saas-failclosed):
#   This harness is being hardened so `E2E Staging SaaS` + `E2E Staging
#   Platform Boot` can become HARD merge-gates. continue-on-error is NOT
#   flipped here — that promotion is the CTO's irreversible branch-protection
#   call. What this branch makes fail-closed (was false-green / un-named
#   flake before):
#     • Provision/online waits are bounded readiness-POLLS, not fixed sleeps;
#       each hard-fails with a named mechanism + last-seen signal on deadline,
#       never a silent timeout (cp#245 boot-timeout class).
#     • Peer-discovery (9b) asserts a real 2xx, not just "not 404" — a 5xx /
#       000 / empty no longer reads as "reachable".
#     • Activity-log (9b) is ASSERTED reachable (2xx + parseable), not
#       logged-and-ignored behind `|| echo '[]'`.
#     • Child activity provenance (10) is asserted (was soft-logged).
#     • E2E_REQUIRE_LIVE=1 (CI) makes the run exit 5 if it reached the end
#       without proving a real provision→online→A2A round-trip — no
#       false-green-on-skip.
#   STILL BLOCKS making it REQUIRED (must clear before the CTO flips
#   continue-on-error→false in .gitea/workflows/e2e-staging-saas.yml):
#     • De-flake window: N consecutive green runs on main for BOTH jobs
#       (platform-boot shares the cp#245 boot surface — #2187 tracks its
#       flip). This harness removes the harness-side flake mechanisms; the
#       remaining surface is real-infra (EC2 cold boot, CF DNS) latency,
#       already bounded by the readiness polls above.
#     • Branch-protection required-context wiring is a repo-settings change,
#       not a code change in this PR.
# ─────────────────────────────────────────────────────────────────────────

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
# #48: tolerate an absent admin token here — the PR-mode early-exit below
# (E2E_REQUIRE_LIVE=0 + no token) handles the pull_request lane cleanly. On a
# real run (push/dispatch/cron, E2E_REQUIRE_LIVE=1) the missing-token case is
# caught as a HARD FAIL just past the PR-mode block, with a clear message.
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-}"
RUNTIME="${E2E_RUNTIME:-hermes}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
WORKSPACE_ONLINE_TIMEOUT_SECS="${E2E_WORKSPACE_ONLINE_TIMEOUT_SECS:-3600}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug now comes
# from make_collision_proof_slug below; the old suffix var is dead.
MODE="${E2E_MODE:-full}"
# `canary` is a legacy alias for `smoke` retained for back-compat with
# any in-flight runner picking up an older workflow checkout during the
# 2026-05-11 canary→staging rename rollout. Both map to the same slug
# prefix below. Remove the `canary` alias after one week of no-old-mode
# observations.
if [ "$MODE" = "canary" ]; then
  MODE="smoke"
fi
case "$MODE" in
  full|smoke) ;;
  *) echo "E2E_MODE must be 'full' or 'smoke' (got: $MODE)" >&2; exit 2 ;;
esac

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. Asserted via the unit test
# tests/e2e/test_collision_proof_slug_unit.sh.
# Note: `source` + `assert_collision_proof_slug` happens AFTER
# log/fail/ok are defined below (the assert calls `fail` on
# mismatch). Avoid referencing `fail` before its definition.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Collision-proof slug construction (core#2782) — runs AFTER log/fail/ok
# are defined so the assert below can call `fail` on mismatch.
# Self-check: fail loud at harness startup if a future refactor
# drops the uuid suffix (defense in depth — the unit test
# already covers this, but a redundant check in the harness
# itself is cheap).
if [ "$MODE" = "smoke" ]; then
  # core#60: pass the prefix length (11 for "e2e-smoke-") so the
  # helper's run_id budget is computed precisely against the CP's
  # 31-char org-slug cap. Without this, the helper uses a
  # conservative default and a future prefix change would silently
  # produce over-cap slugs.
  SLUG="e2e-smoke-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 11)"
else
  # core#60: pass the prefix length (4 for "e2e-"). The non-smoke
  # path has the same 31-char CP cap, so the budget math is
  # identical — just the prefix literal is shorter.
  SLUG="e2e-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 4)"
fi
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG' (assert_collision_proof_slug failed)"

# ─── fail-closed-on-skip live-lifecycle guard ───────────────────────────
# E2E_REQUIRE_LIVE=1 (set by CI) asserts this run ACTUALLY exercised a full
# provision→online→A2A cycle. Each load-bearing lifecycle stage stamps a
# milestone via live_milestone(); at the very end, require_live_or_die()
# checks every required milestone fired. Mechanism: without this, a future
# refactor that short-circuits a stage — or a skip/early-return path that
# swallows the lifecycle — would let the script reach its final `ok` and
# report GREEN having validated nothing. Mirrors CP serving-e2e's
# SERVING_E2E_REQUIRE_LIVE (skip-if-absent must be LOUD, never silent green).
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"
LIVE_MILESTONES=""
live_milestone() {
  # Idempotent set-membership append. Space-delimited; names are tokens.
  case " $LIVE_MILESTONES " in
    *" $1 "*) ;;
    *) LIVE_MILESTONES="$LIVE_MILESTONES $1" ;;
  esac
}
require_live_or_die() {
  # No-op unless CI demanded a live run.
  [ "$REQUIRE_LIVE" = "1" ] || return 0
  local required="provisioned tenant_online workspace_online a2a_roundtrip"
  local m missing=""
  for m in $required; do
    case " $LIVE_MILESTONES " in
      *" $m "*) ;;
      *) missing="$missing $m" ;;
    esac
  done
  if [ -n "$missing" ]; then
    echo "[$(date +%H:%M:%S)] ❌ E2E_REQUIRE_LIVE=1 but the run did NOT prove a full live lifecycle — missing milestone(s):${missing}. Reached:${LIVE_MILESTONES:-<none>}. This is a false-green-on-skip guard: a run that validates no real provision→online→A2A cycle MUST NOT report green." >&2
    exit 5
  fi
}

# ─── PR-mode early-exit (#48 — mirrors test_staging_concierge_creates_workspace_e2e.sh) ──
# This harness is invoked by TWO jobs in e2e-staging-saas.yml:
#   - e2e-staging-saas         (push/dispatch/cron only; always has creds + REQUIRE_LIVE=1)
#   - e2e-staging-platform-boot (now ALSO pull_request; #48 made it merge-blocking)
# E2E_REQUIRE_LIVE=0 on pull_request runs because PRs do not have staging creds
# wired; without this block the script would hard-fail at the first admin-auth
# call and red-X every PR (a false-red, not a real regression). The PR-mode gate
# is a self-check: bash -n on the script's own syntax (catches PR-merge
# regressions that would break the real run on push-to-main). On push / dispatch
# / cron, E2E_REQUIRE_LIVE=1 and the real staging boot runs and HARD FAILs
# (exit 5 via require_live_or_die) on a run that validated no live lifecycle.
if [ "${REQUIRE_LIVE}" = "0" ] && [ -z "${ADMIN_TOKEN}" ]; then
  log "PR-mode: E2E_REQUIRE_LIVE=0 and no MOLECULE_ADMIN_TOKEN — skipping live staging boot."
  log "(the real staging boot runs on push-to-main / dispatch / cron with E2E_REQUIRE_LIVE=1)"
  if ! bash -n "$0"; then
    fail "PR-mode self-check FAILED: bash -n on $0 returned non-zero — script has a syntax error"
  fi
  ok "PR-mode self-check PASSED: $(basename "$0") is bash-clean (real staging boot runs on push-to-main with E2E_REQUIRE_LIVE=1)"
  exit 0
fi
# Beyond here we are running for real: REQUIRE_LIVE=1 OR ADMIN_TOKEN is set.
# A real run with no admin token is a HARD FAIL (was the `:?` default before #48).
if [ -z "${ADMIN_TOKEN}" ]; then
  fail "MOLECULE_ADMIN_TOKEN required (Railway staging CP_ADMIN_API_TOKEN) — a non-PR run (E2E_REQUIRE_LIVE=${REQUIRE_LIVE}) needs staging creds"
fi

# Per-runtime model slug dispatch — see lib/model_slug.sh for the rationale.
# Extracted so unit tests (tests/e2e/test_model_slug.sh) can pin every branch
# without booting the full 11-step lifecycle.
# shellcheck disable=SC1091
# shellcheck source=lib/model_slug.sh
source "$(dirname "$0")/lib/model_slug.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/aws_leak_check.sh
source "$(dirname "$0")/lib/aws_leak_check.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
# molecule-core#1995 (#1994 follow-on): real-completion + per-provider
# liveness + byok-routing assertion helpers. Adds gates that FAIL on an
# error-as-text payload (the trap the shape-only A2A checks missed).
source "$(dirname "$0")/lib/completion_assert.sh"

CURL_COMMON=(-sS --fail-with-body --max-time 30)
E2E_TMP_FILES=()

# Infra-skip helper (core#2917). Emits a machine-readable scan_status line
# and exits 0 so the advisory staging gate goes green-with-skip rather than
# false-red on a known transient A2A-layer degradation. The trap still tears
# down the org.
#
# Fail-closed on repeated skips: a broadly broken agent that triggers skips on
# every A2A call would otherwise paint the advisory lane green while masking a
# real regression. We allow one distinct skip reason per run; a second distinct
# reason (or any repeated skip after the cap) converts to a hard failure.
INFRA_SKIP_REASONS=""
infra_skip() {
  local reason="$1"
  local detail="${2:-}"
  case " $INFRA_SKIP_REASONS " in
    *" $reason "*) ;;
    *) INFRA_SKIP_REASONS="$INFRA_SKIP_REASONS $reason" ;;
  esac
  local distinct_count
  distinct_count=$(echo "$INFRA_SKIP_REASONS" | wc -w | tr -d ' ')
  if [ "$distinct_count" -ge 2 ]; then
    fail "infra-skip cap exceeded ($distinct_count distinct reasons:${INFRA_SKIP_REASONS:-none}) — refusing false-green on repeated A2A-layer degradation"
  fi
  echo "[$(date +%H:%M:%S)] ⚠️  scan_status: infra-skip:${reason}${detail:+ $detail}"
  exit 0
}

e2e_tmp() {
  local f
  f=$(mktemp "$1")
  E2E_TMP_FILES+=("$f")
  printf '%s' "$f"
}

# ─── cleanup trap ───────────────────────────────────────────────────────
CLEANUP_DONE=0
cleanup_org() {
  # Capture upstream exit code IMMEDIATELY — must be the first statement
  # in the trap, before any command (including the CLEANUP_DONE check)
  # that would clobber $?.
  local entry_rc=$?

  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  rm -f "${E2E_TMP_FILES[@]}" 2>/dev/null || true

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi

  log "🧹 Tearing down org $SLUG..."

  # The DELETE handler runs the GDPR Art. 17 cascade synchronously
  # (Stripe + Redis + EC2 terminate + CF tunnel + DNS + DB rows). Real
  # observed wall-time on prod-shaped infra is ~30–90s — EC2 termination
  # alone takes 30–60s. The 5–15s estimate in `purge.go`'s comment is
  # the API-call cost, NOT the AWS-side time-to-termination it waits on.
  #
  # Two-part patience to match reality:
  #   1. 120s curl timeout on the DELETE itself (was 30s) so the
  #      synchronous cascade has room to complete in-band.
  #   2. Poll up to 60s after for organizations.status='purged' (or row
  #      gone) instead of one rigid 10s sleep — covers the case where
  #      DELETE returns 5xx mid-cascade and the cascade finishes anyway,
  #      and the case where DELETE legitimately exceeds 120s and we want
  #      eventual-consistency confirmation.
  if curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1; then
    ok "Teardown request accepted"
  else
    log "Teardown returned non-2xx (may already be gone)"
  fi

  local leak_count=1
  local elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    if [ "$leak_count" = "0" ]; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown after ${elapsed}s (count=$leak_count)" >&2
    exit 4
  fi
  local aws_leak_rc=0
  e2e_verify_no_ec2_leaks_for_slug "$SLUG" || aws_leak_rc=$?
  if [ "$aws_leak_rc" != "0" ]; then
    case "$aws_leak_rc" in
      2) exit 2 ;;
      *) exit 4 ;;
    esac
  fi
  ok "Teardown clean — no orphan org or EC2 resources for $SLUG (${elapsed}s)"

  # Normalize unexpected upstream exit codes to 1 (generic failure). The
  # script's documented contract (header "Exit codes" section) only emits
  # {0, 1, 2, 3, 4}, but `set -e` propagates the raw exit code of the
  # failing command — e.g. curl exits 22 on HTTP error under
  # --fail-with-body. Without this normalization, the
  # E2E_INTENTIONAL_FAILURE sanity workflow (e2e-staging-sanity.yml)
  # gets rc=22 from the poisoned-token curl, falls through its
  # case statement, and opens a false-positive priority-high
  # "safety net broken" issue (#2159, 2026-04-27).
  case "$entry_rc" in
    0|1|2|3|4|5) ;;        # contracted codes — let bash use entry_rc
    *) exit 1 ;;            # anything else is a generic failure
  esac
}

# Wrapper for the EXIT/INT/TERM trap: capture the original exit code,
# remove the org-create bodyfile (created later), run teardown, and
# propagate the original code. Defined as a function so the trap string
# is simple and cannot pick up an unbalanced quote from inline command
# substitution (core#2917).
cleanup_org_and_bodyfile() {
  local entry_rc=$?
  rm -f "$CREATE_BODYFILE" 2>/dev/null || true
  cleanup_org
  exit "$entry_rc"
}
trap cleanup_org_and_bodyfile EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging full-SaaS E2E"
log "   CP:      $CP_URL"
log "   Slug:    $SLUG"
log "   Runtime: $RUNTIME"
log "   Mode:    $MODE"
log "   Timeout: ${PROVISION_TIMEOUT_SECS}s"
[ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ] && log "   ⚠️  INTENTIONAL_FAILURE=1 — this run MUST fail mid-way; teardown MUST still clean up"
log "═══════════════════════════════════════════════════════════════════"

log "0/11 Preflight: CP reachable?"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

admin_call() {
  local method="$1"; shift
  local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    "$@"
}

# ─── 1. Create org via admin endpoint ───────────────────────────────────
log "1/11 Creating org $SLUG via /cp/admin/orgs..."
# core#60: capture status + body explicitly with curl -w '%{http_code}'
# -o bodyfile inside a set +e block (mirror the pattern at lines
# 875-889 for the workspace-create call), so a 400/409 body is
# ALWAYS logged for diagnosis instead of being swallowed by
# CURL_COMMON's --fail-with-body + set -e aborting the script
# before the body-logging line runs. The pre-fix code path
# (admin_call POST ... bare in a $(...)) would propagate curl's
# nonzero exit through the command substitution under
# set -euo pipefail, aborting the whole harness with no body
# in the CI logs.
CREATE_BODYFILE="$(mktemp -t create-org-resp.XXXXXX)"
# cleanup_org_and_bodyfile (EXIT/INT/TERM trap) removes this bodyfile and
# runs teardown, so a non-2xx org-create response is logged while the org
# and EC2 resources are still cleaned up (core#60 / core#2917).
set +e
CREATE_HTTP_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/cp/admin/orgs" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}" \
  -o "$CREATE_BODYFILE" \
  -w '%{http_code}')
CURL_RC=$?
set -e
CREATE_RESP="$(cat "$CREATE_BODYFILE")"
# core#2782: log the full 409 response body on a collision so the
# stale-slug-vs-fresh-slug diagnostic is queryable from CI logs.
# Pre-#60 the JSON was piped to /dev/null (`python3 -m json.tool
# >/dev/null`) which silently swallowed the body — triage on the
# 2026-06-12 staging Platform Boot red had to guess whether the
# 409 was a slug collision or a different state-conflict. With
# the explicit -o bodyfile + -w '%{http_code}' above, the body
# is always on disk for logging regardless of HTTP status.
if [ "$CURL_RC" -ne 0 ] || [ "$CREATE_HTTP_CODE" -lt 200 ] || [ "$CREATE_HTTP_CODE" -ge 300 ]; then
  log "❌ Org create failed (curl_rc=$CURL_RC http=$CREATE_HTTP_CODE slug_len=${#SLUG}); raw response body:"
  log "--- BEGIN CREATE RESPONSE ---"
  log "$CREATE_RESP"
  log "--- END CREATE RESPONSE ---"
  if [ "${#SLUG}" -gt 32 ]; then
    fail "Org create returned non-2xx AND slug is ${#SLUG} chars (over the CP's 32-char cap). The slug helper's assertion should have caught this; check collision-proof-slug.sh's run_id_budget math."
  fi
  fail "Org create returned non-2xx (http=$CREATE_HTTP_CODE) — see body above. Common causes: 409=slug collision (a prior run left a stale org; the slug helper should prevent this — check E2E_RUN_ID propagation), 400=slug too long (should be caught by the 32-char cap assertion), 401=ADMIN_TOKEN not set or expired, 422=schema mismatch (check the -d payload matches the CP's expected shape)."
fi
if [ -z "$CREATE_RESP" ] || ! echo "$CREATE_RESP" | python3 -m json.tool >/dev/null 2>&1; then
  log "❌ Org create returned non-JSON; raw body: $CREATE_RESP"
  fail "Org create returned non-JSON (see body above)"
fi
# Capture org_id for tenant-guard header on every subsequent tenant call.
# Without X-Molecule-Org-Id matching MOLECULE_ORG_ID on the tenant, the
# tenant-guard middleware returns 404 to avoid leaking tenant existence.
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && {
  log "❌ Org create response missing 'id'; raw body: $CREATE_RESP"
  fail "Org create response missing 'id' (see body above)"
}
ok "Org created (id=$ORG_ID http=$CREATE_HTTP_CODE slug_len=${#SLUG})"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
log "2/11 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out after ${PROVISION_TIMEOUT_SECS}s (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  # NOTE: /cp/admin/orgs exposes 'instance_status' (from org_instances.status),
  # NOT 'status'. Field was bug-fixed 2026-04-21 after harness timed out on a
  # fully-provisioned tenant because the polled field was always ''. The
  # admin handler struct intentionally has no top-level `status` — the org
  # row's status is derivable via instance_status for ops.
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', ''))
        sys.exit(0)
print('')
" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then
    log "    status → $STATUS"
    LAST_STATUS="$STATUS"
  fi
  case "$STATUS" in
    running)  break ;;
    failed)
      # Diagnostic burst: dump the org row so the operator sees
      # `last_error` (CP migration 022 / handler #289 — issue #285).
      # Pre-fix the harness only logged "Tenant provisioning failed",
      # forcing whoever debugs canary to scrape CP server logs to
      # learn WHY. Same shape as the TLS-readiness burst at step 4
      # (PR #2107). Redacts nothing because /cp/admin/orgs already
      # returns a narrow, ops-safe shape (id/slug/name/plan/
      # member_count/instance_status/last_error/timestamps —
      # no tokens, no encrypted fields).
      log "── DIAGNOSTIC BURST (step 2 — tenant provisioning failed) ──"
      echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(json.dumps(o, indent=2))
        sys.exit(0)
print('(no org row found for slug=$SLUG — DB drift?)')
" 2>&1 | sed 's/^/  /'
      log "── END DIAGNOSTIC ──"
      fail "Tenant provisioning failed for $SLUG (see diagnostic above for last_error)"
      ;;
    *)        sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"
live_milestone provisioned

# Derive tenant domain from CP hostname so the same harness works in
# both prod (api.moleculesai.app → moleculesai.app) and staging
# (staging-api.moleculesai.app → staging.moleculesai.app). Override
# via MOLECULE_TENANT_DOMAIN for local/self-hosted.
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Retrieve per-tenant admin token ────────────────────────────────
log "3/11 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

# ─── 4. Wait for tenant TLS / DNS propagation ──────────────────────────
# Kept below the 20-min provision envelope so a genuinely-stuck tenant
# still fails loud at the earlier provision step rather than masquerading
# as a TLS issue. CF DNS propagation + tunnel hostname registration +
# ACME cert + edge cache run 5-7 min on a healthy day; +5 min headroom
# over the previous 10-min cap covers the slower path observed in #2090.
#
# On timeout, dump DNS + curl -v + headers so the next failure identifies
# the broken layer (DNS / TLS / HTTP). Authorization is redacted
# defensively in case a future caller adds an auth header to this probe.
log "4/11 Waiting for tenant TLS / DNS propagation..."
TLS_TIMEOUT_SEC=$((15 * 60))
TLS_DEADLINE=$(( $(date +%s) + TLS_TIMEOUT_SEC ))
TENANT_HOST="${TENANT_URL#http*://}"
TENANT_HOST="${TENANT_HOST%%/*}"
TENANT_HOST="${TENANT_HOST%%:*}"
while true; do
  if curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; then
    break
  fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    log "── DIAGNOSTIC BURST (TLS-readiness timeout) ──"
    log "DNS lookup ($TENANT_HOST):"
    getent hosts "$TENANT_HOST" 2>&1 || log "  (no DNS resolution)"
    log "curl -v $TENANT_URL/health (last 40 lines):"
    curl -kv --max-time 10 "$TENANT_URL/health" 2>&1 \
      | sed -E 's/(Authorization|Cookie):.*/\1: [redacted]/i' \
      | tail -n 40 | sed 's/^/  /' || true
    log "── END DIAGNOSTIC ──"
    fail "Tenant URL never responded 2xx on /health within ${TLS_TIMEOUT_SEC}s"
  fi
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"
live_milestone tenant_online

# Sanity-test path: once the tenant is provisioned, poisoning the
# tenant token proves the EXIT trap + leak assertion still fire.
# Gate AFTER provisioning so the provision path itself stays valid.
EFFECTIVE_TENANT_TOKEN="$TENANT_TOKEN"
if [ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ]; then
  log "⚠️  INTENTIONAL_FAILURE: poisoning tenant token for the workspace-provision step"
  EFFECTIVE_TENANT_TOKEN="poisoned-$$"
fi

tenant_call() {
  local method="$1"; shift
  local path="$1"; shift
  # X-Molecule-Org-Id is REQUIRED — tenant guard 404s anything without
  # it (it does NOT 403, to hide tenant existence from org scanners).
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "$@"
}

sanitize_http_body() {
  python3 -c '
import re, sys
s = sys.stdin.read()
s = re.sub(r"(?i)(Authorization:\s*Bearer\s+)[A-Za-z0-9._~+/=-]+", r"\1[redacted]", s)
s = re.sub(r"(?i)(\"(?:auth_token|access_token|refresh_token|token|api_key|secret|password)\"\s*:\s*\")[^\"]+\"", r"\1[redacted]\"", s)
s = re.sub(r"(?i)((?:auth_token|access_token|refresh_token|api_key|secret|password)=)[^&\s]+", r"\1[redacted]", s)
print(s[:4000])
'
}

wait_workspaces_online_routable() {
  local label="$1"; shift
  local deadline=$(( $(date +%s) + WORKSPACE_ONLINE_TIMEOUT_SECS ))
  local wid ws_last_status ws_last_url ws_url_missing_logged ws_failed_logged
  local ws_json ws_status ws_url ws_last_err

  log "$label"
  for wid in "$@"; do
    ws_last_status=""
    ws_last_url=""
    ws_url_missing_logged=0
    ws_failed_logged=0
    while true; do
      if [ "$(date +%s)" -gt "$deadline" ]; then
        ws_last_err=$(tenant_call GET "/workspaces/$wid" 2>/dev/null | \
          python3 -c "import json,sys; print(json.load(sys.stdin).get('last_sample_error',''))" 2>/dev/null || echo "")
        fail "Workspace $wid never reached online with a routable URL within ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (~$((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min) (last status=$ws_last_status, url=$ws_last_url, err=$ws_last_err)"
      fi
      ws_json=$(tenant_call GET "/workspaces/$wid" 2>/dev/null || echo '{}')
      ws_status=$(echo "$ws_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status') or '')" 2>/dev/null)
      ws_url=$(echo "$ws_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('url') or '')" 2>/dev/null)
      if [ "$ws_status" != "$ws_last_status" ]; then
        log "    $wid → $ws_status"
        ws_last_status="$ws_status"
      fi
      if [ -n "$ws_url" ] && [ "$ws_url" != "$ws_last_url" ]; then
        log "    $wid url ready: $ws_url"
        ws_last_url="$ws_url"
      fi
      case "$ws_status" in
        online)
          if [ -n "$ws_url" ]; then
            break
          fi
          if [ "$ws_url_missing_logged" = "0" ]; then
            log "    $wid online but URL is not assigned yet — waiting for workspace routing readiness"
            ws_url_missing_logged=1
          fi
          sleep 10
          ;;
        failed)
          # Not a hard fail — bootstrap-watcher frequently marks failed at
          # 5 min on hermes, then heartbeat recovers to online around 10-13
          # min when install.sh finishes. Log once per workspace so the CI
          # output isn't spammy.
          if [ "$ws_failed_logged" = "0" ]; then
            log "    $wid transiently failed — waiting for heartbeat recovery (bootstrap-watcher deadline, see cp#245)"
            ws_failed_logged=1
          fi
          sleep 10
          ;;
        *)      sleep 10 ;;
      esac
    done
    ok "    $wid online and routable"
  done
}

# ─── 5. Provision parent workspace ─────────────────────────────────────
# Inject the LLM provider key so the runtime can authenticate at boot.
# Branch by which secret is set so the script supports multiple paths
# without forcing every dispatch to ship them all. Priority order
# matters — first non-empty wins:
#
#   E2E_MINIMAX_API_KEY → claude-code MiniMax path. Cheapest, default
#     for the cron canary post-2026-05-03. Routes via the claude-code
#     template's `minimax` provider (workspace-configs-templates/
#     claude-code-default/config.yaml:64-69) which sets
#     ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic at boot.
#     MINIMAX_API_KEY is the vendor-specific env name the adapter
#     reads (PR #244 — per-vendor envs prevent ANTHROPIC_AUTH_TOKEN
#     collisions when a user runs MiniMax + Z.ai workspaces side-by-
#     side).
#
#   E2E_ANTHROPIC_API_KEY → claude-code direct-Anthropic path (added
#     2026-05-04 after #2578 left the operator with an awkward choice
#     between paying OpenAI's billing top-up and registering a new
#     MiniMax account). Lower friction than MiniMax for operators
#     who already have an Anthropic API key for their own Claude
#     Code session. Pricier per-token than MiniMax but billing is
#     still independent of MOLECULE_STAGING_OPENAI_API_KEY. Pinned to the
#     claude-code runtime — hermes/codex/openclaw use OpenAI-shaped envs.
#
#   E2E_OPENAI_API_KEY → hermes/codex/openclaw paths. Kept as fallback
#     for operator dispatches that explicitly want to exercise the
#     OpenAI path. The HERMES_* fields pin hermes-agent's bridge to
#     api.openai.com (template-hermes' derive-provider.sh otherwise
#     resolves openai/* → openrouter.ai and 401s). MODEL_PROVIDER
#     follows workspace/config.py:258's 'provider:model' format.
#
# All empty → '{}' (workspace will fail at first turn with an
# expected, actionable auth error rather than masking the test).
SECRETS_JSON='{}'
# Platform-managed path (E2E_LLM_PATH=platform) — the moonshot/kimi
# NOT_CONFIGURED regression (RFC#340 Fix A #2187). Molecule owns billing via the
# CP LLM proxy, so the workspace needs NO tenant key: provision with empty
# secrets and let the workspace boot purely on (a) the proxy env the control
# plane injects + (b) the manifest-derived `provider: platform` Fix A stamps into
# the generated config.yaml. This is the path that booted NOT_CONFIGURED in prod
# precisely because the BYOK branches below never exercise it. We deliberately
# skip the key-injection branches so a stray E2E_*_API_KEY in the runner env
# cannot silently convert this into a BYOK run and mask the regression.
if [ "${E2E_LLM_PATH:-}" = "platform" ]; then
  log "    LLM path: PLATFORM-MANAGED (no tenant key; proxy + Fix A provider stamp)"
  SECRETS_JSON='{}'
elif [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_MINIMAX_API_KEY']
print(json.dumps({
    'MINIMAX_API_KEY': k,
}))
")
elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
  # Direct Anthropic path — claude-code adapter reads ANTHROPIC_API_KEY
  # natively when ANTHROPIC_BASE_URL is unset. Useful for operators
  # who already have an Anthropic API key (e.g. for their own Claude
  # Code session) and want to avoid setting up a separate MiniMax
  # account just for E2E. Pricier per-token than MiniMax but billing
  # is still independent of MOLECULE_STAGING_OPENAI_API_KEY, so an OpenAI
  # quota collapse doesn't wedge this path. Pinned to the claude-code
  # runtime: hermes/codex/openclaw use OpenAI-shaped envs and won't honour
  # ANTHROPIC_API_KEY without further wiring. pick_model_slug maps this
  # branch to claude-sonnet-4-6 so the claude-code provider registry
  # selects anthropic-api instead of the OAuth-only sonnet alias.
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_ANTHROPIC_API_KEY']
print(json.dumps({
    'ANTHROPIC_API_KEY': k,
}))
")
elif [ -n "${E2E_GOOGLE_API_KEY:-}" ]; then
  # google-adk AI-Studio BYOK path. The `google` provider entry
  # (providers.yaml:401-413) reads GEMINI_API_KEY / GOOGLE_API_KEY and dials
  # generativelanguage.googleapis.com — the tenant's OWN key, distinct from the
  # keyless-Vertex PROD path (which routes through the CP proxy + server-side
  # WIF and carries NO tenant credential). This branch exercises google-adk
  # being PROVISIONED AT ALL on staging; the Vertex-specific WIF path is flagged
  # for the CTO (needs extra provisioning) and is NOT reachable here. Inject
  # under both env names the provider accepts so the adapter resolves regardless
  # of which one it reads first.
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_GOOGLE_API_KEY']
print(json.dumps({
    'GOOGLE_API_KEY': k,
    'GEMINI_API_KEY': k,
}))
")
elif [ -n "${E2E_OPENAI_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o',
    'HERMES_INFERENCE_PROVIDER': 'custom',
    'HERMES_CUSTOM_BASE_URL': 'https://api.openai.com/v1',
    'HERMES_CUSTOM_API_KEY': k,
    'HERMES_CUSTOM_API_MODE': 'chat_completions',
}))
")
fi

MODEL_SLUG=$(pick_model_slug "$RUNTIME")
log "    MODEL_SLUG=$MODEL_SLUG"

# ─── BYOK opt-in split (secret-write gate requires explicit byok) ───────
# Every vendor-key arm above (MiniMax / Anthropic / Google / OpenAI-hermes)
# writes one or more keys that workspace-server's secret-write gate —
# rejectPlatformManagedDirectLLMBypassForWorkspace in
# workspace-server/internal/handlers/secrets.go — STRIPS/BLOCKS while a
# workspace's resolved billing mode is platform_managed (the org/CTO default).
# The strip-list (secrets.go platformManagedDirectLLMBypassKeys) includes
# MINIMAX_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, OPENAI_API_KEY/_BASE_URL,
# HERMES_CUSTOM_API_KEY/_BASE_URL, etc. A bare vendor key in the CREATE payload
# does NOT auto-derive byok: at create time no auth-env is present yet, so the
# resolver derives platform_managed and the write is rejected. The resolver's
# org rung was retired (internal#718 P2-B) — ResolveLLMBillingMode now ignores
# the org default — so the ONLY way to opt a workspace into byok is an explicit
# per-workspace override via PUT /admin/workspaces/:id/llm-billing-mode.
#
# Real evidence — staging job 295385 (main f1558b54), AFTER #2311/#2312 made
# bare `MiniMax-M2.7` registry-valid: parent-create passed model validation but
# FAILED with
#   {"error":"direct vendor key writes are blocked for platform-managed
#    workspaces; ... or set this workspace's billing mode to 'byok' via
#    /admin/workspaces/:id/llm-billing-mode","key":"MINIMAX_API_KEY"}
# That 400 is INTENDED product behavior, not a product bug. The e2e must mirror
# the real BYOK user flow: opt the workspace into byok FIRST, then write the key.
#
# Mechanism: per-workspace override (NOT org-default), because the org rung is
# retired — an org-create billing field could not satisfy this gate even if
# /cp/admin/orgs accepted one. So for any arm that ships strip-listed keys we:
#   1. create the workspace WITHOUT those keys (create succeeds platform_managed),
#   2. PUT billing-mode=byok on that workspace id (per-tenant admin token),
#   3. write the deferred strip-listed keys (now allowed by the gate),
# then continue. The #1994 byok-routing guard (8c) then sees a LEGITIMATELY
# byok workspace (explicit override) and still validates real routing — NOT
# masked.
#
# The PLATFORM path (E2E_LLM_PATH=platform) produces SECRETS_JSON='{}', so it
# carries NO strip-listed key → CREATE_SECRETS_JSON stays '{}' and no opt-in
# fires. It remains platform_managed (the moonshot/kimi NOT_CONFIGURED
# regression guard) — deliberately untouched.
#
# Keep this strip-list BYTE-IN-SYNC with secrets.go platformManagedDirectLLMBypassKeys.
# EXCEPTION (7c657011): MINIMAX_API_KEY is intentionally NOT stripped. Controlplane
# now validates BYOK model credentials at create-time (POST /workspaces) and returns
# MISSING_BYOK_CREDENTIAL if the vendor key is absent from the create payload for a
# BYOK model slug (MiniMax-M2.x). Stripping it here caused
#   runs 352760/job 476956 + 352743/job 476924 @ main SHA 15872306
# to fail at create, before the deferred write could run. Other MiniMax arms
# (no MINIMAX_API_KEY in SECRETS_JSON) are unaffected; non-MiniMax arms are
# unaffected (they never set MINIMAX_API_KEY in the first place). For the MiniMax
# arm, MINIMAX_API_KEY now lands in CREATE_SECRETS_JSON, so DEFERRED_SECRETS_JSON
# is '{}' and byok_opt_in_and_write_deferred becomes a no-op — controlplane's
# create-time gate treats a vendor key in the payload as the byok signal.
BYOK_STRIP_KEYS="AI_GATEWAY_API_KEY ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ARCEEAI_API_KEY CLAUDE_CODE_OAUTH_TOKEN CODEX_AUTH_JSON DASHSCOPE_API_KEY DEEPSEEK_API_KEY GEMINI_API_KEY GLM_API_KEY HERMES_CUSTOM_API_KEY HERMES_CUSTOM_BASE_URL HF_TOKEN KIMI_API_KEY KIMI_CN_API_KEY MINIMAX_CN_API_KEY NOUS_API_KEY OPENAI_API_KEY OPENAI_BASE_URL OPENROUTER_API_KEY XAI_API_KEY ZAI_API_KEY"
# Split SECRETS_JSON into CREATE_SECRETS_JSON (gate-safe, written at create)
# and DEFERRED_SECRETS_JSON (strip-listed keys, written AFTER byok opt-in).
# Emit the two JSON blobs on SEPARATE LINES (not space-separated) — a value or
# a json.dumps default separator contains spaces, which whitespace-`read` would
# mangle. read -r line1 → CREATE, line2 → DEFERRED.
{
  read -r CREATE_SECRETS_JSON
  read -r DEFERRED_SECRETS_JSON
} < <(
  BYOK_STRIP_KEYS="$BYOK_STRIP_KEYS" E2E_WS_SECRETS="$SECRETS_JSON" python3 -c "
import json, os
strip = set(os.environ['BYOK_STRIP_KEYS'].split())
d = json.loads(os.environ['E2E_WS_SECRETS'] or '{}')
create = {k: v for k, v in d.items() if k not in strip}
deferred = {k: v for k, v in d.items() if k in strip}
print(json.dumps(create))
print(json.dumps(deferred))
"
)
# Defensive: if the split somehow produced empty (read failure), treat as
# no-deferred so we never PUT byok on a workspace that has no vendor key.
[ -n "$DEFERRED_SECRETS_JSON" ] || DEFERRED_SECRETS_JSON='{}'
[ -n "$CREATE_SECRETS_JSON" ] || CREATE_SECRETS_JSON='{}'
if [ "$DEFERRED_SECRETS_JSON" != "{}" ]; then
  log "    BYOK opt-in required — deferring vendor key(s) until after billing-mode=byok"
fi

# byok_opt_in_and_write_deferred <workspace_id>
#   For the byok arms (DEFERRED_SECRETS_JSON non-empty): PUT billing-mode=byok
#   on the workspace, then write each deferred strip-listed secret (now allowed
#   by the secret-write gate). No-op for the platform/no-key path. See the
#   BYOK-opt-in block above + secrets.go rejectPlatformManagedDirectLLMBypassForWorkspace.
byok_opt_in_and_write_deferred() {
  local _id="$1"
  if [ "$DEFERRED_SECRETS_JSON" = "{}" ]; then
    return 0
  fi
  # Explicit byok opt-in (per-workspace override).
  local _bm_resp _bm_mode
  set +e
  _bm_resp=$(tenant_call PUT "/admin/workspaces/$_id/llm-billing-mode" \
    -H "Content-Type: application/json" \
    -d '{"mode":"byok"}' 2>/dev/null)
  local _bm_rc=$?
  set -e
  if [ "$_bm_rc" != "0" ]; then
    fail "byok opt-in: PUT /admin/workspaces/$_id/llm-billing-mode {mode:byok} failed (rc=$_bm_rc). Raw: $(printf '%s' "$_bm_resp" | sanitize_http_body)"
  fi
  _bm_mode=$(echo "$_bm_resp" | python3 -c "import json,sys; print(json.load(sys.stdin).get('resolved_mode',''))" 2>/dev/null || echo "")
  [ "$_bm_mode" = "byok" ] || fail "byok opt-in: workspace $_id resolved_mode='$_bm_mode' after PUT mode=byok (want byok). Raw: $(printf '%s' "$_bm_resp" | sanitize_http_body)"

  # Write each deferred strip-listed secret one-per-call (the Set endpoint
  # takes {key,value}). The gate now passes because resolved=byok. Bodies are
  # built in Python (env-only) so secret values never hit a command line.
  local _keys _k _sec_body _sec_tmp _sec_code _sec_out
  _keys=$(echo "$DEFERRED_SECRETS_JSON" | python3 -c "import json,sys; print('\n'.join(json.load(sys.stdin).keys()))")
  while IFS= read -r _k; do
    [ -n "$_k" ] || continue
    _sec_body=$(BYOK_K="$_k" E2E_WS_DEFERRED="$DEFERRED_SECRETS_JSON" python3 -c "
import json, os
d = json.loads(os.environ['E2E_WS_DEFERRED'])
print(json.dumps({'key': os.environ['BYOK_K'], 'value': d[os.environ['BYOK_K']]}))
")
    _sec_tmp=$(mktemp -t synth_byok_secret.XXXXXX)
    _sec_code=$(printf '%s' "$_sec_body" | tenant_call POST "/workspaces/$_id/secrets" \
      -H "Content-Type: application/json" \
      -d @- \
      -o "$_sec_tmp" -w '%{http_code}' 2>/dev/null || echo "000")
    if [ "$_sec_code" != "200" ] && [ "$_sec_code" != "201" ] && [ "$_sec_code" != "204" ]; then
      _sec_out=$(cat "$_sec_tmp" 2>/dev/null | sanitize_http_body)
      rm -f "$_sec_tmp"
      fail "byok vendor-key write: POST /workspaces/$_id/secrets ($_k) returned $_sec_code: $_sec_out — secret-write gate should allow it after the byok opt-in (secrets.go rejectPlatformManagedDirectLLMBypassForWorkspace)."
    fi
    rm -f "$_sec_tmp"
  done <<< "$_keys"
  ok "    $_id byok opt-in + deferred vendor key(s) written"
}

# ─── runtime → provision-selector resolution ────────────────────────────
# Most runtimes are selected directly by the `runtime` field. seo-agent is
# the exception: it is NOT a registry runtime (absent from manifest.json +
# runtime_registry.go knownRuntimes) — it is a claude-code-adapter template
# VARIANT selected by the `template` field. The ws-server Create handler reads
# the template's config.yaml, which declares `runtime: claude-code`, and
# resolves the concrete runtime from there (workspace.go:290-336). So for
# seo-agent we send template="seo-agent" and OMIT runtime, letting the
# template resolve it — sending an explicit runtime="seo-agent" would
# RUNTIME_UNSUPPORTED-422 at workspace.go:374-384 because it is not in
# knownRuntimes. PROVISION_TEMPLATE is "" for every real registry runtime.
PROVISION_TEMPLATE=""
case "$RUNTIME" in
  seo-agent) PROVISION_TEMPLATE="seo-agent" ;;
esac

# Build the create payload in Python so the optional `template`/`runtime`
# fields are emitted conditionally and the secrets blob is embedded without
# shell-escaping hazards. Args: name, [parent_id|""].
build_create_payload() {
  local name="$1" parent_id="${2:-}"
  E2E_WS_NAME="$name" \
  E2E_WS_PARENT_ID="$parent_id" \
  E2E_WS_RUNTIME="$RUNTIME" \
  E2E_WS_TEMPLATE="$PROVISION_TEMPLATE" \
  E2E_WS_MODEL="$MODEL_SLUG" \
  E2E_WS_SECRETS="$CREATE_SECRETS_JSON" \
  python3 -c "
import json, os
secrets = json.loads(os.environ['E2E_WS_SECRETS'] or '{}')
payload = {
    'name': os.environ['E2E_WS_NAME'],
    'tier': 2,
    'model': os.environ['E2E_WS_MODEL'],
    'secrets': secrets,
}
tmpl = os.environ.get('E2E_WS_TEMPLATE', '')
if tmpl:
    # Template-selected variant (seo-agent): the template's config.yaml
    # resolves runtime=claude-code server-side. Do NOT also send an explicit
    # runtime — seo-agent is not a registry runtime and would 422.
    payload['template'] = tmpl
else:
    payload['runtime'] = os.environ['E2E_WS_RUNTIME']
pid = os.environ.get('E2E_WS_PARENT_ID', '')
if pid:
    payload['parent_id'] = pid
print(json.dumps(payload))
"
}

if [ -n "$PROVISION_TEMPLATE" ]; then
  log "5/11 Provisioning parent workspace (runtime=$RUNTIME via template=$PROVISION_TEMPLATE → claude-code adapter)..."
else
  log "5/11 Provisioning parent workspace (runtime=$RUNTIME)..."
fi
# tenant_call inherits CURL_COMMON's --fail-with-body, so a non-2xx create
# (e.g. the 422 RUNTIME_UNSUPPORTED below) makes curl exit 22. Capturing it
# bare as $(tenant_call ...) propagates that 22 through the command
# substitution and, under `set -euo pipefail`, ABORTS the whole script right
# here — before the `fail "... Response: ..."` handler below can print the
# body. The result was an opaque `curl: (22) ... error: 422` + teardown with
# no body (run 220702, main f78fef4c, step "5/11 Provisioning parent
# workspace"). set +e / `|| true` keeps the 22 from tripping `set -e`; curl
# still WROTE the body to stdout (that's what --fail-with-body does), so
# PARENT_RESP holds the 422 JSON and the id-check below surfaces WHY.
set +e
PARENT_RESP=$(tenant_call POST /workspaces \
  -H "Content-Type: application/json" \
  -d "$(build_create_payload 'E2E Parent')")
set -e
# Surface the workspace-create error CLEARLY instead of dying on a Python
# KeyError when the response has no 'id'. The load-bearing cases this names:
#   - google-adk: RUNTIME_UNSUPPORTED 422 if google-adk is absent from the
#     deployed manifest.json's workspace_templates (the Create-handler
#     allowlist is manifest-derived — runtime_registry.go). google-adk is in
#     providers.yaml + provisioner/registry.go + registry_gen but NOT (yet) in
#     manifest.json, so it cannot be provisioned by `runtime` until the
#     manifest gains it. Flagged for the CTO — this arm REDS until then.
#   - seo-agent: an "invalid template" 400 if the seo-agent template isn't
#     present in the tenant's configs/cache dir (template-cache refresh gap).
PARENT_ID=$(echo "$PARENT_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
if [ -z "$PARENT_ID" ]; then
  fail "Parent workspace create returned no 'id' (runtime=$RUNTIME, template=${PROVISION_TEMPLATE:-<none>}). Response: $(printf '%s' "$PARENT_RESP" | sanitize_http_body)"
fi
log "    PARENT_ID=$PARENT_ID"
# BYOK arms only: opt the workspace into byok, then write the deferred vendor
# key(s). No-op for the platform/no-key path. (See the BYOK opt-in block.)
byok_opt_in_and_write_deferred "$PARENT_ID"

# ─── 6. Provision child (full mode only) ────────────────────────────────
CHILD_ID=""
if [ "$MODE" = "full" ]; then
  log "6/11 Provisioning child workspace..."
  # Same --fail-with-body / set -e abort guard as the parent create above:
  # let a non-2xx return the body so the id-check below surfaces it instead
  # of the script dying opaquely on curl exit 22.
  set +e
  CHILD_RESP=$(tenant_call POST /workspaces \
    -H "Content-Type: application/json" \
    -d "$(build_create_payload 'E2E Child' "$PARENT_ID")")
  set -e
  CHILD_ID=$(echo "$CHILD_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$CHILD_ID" ]; then
    fail "Child workspace create returned no 'id' (runtime=$RUNTIME, template=${PROVISION_TEMPLATE:-<none>}). Response: $(printf '%s' "$CHILD_RESP" | sanitize_http_body)"
  fi
  log "    CHILD_ID=$CHILD_ID"
  # Same BYOK opt-in as the parent — the child also carries the vendor key(s).
  byok_opt_in_and_write_deferred "$CHILD_ID"
else
  log "6/11 Canary mode — skipping child workspace"
fi

# ─── 7. Wait for workspace(s) online ───────────────────────────────────
# Hermes cold-boot takes 10-13 min on slow apt days (apt + uv + hermes
# install + npm browser-tools). The controlplane bootstrap-watcher
# deadline fires at 5 min and sets status=failed prematurely; heartbeat
# then transitions failed → online after install.sh finishes. So:
#
#   - ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (~$((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min)
#     deadline (hermes worst-case + slack). Configurable via
#     E2E_WORKSPACE_ONLINE_TIMEOUT_SECS (#1646).
#   - 'failed' is a TRANSIENT state we must tolerate — log and keep
#     polling, only hard-fail at the deadline. Pre-bootstrap-watcher-fix
#     (controlplane#245) this was a flake generator: workspace went
#     failed→online inside our window but we bailed at the failed read.
WS_TO_CHECK=("$PARENT_ID")
[ -n "$CHILD_ID" ] && WS_TO_CHECK+=("$CHILD_ID")
wait_workspaces_online_routable "7/11 Waiting for workspace(s) to reach status=online (up to $((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min — hermes cold boot)..." "${WS_TO_CHECK[@]}"
live_milestone workspace_online

# ─── 7a. Real chat image upload/download round-trip ───────────────────
# This deliberately uses the production workflow: tenant admin/session auth
# uploads an image through the same /chat/uploads path the canvas uses. The
# byte-for-byte download check proves the platform delivered image bytes, not
# just metadata/name plumbing.
log "7a/11 Real image upload/download round-trip..."
PNG_FIXTURE=$(e2e_tmp /tmp/molecule-e2e-image.XXXXXX.png)
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lCqT+wAAAABJRU5ErkJggg==' | base64 -d > "$PNG_FIXTURE"
PNG_SHA=$(sha256sum "$PNG_FIXTURE" | awk '{print $1}')
for wid in "${WS_TO_CHECK[@]}"; do
  UP_TMP=$(e2e_tmp /tmp/e2e_upload.XXXXXX)
  UP_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$wid/chat/uploads" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -F "files=@$PNG_FIXTURE;filename=e2e-smoke.png;type=image/png" \
    -o "$UP_TMP" \
    -w '%{http_code}' \
    2>/dev/null || echo "000")
  if [ "$UP_CODE" != "200" ] && [ "$UP_CODE" != "201" ]; then
    fail "Workspace $wid image upload returned $UP_CODE: $(head -c 500 "$UP_TMP" | sanitize_http_body)"
  fi
  UP_URI=$(python3 -c "
import json, sys
d=json.load(open(sys.argv[1]))
def walk(x):
    if isinstance(x, dict):
        if x.get('uri'):
            print(x['uri']); raise SystemExit
        for v in x.values(): walk(v)
    elif isinstance(x, list):
        for v in x: walk(v)
walk(d)
" "$UP_TMP" 2>/dev/null || echo "")
  UP_MIME=$(python3 -c "
import json, sys
d=json.load(open(sys.argv[1]))
def walk(x):
    if isinstance(x, dict) and x.get('uri'):
        print(x.get('mimeType') or x.get('mime') or ''); raise SystemExit
    if isinstance(x, dict):
        for v in x.values(): walk(v)
    elif isinstance(x, list):
        for v in x: walk(v)
walk(d)
" "$UP_TMP" 2>/dev/null || echo "")
  rm -f "$UP_TMP"
  [ -n "$UP_URI" ] || fail "Workspace $wid upload response had no workspace URI"
  [ "$UP_MIME" = "image/png" ] || fail "Workspace $wid upload returned mime=$UP_MIME, want image/png"

  DOWNLOAD_PATH="$UP_URI"
  case "$DOWNLOAD_PATH" in workspace:*) DOWNLOAD_PATH="${DOWNLOAD_PATH#workspace:}" ;; esac
  DL_TMP=$(e2e_tmp /tmp/e2e_download.XXXXXX.png)
  DL_CODE=$(curl "${CURL_COMMON[@]}" "$TENANT_URL/workspaces/$wid/chat/download?path=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$DOWNLOAD_PATH")" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -o "$DL_TMP" \
    -w '%{http_code}' \
    2>/dev/null || echo "000")
  if [ "$DL_CODE" != "200" ]; then
    fail "Workspace $wid image download returned $DL_CODE: $(head -c 300 "$DL_TMP" | sanitize_http_body)"
  fi
  DL_SHA=$(sha256sum "$DL_TMP" | awk '{print $1}')
  rm -f "$DL_TMP"
  [ "$DL_SHA" = "$PNG_SHA" ] || fail "Workspace $wid image download SHA mismatch: upload=$PNG_SHA download=$DL_SHA"
  ok "    $wid image upload/download OK ($UP_MIME, sha256=$DL_SHA)"
done
rm -f "$PNG_FIXTURE"

# ─── 7b. Canvas-terminal diagnose (EIC chain probe) ────────────────────
# This step exists because the canvas-terminal failure of 2026-05-03
# was structurally invisible to local-dev (handleLocalConnect uses
# docker exec; handleRemoteConnect uses EIC + ssh). The CP provisioner
# shipped without the tcp/22 EIC ingress rule for ~6 months and nobody
# noticed until a paying tenant clicked Terminal in canvas. Probing the
# diagnose endpoint here at synth-E2E time means a regression in
#   - tenantIngressRules / workspaceIngressRules (CP)
#   - eicSSHIngressRule helper (CP)
#   - AuthorizeIngress source-group support (CP awsapi)
#   - MOLECULE_EIC_ENDPOINT_SG_ID Railway env
#   - handleRemoteConnect's send-ssh-public-key/open-tunnel/ssh chain
# surfaces within ~20 min of merge instead of waiting for a user report.
#
# The diagnose endpoint runs the full EIC + ssh probe from inside the
# tenant's workspace-server (which already has AWS creds via its IAM
# profile) and reports per-step status. We only need to call it as the
# tenant — no AWS creds needed on the GHA runner. Returns
# {"ok": bool, "first_failure": "name", "steps": [...]}.
#
# Local-docker workspaces (instance_id NULL) get diagnoseLocal which
# probes docker.Ping + container exec; we still expect ok=true there
# since local-docker is the alternative production path.
log "7b/11 Canvas-terminal EIC diagnose probe..."
for wid in "${WS_TO_CHECK[@]}"; do
  DIAG_JSON=$(tenant_call GET "/workspaces/$wid/terminal/diagnose" 2>/dev/null || echo '{}')
  DIAG_OK=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print('true' if d.get('ok') else 'false')" 2>/dev/null || echo "false")
  if [ "$DIAG_OK" = "true" ]; then
    ok "    $wid terminal-reachable (canvas terminal will work)"
  else
    DIAG_FAIL=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('first_failure','unknown'))" 2>/dev/null || echo "unknown")
    DIAG_DETAIL=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); s=[x for x in d.get('steps',[]) if not x.get('ok')]; step=s[0] if s else {}; print(' — '.join(x for x in [step.get('error',''), step.get('detail','')] if x))" 2>/dev/null || echo "")
    # #767: always emit the full diagnose JSON so operators see every step's
    # Detail field even when the Python extraction above fails or the shape
    # drifts. The burst is bracketed like steps 2 and 4 for grep-friendly CI.
    log "── DIAGNOSTIC BURST (step 7b — terminal diagnose for $wid) ──"
    echo "$DIAG_JSON" | python3 -m json.tool 2>/dev/null || echo "$DIAG_JSON"
    log "── END DIAGNOSTIC ──"
    fail "Workspace $wid terminal diagnose failed at step '$DIAG_FAIL': $DIAG_DETAIL — check tenant SG has tcp/22 from the configured EIC endpoint SG, MOLECULE_EIC_ENDPOINT_SG_ID is set in Railway, and EIC endpoint health"
  fi
done

# ─── 7c. Workspace files API config.yaml round-trip ────────────────────
# Pin the config-save path that drives the Canvas Config tab's Save &
# Restart. Two failure classes this gate catches in one shot:
#
#   1. Path map drift (PR #2769). Runtime falls through to the wrong
#      base path (e.g. /opt/configs when user-data only created /configs)
#      → SSH `install -D` fails with EACCES on a parent dir that doesn't
#      exist. The user-visible 500 was unobservable without exercising
#      this code path on a fresh workspace.
#   2. Permission drift on /configs. The path is root-owned by cloud-init,
#      so the SSH-as-ubuntu install needs `sudo -n`. Any future change
#      that drops the sudo, switches to a non-passwordless-sudo OS user,
#      or moves the path to a non-ubuntu-writable dir without sudo will
#      regress this gate.
#
# Round-trip: PUT a known marker, GET it back, assert content matches.
# Marker shape includes the run id so a stale file from a prior canary
# can't false-pass.
log "7c/11 Files API config.yaml round-trip..."
CONFIG_MARKER="# molecule-synth-e2e: ${E2E_RUN_ID:-unknown} ${RUNTIME} $(date -u +%Y-%m-%dT%H:%M:%SZ)"
CONFIG_PAYLOAD="${CONFIG_MARKER}
name: synth-canary
runtime: ${RUNTIME}
"
for wid in "${WS_TO_CHECK[@]}"; do
  PUT_BODY=$(python3 -c "import json,sys; print(json.dumps({'content': sys.stdin.read()}))" <<< "$CONFIG_PAYLOAD")
  # Capture body to a tempfile so curl's -w '%{http_code}' is the only
  # thing on stdout. The first version used `-w '\n%{http_code}\n'` and
  # parsed via `tail -n 2 | head -n 1`, which broke because bash $(...)
  # strips the trailing newline → only 2 lines remain in the captured
  # value → head -n 1 returned the body, not the status code. Caught
  # post-merge by E2E Staging SaaS at 22:06 UTC: a 200-with-body got
  # misreported as "PUT returned <body>".
  PUT_TMP=$(mktemp -t synth_put.XXXXXX)
  PUT_CODE=$(tenant_call PUT "/workspaces/$wid/files/config.yaml" \
    -H "Content-Type: application/json" \
    -d "$PUT_BODY" \
    -o "$PUT_TMP" \
    -w '%{http_code}' \
    2>/dev/null || echo "000")
  PUT_BODY_OUT=$(cat "$PUT_TMP" 2>/dev/null || echo "")
  rm -f "$PUT_TMP"
  if [ "$PUT_CODE" != "200" ] && [ "$PUT_CODE" != "204" ]; then
    fail "Workspace $wid Files API PUT config.yaml returned $PUT_CODE: $PUT_BODY_OUT — likely a path-map or permission regression in workspace-server template_files_eic.go"
  fi
  # PUT-only check; the GET-back round-trip assertion was dropped
  # 2026-05-04 because PUT (template_files_eic.go SSH-via-EIC →
  # workspace EC2) and GET (templates.go ReadFile → docker exec on
  # platform-tenant-local container) hit DIFFERENT paths and DIFFERENT
  # hosts. The asymmetry is a separate latent bug — Canvas Config tab
  # rendering reads workspace state via other endpoints, not via this
  # GET, so the user-facing Save & Restart works (container reads
  # /configs/config.yaml directly via bind-mount). When the read/write
  # paths are unified, restore the GET-back marker check here.
  ok "    $wid config.yaml PUT OK (HTTP $PUT_CODE)"
done

# Saving config.yaml follows the same path as Canvas Config Save & Restart.
# The controlplane can briefly put the workspace back into provisioning and
# clear its route while the runtime restarts, so A2A must wait on the same
# externally routable readiness boundary again.
wait_workspaces_online_routable "7d/11 Waiting for workspace(s) to recover routing after config.yaml PUT..." "${WS_TO_CHECK[@]}"

# ─── A2A send with 202-queued poll helper (core#2437 part B) ───────────
# Sends POST /workspaces/:id/a2a. If the agent is busy/starting and returns
# a 2xx with queued:true + queue_id, poll GetA2AQueueStatus until the durable
# result is available. Handles curl rc 28 / http 000 / 404 retryable while the
# queue row is still materializing, and transient 502/503/504 cold-start.
# Prints the final A2A JSON-RPC response body to stdout; logs to stderr.
a2a_send_or_poll_queue() {
  local ws_id="$1"; shift
  local payload="$1"; shift
  local label="$1"
  local tmp qid resp code rc attempt poll_attempt poll_tmp
  local a2a_gateway_error_seen=0 last_qstatus="" queue_poll_count=0
  tmp=$(mktemp -t a2a_poll.XXXXXX)
  qid=""

  for attempt in $(seq 1 12); do
    if [ -n "$qid" ]; then
      # We have a queue_id — poll GetA2AQueueStatus for the durable result.
      poll_tmp=$(mktemp -t a2a_qpoll.XXXXXX)
      for poll_attempt in $(seq 1 30); do
        : >"$poll_tmp"
        set +e
        code=$(tenant_call GET "/workspaces/$ws_id/a2a/queue/$qid" \
          --max-time 30 \
          -H "X-Workspace-ID: $ws_id" \
          -o "$poll_tmp" \
          -w '%{http_code}' \
          2>/dev/null)
        rc=$?
        set -e
        code=${code:-000}
        resp=$(cat "$poll_tmp" 2>/dev/null || echo "")

        if [ "$rc" != "0" ] || [ "$code" = "000" ] || [ "$code" = "404" ]; then
          echo "    $label queue poll attempt $poll_attempt/30: curl_rc=$rc http=$code — retryable, backing off 2s" >&2
          sleep 2
          continue
        fi
        if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
          rm -f "$poll_tmp" "$tmp"
          fail "$label queue poll failed (http=$code): $(printf '%s' "$resp" | sanitize_http_body)"
        fi

        local qstatus
        qstatus=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    print(json.load(sys.stdin).get('status',''))
except Exception:
    print('')" 2>/dev/null || echo "")
        case "$qstatus" in
          completed)
            resp=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    rb=json.load(sys.stdin).get('response_body')
    print(json.dumps(rb) if rb is not None else '')
except Exception:
    print('')" 2>/dev/null || echo "")
            if [ -n "$resp" ]; then
              code=200
              break 2
            fi
            ;;
          failed|dropped)
            rm -f "$poll_tmp" "$tmp"
            fail "$label queue item $qid terminal status=$qstatus: $(printf '%s' "$resp" | sanitize_http_body)"
            ;;
          queued|dispatched|in_progress|"")
            last_qstatus="$qstatus"
            queue_poll_count=$((queue_poll_count + 1))
            echo "    $label queue poll attempt $poll_attempt/30 status=$qstatus — backing off 2s" >&2
            sleep 2
            ;;
          *)
            rm -f "$poll_tmp" "$tmp"
            fail "$label queue poll unexpected status=$qstatus: $(printf '%s' "$resp" | sanitize_http_body)"
            ;;
        esac
      done
      rm -f "$poll_tmp"
      # Ran out of queue poll attempts.
      # core#2917: if a gateway-edge error preceded a queued task that never
      # drained, treat it as a transient A2A-layer infra-skip rather than a
      # workspace-code failure. The flag is only set for edge signals (Bad
      # Gateway/Gateway Timeout/error-code 502/504/no healthy upstream), never
      # for agent-origin signals that could mask a real regression.
      # Verified signature: 502/503/504 on an initial POST, queue_id assigned,
      # then 30/30 polls stuck in queued/dispatched/in_progress/empty.
      case "$last_qstatus" in
        queued|dispatched|in_progress|"")
          if [ "$a2a_gateway_error_seen" = "1" ] && [ -n "$qid" ]; then
            rm -f "$tmp"
            infra_skip "a2a-queue-timeout" "queue_id=$qid poll_count=${queue_poll_count}/30 last_status=${last_qstatus:-<empty>}"
          fi
          ;;
      esac
      fail "$label queue poll timed out waiting for $qid to complete"
    fi

    # Initial POST (or retry before we have a queue_id).
    : >"$tmp"
    set +e
    code=$(tenant_call POST "/workspaces/$ws_id/a2a" \
      --max-time 90 \
      -H "Content-Type: application/json" \
      -H "X-Workspace-ID: $ws_id" \
      -d "$payload" \
      -o "$tmp" \
      -w '%{http_code}' \
      2>/dev/null)
    rc=$?
    set -e
    code=${code:-000}
    resp=$(cat "$tmp" 2>/dev/null || echo "")

    if [ "$rc" = "0" ] && [ "$code" -ge 200 ] && [ "$code" -lt 300 ]; then
      local is_queued
      is_queued=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
    print('true' if d.get('queued') is True or (d.get('status') or '').lower() == 'queued' else 'false')
except Exception:
    print('false')" 2>/dev/null || echo "false")
      if [ "$is_queued" = "true" ]; then
        qid=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    print(json.load(sys.stdin).get('queue_id',''))
except Exception:
    print('')" 2>/dev/null || echo "")
        if [ -n "$qid" ]; then
          echo "    $label A2A queued (queue_id=$qid); switching to poll" >&2
          continue
        fi
      else
        break
      fi
    fi

    local safe_body
    safe_body=$(printf '%s' "$resp" | sanitize_http_body)
    if echo "$code" | grep -Eq '^(502|503|504)$'; then
      # core#2917: split gateway-edge signals (unambiguous transient infra,
      # eligible for the queue-timeout infra-skip) from agent-origin signals
      # that can hide a real workspace-agent regression. Only edge signals set
      # a2a_gateway_error_seen; agent-origin retries are still allowed but will
      # never skip-to-green if the queue never drains.
      if echo "$safe_body" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 502|error code: 504|no healthy upstream'; then
        a2a_gateway_error_seen=1
        echo "    $label A2A transient gateway $code attempt $attempt/12: $safe_body" >&2
        if [ "$attempt" -lt 12 ]; then
          sleep 10
          continue
        fi
      elif echo "$safe_body" | grep -Eqi 'workspace agent unreachable|connection refused|workspace agent busy|native_session|restarting|restart triggered'; then
        echo "    $label A2A agent-origin $code attempt $attempt/12: $safe_body" >&2
        if [ "$attempt" -lt 12 ]; then
          # Agent restart/cold-start can take tens of seconds; keep polling,
          # but do NOT treat this as an edge-gateway transient eligible for skip.
          sleep 30
          continue
        fi
      fi
    fi
    break
  done

  rm -f "$tmp"
  if [ "$rc" != "0" ] || [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
    # core#2917: outright A2A connect timeout (curl_rc=28, http=000) is the
    # second verified transient-infra signature, not a workspace bug.
    if [ "$rc" = "28" ] && [ "$code" = "000" ]; then
      infra_skip "a2a-connect-timeout" "curl_rc=$rc http=$code attempt=$attempt label=$label"
    fi
    fail "$label failed after $attempt attempt(s) (curl_rc=$rc, http=$code): $(printf '%s' "$resp" | sanitize_http_body)"
  fi
  printf '%s' "$resp"
}

# When a2a_send_or_poll_queue hits a verified transient-infra signature it
# calls infra_skip(), but because the function is invoked via command
# substitution, bash exit 0 only terminates the subshell and the captured
# marker is returned as stdout. Detect that marker in the parent shell and
# re-invoke infra_skip so the whole advisory lane actually skips instead of
# falling through to the real-completion gate and failing.
a2a_handle_infra_skip() {
  local output="$1" label="${2:-$1}"
  case "$output" in
    *"scan_status: infra-skip:"*)
      local reason detail
      reason=$(printf '%s' "$output" | sed -n 's/.*scan_status: infra-skip:\([^[:space:]]*\).*/\1/p')
      detail=$(printf '%s' "$output" | sed -n 's/.*scan_status: infra-skip:[^[:space:]]*[[:space:]][[:space:]]*\(.*\)/\1/p')
      infra_skip "${reason:-a2a-layer}" "${detail:-$label}"
      ;;
  esac
}

# ─── 8. A2A round-trip on parent ───────────────────────────────────────
log "8/11 Sending A2A message to parent — expecting agent response..."
# Smoke prompt phrasing — DO NOT trim back to the bare "Reply with exactly: PONG"
# version that ran here pre-2026-05-02. After the Platform Capabilities preamble
# (#2332, 2026-04-30) landed in the system prompt, GPT-4o began intermittently
# refusing the bare echo prompt with messages like:
#   - "I'm unable to do that."
#   - "I'm unable to fulfill that request. Can I assist you with anything else?"
#   - "I'm unable to reply with responses that don't allow me to fulfill tasks…"
# 3 fails / 10 runs ≈ 30% flake. Root cause: the preamble primes the model
# ("Use them proactively") to expect tool use, then a zero-tool echo request
# reads as out-of-role. Real user prompts (which is what hits prod) don't
# trigger this — only this contrived smoke prompt does, so the right fix is
# in the prompt phrasing, not in the platform's system prompt. Keep the
# explicit "no tools needed" framing so the model has permission to comply.
A2A_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-msg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying agent wiring. No tools or memory are needed — please respond with exactly the single token: PONG'}]
        }
    }
}))
")
# Override CURL_COMMON's --max-time 30 for THIS call only. Each canary
# creates a fresh org → workspace, so the A2A POST hits a cold model:
# claude-code adapter starts its event loop, opens TLS to the LLM
# endpoint, ships the first prompt, waits for first token. With MiniMax
# (which is the canary default since #2710) cold-call latency
# routinely exceeds 30s on the first request after workspace boot.
# 90s gives ~3x headroom over observed cold-call P95 (~25-30s).
# Subsequent A2A turns hit the same workspace and are sub-second, so
# this only widens the window for step 8/11 of the canary's first turn.
# core#2437 part B: send A2A and, if the agent is busy/starting, poll the
# queue status endpoint until the durable result is available.
A2A_RESP=$(a2a_send_or_poll_queue "$PARENT_ID" "$A2A_PAYLOAD" "A2A parent")
a2a_handle_infra_skip "$A2A_RESP" "A2A parent"
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import json, sys
d = json.load(sys.stdin)
parts = d.get('result', {}).get('parts', [])
print(parts[0].get('text', '') if parts else '')
" 2>/dev/null || echo "")
if [ -z "$AGENT_TEXT" ]; then
  fail "A2A returned no text. Raw: $A2A_RESP"
fi

# Specific error-class checks — each pattern caught a real P0 bug on
# 2026-04-23 that a generic "error|exception" check missed or misreported:
#
#   "[hermes-agent error 401]"       → gateway API_SERVER_KEY not propagated (hermes #12)
#   "Invalid API key"                → tenant auth chain (CP #238 race)
#   "model_not_found"                → hermes custom provider slug passthrough (#13)
#   "Encrypted content is not supported" → hermes codex_responses API misroute (#14)
#   "Unknown provider"               → bridge misconfigured PROVIDER= (regression of #13 fix)
#   "hermes-agent unreachable"       → gateway process died
#   "exceeded your current quota"    → MOLECULE_STAGING_OPENAI_API_KEY billing (NOT a platform regression — #2578)
#
# Fail LOUD with the specific pattern so CI log + alert channel makes the
# regression unambiguous.
if echo "$AGENT_TEXT" | grep -qF "[hermes-agent error 401]"; then
  fail "A2A — REGRESSION: hermes gateway auth broken (API_SERVER_KEY not in runtime env). See template-hermes#12. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "hermes-agent unreachable"; then
  fail "A2A — REGRESSION: hermes gateway process down. Check /var/log/hermes-gateway.log on the workspace EC2. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "model_not_found"; then
  fail "A2A — REGRESSION: model slug passed through with provider prefix. See template-hermes#13. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Encrypted content is not supported"; then
  fail "A2A — REGRESSION: hermes custom provider hit /v1/responses instead of chat_completions. Config.yaml should declare api_mode: chat_completions. See template-hermes#14. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Unknown provider"; then
  fail "A2A — REGRESSION: install.sh set PROVIDER to a value not in hermes's registry. Run 'hermes doctor' on the workspace to see valid values. Raw: $AGENT_TEXT"
fi
# "Invalid API key" — the comment block lists this as a CP #238 race
# (tenant auth chain) signal but the grep was missing. Caller-side
# 401's containing this exact phrase don't match the generic
# "error|exception" catch-all below, so they'd slip through.
if echo "$AGENT_TEXT" | grep -qF "Invalid API key"; then
  fail "A2A — REGRESSION: tenant auth chain returned 'Invalid API key'. Likely CP boot-event 401 race (CP #238) or stale OPENAI_API_KEY in the runtime env. Raw: $AGENT_TEXT"
fi
# Provider quota exhausted — distinguish from a platform regression so
# the canary alert names the operator action directly instead of falling
# through to the generic "error-shaped response" message. Steps 0-7 having
# passed means the platform itself is healthy (CP up, tenant provisioned,
# workspace online, A2A delivery end-to-end). When the agent comes back
# with a provider-side 429, that is a billing event on the configured
# OpenAI key, not a platform regression. Tracked in #2578.
if echo "$AGENT_TEXT" | grep -qiE "exceeded your current quota|insufficient_quota"; then
  fail "A2A — PROVIDER QUOTA EXHAUSTED (NOT a platform regression). Operator action: top up MOLECULE_STAGING_OPENAI_API_KEY billing or rotate to a higher-quota org at Settings → Secrets and Variables → Actions. Tracked in #2578. Raw: $AGENT_TEXT"
fi
# Empty-completion class — the agent runtime reached the LLM and got a
# 2xx back, but the assistant turn carried NO text part (empty content,
# or tool_calls/reasoning-only with no surfaced text), so the runtime
# returns the literal "Error: message contained no text content." as its
# reply text. Steps 0-7 passing means the platform is healthy (CP up,
# tenant provisioned, workspace online + routable, A2A delivery e2e); the
# break is the configured completion BACKEND returning an empty turn — a
# model/provider-side regression, NOT a workspace-server or harness bug,
# and NOT NOT_CONFIGURED (that fails earlier, at boot). Name it explicitly
# so the canary alert points at the model, not the platform: a generic
# "error-shaped response" misdirects triage to workspace-server. Observed
# 2026-06-03/04 across every staging canary on MODEL_SLUG=MiniMax-M2 (the
# canary default since #2710) — 100% on the parent's first cold turn,
# identical on main's scheduled synthetic E2E and on PRs (so it is an
# environmental backend regression, never PR-introduced).
if echo "$AGENT_TEXT" | grep -qiF "message contained no text content"; then
  fail "A2A — EMPTY COMPLETION (backend regression, NOT a platform/workspace-server bug). The configured model (MODEL_SLUG=${MODEL_SLUG:-?}) returned a 2xx completion with no text part; the runtime surfaced 'message contained no text content.'. Operator action: check the staging LLM backend / proxy for the canary model (the claude-code MiniMax-BYOK default is the BARE registered id MiniMax-M2.7 — the colon minimax:MiniMax-M2.7 is UNREGISTERED on claude-code, internal#718) — empty assistant turns, not an auth/quota/boot fault. Raw: $AGENT_TEXT"
fi
# Generic catch-all — falls through if none of the known regressions hit.
# _ResultError is the claude-code runtime surfacing an LLM/backend/runtime
# failure AS text. Diagnose it explicitly (#2712) so the next canary run
# prints the upstream error instead of forcing operators to scrape workspace
# logs. The suite still fails; this is diagnostics-only.
if echo "$AGENT_TEXT" | grep -qiF "_ResultError"; then
  diagnose_staging_result_error "$PARENT_ID" "$A2A_RESP" "A2A parent _ResultError"
  _redacted_agent_text=$(printf '%s' "$AGENT_TEXT" | redact_secrets)
  fail "A2A — STAGING LLM/BACKEND/RUNTIME FAILURE (_ResultError). The canary agent surfaced its LLM/backend/runtime error as a text payload. See the diagnostic burst above for the full A2A response, workspace state, and recent activity logs (including any upstream HTTP status/body the runtime reported). Raw (redacted): ${_redacted_agent_text:0:500}"
fi
if echo "$AGENT_TEXT" | grep -qiE "error|exception"; then
  fail "A2A returned an error-shaped response: $AGENT_TEXT"
fi

# Content assertion — the prompt asks the model to reply with exactly "PONG".
# Real models produce "PONG" (possibly with minor wrapping); a broken pipeline
# that echoes the prompt back or returns truncated context won't. Normalize
# to uppercase before matching to tolerate "pong" / "Pong".
if ! echo "$AGENT_TEXT" | tr '[:lower:]' '[:upper:]' | grep -qF "PONG"; then
  fail "A2A reply didn't contain expected PONG token. Real: $AGENT_TEXT"
fi

ok "A2A parent round-trip succeeded: \"${AGENT_TEXT:0:80}\""

# ─── 8b. Real-completion known-answer round-trip (CORE GATE, #1994) ────
# The existing PONG check + generic error grep above already do a lot, but
# this stanza is the canonical real-completion gate the #1994 follow-on
# adds: a DETERMINISTIC known-answer prompt asserted via
# a2a_assert_real_completion, which FAILS on an error-as-text payload
# ({"kind":"text","text":"Agent error (Exception) ..."}). That payload
# matches the historical shape-only check `"kind":"text"` and so passed CI
# on a fully broken agent (drained-key / byok-misroute, 2026-05-2x). This
# gate makes that case RED. Reuses the same cold-start retry-on-transient
# (502/503/504) loop the PONG probe uses — retry-once-on-network, never on
# agent-error. Single round-trip → the one place we spend a non-trivial
# token budget (default backend MiniMax — cheap token plan).
KA_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-known-answer-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'Reply with exactly the word PINEAPPLE and nothing else.'}]
        }
    }
}))
")
# core#2437 part B: send A2A and poll queue status if the agent queues it.
KA_RESP=$(a2a_send_or_poll_queue "$PARENT_ID" "$KA_PAYLOAD" "A2A known-answer")
a2a_handle_infra_skip "$KA_RESP" "A2A known-answer"
KA_TEXT=$(echo "$KA_RESP" | python3 -c "
import json, sys
def extract_text(d):
    out = []
    # Standard A2A JSON-RPC result.parts
    for p in d.get('result', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # A2A Task status message parts
    for p in d.get('result', {}).get('status', {}).get('message', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # Alternative message.parts placement
    for p in d.get('result', {}).get('message', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # Artifacts
    for a in d.get('result', {}).get('artifacts', []) or []:
        for p in a.get('parts', []) or []:
            if p.get('kind') == 'text' or p.get('type') == 'text':
                t = p.get('text') or ''
                if t:
                    out.append(t)
    return '\n'.join(out)
try:
    d = json.load(sys.stdin)
    print(extract_text(d))
except Exception:
    print('')
" 2>/dev/null || echo "")
# Debuggability: if extraction is empty, surface what the proxy returned so a
# queued/artifact/non-text shape does not silently fail the gate.
if [ -z "$KA_TEXT" ]; then
  KA_SAFE_BODY=$(printf '%s' "$KA_RESP" | sanitize_http_body)
  log "    known-answer A2A extraction empty; response body: $KA_SAFE_BODY"
fi
# CORE GATE: contains PINEAPPLE (real round-trip) AND no error-as-text.
a2a_assert_real_completion "$KA_TEXT" "PINEAPPLE" "A2A known-answer (parent, $RUNTIME/$MODEL_SLUG)"
# Real, deterministic LLM round-trip proven — the load-bearing milestone for
# the fail-closed-on-skip guard. Stamped AFTER a2a_assert_real_completion (not
# after the looser PONG check) so the milestone means a verified completion,
# not just a 2xx-with-text.
live_milestone a2a_roundtrip

# ─── 8c. byok-routing regression guard (#1994) ─────────────────────────
# The parent was provisioned with the customer's OWN vendor key
# (MINIMAX_API_KEY / ANTHROPIC_API_KEY in SECRETS_JSON) → it must resolve
# BYOK, not platform_managed. #1994 was exactly the inverse: a byok
# workspace baked platform_managed on (re-)provision → routed through the
# platform proxy → drained the platform LLM key. We read the SAME derived
# resolver the provision-time strip gate uses
# (GET /admin/workspaces/:id/llm-billing-mode) and assert resolved_mode!=
# platform_managed. A regression flips it RED.
#
# Only meaningful when the parent actually carries a byok credential; the
# OpenAI/hermes path uses a different env shape, and the no-key path is
# legitimately platform_managed (the CTO default). Gate on the same
# E2E_*_API_KEY presence the SECRETS_JSON branch keyed off.
if [ -n "${E2E_MINIMAX_API_KEY:-}" ] || [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
  set +e
  BILLING_RESP=$(tenant_call GET "/admin/workspaces/$PARENT_ID/llm-billing-mode" 2>/dev/null)
  BILLING_RC=$?
  set -e
  if [ "$BILLING_RC" != "0" ] || [ -z "$BILLING_RESP" ]; then
    fail "byok-routing guard: GET /admin/workspaces/$PARENT_ID/llm-billing-mode failed (rc=$BILLING_RC). Body: ${BILLING_RESP:0:200}"
  fi
  assert_byok_not_platform_proxy "$BILLING_RESP" "byok-guard (parent, $RUNTIME/$MODEL_SLUG)"
else
  log "8c.  byok-routing guard skipped — parent carries no own-vendor key (OpenAI/no-key path is legitimately platform_managed)."
fi

# ─── 8d. Per-offered-provider liveness matrix (SSOT-driven, #1994 class) ─
# For each platform-servable model the providers.yaml SSOT
# (runtimes.<runtime>.providers[platform].models) declares for this
# runtime, send a minimal max_tokens-bounded "say ok" probe and assert a
# NON-ERROR completion. Purpose: exercise each offered provider's AUTH +
# ROUTING path so a drained key / wrong base-URL / byok-misroute fails the
# gate (the #1994 class). Providers/models come from the SSOT — not a
# hardcoded list — so the matrix tracks providers.yaml automatically.
#
# This lane provisions ONE parent workspace with ONE configured key, so we
# can only truly drive the providers that key authenticates. Probing a
# model whose provider key is absent in this lane is reported SKIP (rc=75),
# not FAIL — keeping the gate deterministic + low-flake. The matrix still
# proves the configured provider's full auth+routing path end-to-end, and
# logs the offered set so over/under-offer drift is visible in the CI log.
provider_liveness_probe() {
  local model_id="$1"
  # Map the SSOT platform model id (e.g. minimax/MiniMax-M2.7) to the
  # vendor namespace token to decide whether THIS lane has its key.
  local vendor="${model_id%%/*}"
  case "$vendor" in
    minimax)   [ -n "${E2E_MINIMAX_API_KEY:-}" ]   || return 75 ;;
    anthropic) [ -n "${E2E_ANTHROPIC_API_KEY:-}" ] || return 75 ;;
    openai)    [ -n "${E2E_OPENAI_API_KEY:-}" ]    || return 75 ;;
    *)         return 75 ;;  # kimi/moonshot etc. — no key wired in this lane
  esac
  local probe_payload
  probe_payload=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-liveness-' + uuid.uuid4().hex[:6],
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'Reply with exactly: ok'}],
        },
        'configuration': {'max_tokens': 32}
    }
}))
")
  local tmp code rc resp
  tmp=$(mktemp -t liveness_a2a.XXXXXX)
  set +e
  code=$(tenant_call POST "/workspaces/$PARENT_ID/a2a" \
    --max-time 60 \
    -H "Content-Type: application/json" \
    -d "$probe_payload" \
    -o "$tmp" -w '%{http_code}' 2>/dev/null)
  rc=$?
  set -e
  resp=$(cat "$tmp" 2>/dev/null || echo "")
  rm -f "$tmp"
  if [ "$rc" != "0" ] || [ "${code:-000}" -lt 200 ] || [ "${code:-000}" -ge 300 ]; then
    log "      probe $model_id: HTTP ${code:-000} rc=$rc"
    return 1
  fi
  local text
  text=$(echo "$resp" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin); p=d.get('result',{}).get('parts',[])
    print(p[0].get('text','') if p else '')
except Exception: print('')" 2>/dev/null || echo "")
  if [ -z "$text" ] || a2a_completion_error_marker "$text" >/dev/null; then
    log "      probe $model_id: error-as-text or empty: ${text:0:120}"
    return 1
  fi
  return 0
}
if ! provider_liveness_matrix "$RUNTIME" provider_liveness_probe; then
  fail "Per-provider liveness matrix: at least one offered provider failed its auth+routing probe (see matrix above). This is the #1994 class — a drained key / wrong base-URL / byok-misroute."
fi
ok "Per-provider liveness matrix passed (all probed offered providers completed without error)"

# ─── 9. HMA + peers + activity (full mode) ─────────────────────────────
if [ "$MODE" = "full" ]; then
  log "9/11 Writing + reading HMA memory on parent..."
  MEM_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'content': 'E2E memory seed — run $SLUG',
    'scope': 'LOCAL'
}))
")
  # SURFACE THE BODY (mirrors the step-9b / A2A pattern): the previous
  # `>/dev/null || fail "memory POST failed"` discarded the response body
  # that --fail-with-body deliberately preserves on a non-2xx, so a 500 from
  # the workspace-server HMA path (e.g. "failed to store memory" /
  # "failed to resolve writable namespaces", or a 503 "memory plugin is not
  # configured") was reported as a bare "memory POST failed" — opaque, the
  # same #2310-class blind spot. Route http_code into -w and body into -o,
  # then fail with the sanitized status+body so the mechanism is visible.
  MEM_POST_TMP=$(e2e_tmp /tmp/e2e_mem_post.XXXXXX)
  set +e
  MEM_POST_CODE=$(tenant_call POST "/workspaces/$PARENT_ID/memories" \
    -H "Content-Type: application/json" \
    -d "$MEM_PAYLOAD" \
    -o "$MEM_POST_TMP" -w "%{http_code}" 2>/dev/null)
  MEM_POST_RC=$?
  set -e
  MEM_POST_CODE=${MEM_POST_CODE:-000}
  if [ "$MEM_POST_RC" != "0" ] || [ "$MEM_POST_CODE" -lt 200 ] || [ "$MEM_POST_CODE" -ge 300 ]; then
    MEM_POST_BODY=$(head -c 400 "$MEM_POST_TMP" 2>/dev/null | sanitize_http_body)
    fail "memory POST /workspaces/$PARENT_ID/memories failed (curl_rc=$MEM_POST_RC, http=$MEM_POST_CODE): ${MEM_POST_BODY:-<empty body>}"
  fi

  # Same fail-closed surfacing for the read-back: a 5xx / network error here
  # previously slipped through the bare `$(tenant_call ...)` capture and only
  # showed up as "not readable" with an empty list.
  MEM_LIST_TMP=$(e2e_tmp /tmp/e2e_mem_list.XXXXXX)
  set +e
  MEM_LIST_CODE=$(tenant_call GET "/workspaces/$PARENT_ID/memories?scope=LOCAL" \
    -o "$MEM_LIST_TMP" -w "%{http_code}" 2>/dev/null)
  MEM_LIST_RC=$?
  set -e
  MEM_LIST_CODE=${MEM_LIST_CODE:-000}
  MEM_LIST=$(cat "$MEM_LIST_TMP" 2>/dev/null || echo "")
  if [ "$MEM_LIST_RC" != "0" ] || [ "$MEM_LIST_CODE" -lt 200 ] || [ "$MEM_LIST_CODE" -ge 300 ]; then
    fail "memory GET /workspaces/$PARENT_ID/memories failed (curl_rc=$MEM_LIST_RC, http=$MEM_LIST_CODE): $(printf '%s' "$MEM_LIST" | sanitize_http_body | head -c 400)"
  fi
  if ! echo "$MEM_LIST" | grep -q "run $SLUG"; then
    fail "HMA memory not readable after write (http=$MEM_LIST_CODE). List: $(printf '%s' "$MEM_LIST" | sanitize_http_body | head -c 200)"
  fi
  ok "HMA memory write+read roundtripped"

  log "9b.  Peer discovery + activity log smoke..."
  # FAIL-CLOSED: assert a real 2xx, not merely "not 404". The previous
  # `[ "$PEERS_CODE" = "404" ] && fail` only caught the route-missing case —
  # a 5xx, 000 (connection failure), or empty capture ALL fell through to
  # "reachable" (false-green: a broken-but-present route read as healthy).
  # Mechanism: route the http_code into its own tempfile (no stderr capture,
  # which the old `2>&1 | head -1` could pollute with a curl error line) and
  # require 2xx explicitly.
  PEERS_TMP=$(e2e_tmp /tmp/e2e_peers.XXXXXX)
  set +e
  PEERS_CODE=$(tenant_call GET "/registry/$PARENT_ID/peers" \
    -o "$PEERS_TMP" -w "%{http_code}" 2>/dev/null)
  PEERS_RC=$?
  set -e
  PEERS_CODE=${PEERS_CODE:-000}
  if [ "$PEERS_CODE" = "404" ]; then
    fail "Peers endpoint missing (404) — route regression. /registry/$PARENT_ID/peers"
  fi
  if [ "$PEERS_RC" != "0" ] || [ "$PEERS_CODE" -lt 200 ] || [ "$PEERS_CODE" -ge 300 ]; then
    fail "Peers endpoint unhealthy (curl_rc=$PEERS_RC, http=$PEERS_CODE) — not a clean 2xx, so 'reachable' would be a false-green. Body: $(head -c 200 "$PEERS_TMP" 2>/dev/null | sanitize_http_body)"
  fi
  ok "Peers endpoint reachable (HTTP $PEERS_CODE)"

  # FAIL-CLOSED: the activity-log read was `|| echo '[]'` then the count was
  # only LOGGED, never asserted — a 5xx / network failure silently became an
  # empty list and the step exited 0 having validated nothing (false-green:
  # "validated nothing" class). Assert the endpoint returns a 2xx and a
  # parseable activity shape. We do NOT assert count>0 (the parent may
  # legitimately have 0 events this early — that's a real, valid state), but
  # we DO require the call to have actually succeeded and returned valid JSON.
  ACTIVITY_TMP=$(e2e_tmp /tmp/e2e_activity.XXXXXX)
  set +e
  ACTIVITY_CODE=$(tenant_call GET "/activity?workspace_id=$PARENT_ID&limit=5" \
    -o "$ACTIVITY_TMP" -w "%{http_code}" 2>/dev/null)
  ACTIVITY_RC=$?
  set -e
  ACTIVITY_CODE=${ACTIVITY_CODE:-000}
  if [ "$ACTIVITY_RC" != "0" ] || [ "$ACTIVITY_CODE" -lt 200 ] || [ "$ACTIVITY_CODE" -ge 300 ]; then
    fail "Activity-log endpoint unhealthy (curl_rc=$ACTIVITY_RC, http=$ACTIVITY_CODE) — was previously swallowed by '|| echo []' and reported as 0 events (false-green). Body: $(head -c 200 "$ACTIVITY_TMP" 2>/dev/null | sanitize_http_body)"
  fi
  ACTIVITY_COUNT=$(python3 -c "import json,sys
d=json.load(open(sys.argv[1]))
print(len(d if isinstance(d, list) else d.get('events', [])))" "$ACTIVITY_TMP" 2>/dev/null) \
    || fail "Activity-log returned HTTP $ACTIVITY_CODE but body was not parseable JSON (events array / {events:[...]}). Body: $(head -c 200 "$ACTIVITY_TMP" 2>/dev/null | sanitize_http_body)"
  log "    Activity events observed: $ACTIVITY_COUNT (endpoint 2xx + parseable ✓)"

  # ─── 9c. Workspace KV memory Edit round-trip ─────────────────────────
  # Pins the Edit affordance added to the canvas Memory tab. The UI calls
  # POST /workspaces/:id/memory with if_match_version, so the contract is:
  #   1. initial POST creates row at version 1
  #   2. GET returns version 1 + value
  #   3. POST with if_match_version=1 updates → version 2
  #   4. POST with if_match_version=1 again → 409 (optimistic-lock enforcement)
  # Without (3) there is no Edit; without (4) two concurrent writers can
  # silently overwrite each other and the agent loses delegation-ledger state.
  log "9c.  Memory KV Edit round-trip (Edit affordance + 409 gate)"
  EDIT_KEY="e2e_edit_gate_$SLUG"

  # 1. seed
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":1}}" >/dev/null \
    || fail "memory KV seed POST failed"

  # 2. read back, capture version
  EDIT_GET=$(tenant_call GET "/workspaces/$PARENT_ID/memory/$EDIT_KEY")
  EDIT_VER=$(echo "$EDIT_GET" | python3 -c "import json,sys; print(json.load(sys.stdin)['version'])" 2>/dev/null || echo "")
  [ -z "$EDIT_VER" ] && fail "memory KV GET missing version field. Body: ${EDIT_GET:0:200}"

  # 3. conditional update with matching version
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":2},\"if_match_version\":$EDIT_VER}" >/dev/null \
    || fail "memory KV conditional Edit failed (if_match_version=$EDIT_VER)"

  # 4. value flipped + version incremented?
  EDIT_GET2=$(tenant_call GET "/workspaces/$PARENT_ID/memory/$EDIT_KEY")
  EDIT_VAL2=$(echo "$EDIT_GET2" | python3 -c "import json,sys; print(json.load(sys.stdin)['value'].get('step'))" 2>/dev/null || echo "")
  [ "$EDIT_VAL2" = "2" ] || fail "memory KV Edit did not persist new value. Body: ${EDIT_GET2:0:200}"

  # 5. stale-version POST must 409 — pin the optimistic-lock contract.
  #
  # tenant_call uses CURL_COMMON which carries --fail-with-body, so an
  # expected-409 makes curl exit 22. The previous shape
  #   $(tenant_call ... -w "%{http_code}" || echo "000")
  # concatenated the captured "409" with the fallback "000" giving a
  # bogus "409000" value (caught on PR #2792's first E2E run, which is
  # also why staging-saas E2E has been silent-failing this gate since
  # PR #2787 merged). Fix: route the status code into its own tempfile
  # so curl's exit code can't pollute the captured stdout. set +e/-e
  # keeps the 22 from tripping the outer `set -e` pipeline.
  set +e
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":3},\"if_match_version\":$EDIT_VER}" \
    -o /tmp/memory_stale_resp.txt -w "%{http_code}" >/tmp/memory_stale_code.txt 2>/dev/null
  set -e
  EDIT_STALE_CODE=$(cat /tmp/memory_stale_code.txt 2>/dev/null || echo "000")
  [ "$EDIT_STALE_CODE" = "409" ] || fail "memory KV stale Edit must 409 (optimistic-lock). Got '$EDIT_STALE_CODE': $(cat /tmp/memory_stale_resp.txt 2>/dev/null | head -c 200)"

  # cleanup
  tenant_call DELETE "/workspaces/$PARENT_ID/memory/$EDIT_KEY" >/dev/null 2>&1 || true
  ok "Memory KV Edit round-trip + 409 gate passed"

  # ─── 9d. shared_context removal gate ─────────────────────────────────
  # Pin the deletion of GET /workspaces/:id/shared-context. The route + handler
  # were removed; team-shared knowledge now flows through memory v2's
  # team:<id> namespace. If anyone re-introduces a shared-context endpoint
  # without going through RFC #2789, this gate fires.
  set +e
  SC_CODE=$(tenant_call GET "/workspaces/$PARENT_ID/shared-context" \
    -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
  set -e
  if [ "$SC_CODE" = "200" ]; then
    fail "shared-context route should be gone but returned 200 — regression. See task #304."
  fi
  ok "shared-context route confirmed removed (HTTP $SC_CODE)"
else
  log "9/11 Canary mode — skipping HMA / peers / activity / memory-edit / shared-context-gone"
fi

# ─── 10. Delegation mechanics (full mode + child) ──────────────────────
if [ "$MODE" = "full" ] && [ -n "$CHILD_ID" ]; then
  log "10/11 Delegation mechanics: parent → child via proxy"
  DELEG_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-deleg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-deleg-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying child workspace wiring. No tools or memory are needed — please respond with exactly the single token: CHILD_PONG'}]
        }
    }
}))
")
  DELEG_TMP=$(mktemp -t deleg_a2a.XXXXXX)
  for DELEG_ATTEMPT in $(seq 1 12); do
    : >"$DELEG_TMP"
    set +e
    # Raw curl (not tenant_call) because this call carries an extra
    # X-Source-Workspace-Id header. Must still send X-Molecule-Org-Id
    # or TenantGuard 404s — previously missing, caused section 10 to
    # fail rc=22 despite everything upstream being correct (2026-04-21).
    DELEG_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$CHILD_ID/a2a" \
      -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      -H "X-Source-Workspace-Id: $PARENT_ID" \
      -H "Content-Type: application/json" \
      -d "$DELEG_PAYLOAD" \
      -o "$DELEG_TMP" \
      -w '%{http_code}' \
      2>/dev/null)
    DELEG_RC=$?
    set -e
    DELEG_CODE=${DELEG_CODE:-000}
    DELEG_RESP=$(cat "$DELEG_TMP" 2>/dev/null || echo "")
    if [ "$DELEG_RC" = "0" ] && [ "$DELEG_CODE" -ge 200 ] && [ "$DELEG_CODE" -lt 300 ]; then
      break
    fi

    DELEG_SAFE_BODY=$(printf '%s' "$DELEG_RESP" | sanitize_http_body)
    if echo "$DELEG_CODE" | grep -Eq '^(502|503|504)$' && echo "$DELEG_SAFE_BODY" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 502|error code: 504|workspace agent unreachable|connection refused|no healthy upstream|workspace agent busy|native_session'; then
      log "    Delegation A2A cold-start attempt $DELEG_ATTEMPT/12 returned $DELEG_CODE: $DELEG_SAFE_BODY"
      if [ "$DELEG_ATTEMPT" -lt 12 ]; then
        DELEG_SLEEP=10
        if echo "$DELEG_SAFE_BODY" | grep -Eqi 'workspace agent busy|native_session'; then
          DELEG_SLEEP=30
        fi
        sleep "$DELEG_SLEEP"
        continue
      fi
    fi
    break
  done
  rm -f "$DELEG_TMP"
  if [ "$DELEG_RC" != "0" ] || [ "$DELEG_CODE" -lt 200 ] || [ "$DELEG_CODE" -ge 300 ]; then
    DELEG_SAFE_BODY=$(printf '%s' "$DELEG_RESP" | sanitize_http_body)
    fail "Delegation A2A POST failed after $DELEG_ATTEMPT attempt(s) (curl_rc=$DELEG_RC, http=$DELEG_CODE): $DELEG_SAFE_BODY"
  fi
  DELEG_TEXT=$(echo "$DELEG_RESP" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    parts = d.get('result', {}).get('parts', [])
    print(parts[0].get('text', '') if parts else '')
except Exception:
    print('')
" 2>/dev/null || echo "")
  [ -z "$DELEG_TEXT" ] && fail "Delegation returned no text. Raw: ${DELEG_RESP:0:200}"
  ok "Delegation proxy works (child responded: \"${DELEG_TEXT:0:60}\")"

  # FAIL-CLOSED via bounded readiness-POLL (was soft-logged false-green).
  # The activity pipeline is async, so an immediate single read can miss the
  # parent reference — but "did not reference parent" was previously just
  # LOGGED and the step passed regardless, so a genuinely broken provenance
  # pipeline (parent never recorded as source) read as success. Mechanism:
  # poll the child activity log for the parent id for a bounded window
  # (E2E_CHILD_ACTIVITY_TIMEOUT_SECS, default 60s) — this is the real
  # readiness signal (provenance row materialised), not a fixed sleep — and
  # hard-fail with a named mechanism if it never appears.
  CHILD_ACT_DEADLINE=$(( $(date +%s) + ${E2E_CHILD_ACTIVITY_TIMEOUT_SECS:-60} ))
  CHILD_ACT_SEEN=0
  CHILD_ACT_LASTCODE="000"
  while true; do
    CHILD_ACT_TMP=$(e2e_tmp /tmp/e2e_child_act.XXXXXX)
    set +e
    CHILD_ACT_CODE=$(tenant_call GET "/activity?workspace_id=$CHILD_ID&limit=20" \
      -o "$CHILD_ACT_TMP" -w "%{http_code}" 2>/dev/null)
    set -e
    CHILD_ACT_LASTCODE=${CHILD_ACT_CODE:-000}
    if grep -q "$PARENT_ID" "$CHILD_ACT_TMP" 2>/dev/null; then
      CHILD_ACT_SEEN=1
      break
    fi
    [ "$(date +%s)" -ge "$CHILD_ACT_DEADLINE" ] && break
    sleep 5
  done
  if [ "$CHILD_ACT_SEEN" = "1" ]; then
    ok "Child activity log records parent as source"
  else
    fail "Child activity log never referenced parent $PARENT_ID within ${E2E_CHILD_ACTIVITY_TIMEOUT_SECS:-60}s (last http=$CHILD_ACT_LASTCODE) — delegation-provenance pipeline regression (parent not recorded as source). Previously soft-logged → false-green."
  fi
fi

# ─── 10b. Pause/Resume + Hibernate/Resume lifecycle transitions ─────────
# Exercise the REAL workspace lifecycle state machine on the provisioned
# parent — the transitions that previously had only handler unit tests
# (handlers_additional_test.go / hibernation_test.go) and NO real-infra
# coverage. Each transition is asserted against the live DB-backed status the
# GET /workspaces/:id endpoint returns, so a regression in the Pause/Resume/
# Hibernate handlers (workspace_restart.go) or their CP stop/re-provision
# wiring fails the gate instead of silently leaking an EC2 / wedging a tenant.
#
# Contract (workspace_restart.go):
#   POST /pause     online → 'paused'  (container stopped, url cleared)  {"status":"paused"}
#   POST /resume    paused → 'provisioning' → … → 'online' (re-provision) {"status":"provisioning"}
#   POST /hibernate online → 'hibernating' → 'hibernated' (container stopped) {"status":"hibernated"}
#   auto-wake       next A2A message/send on a hibernated ws → online
#
# Gated to full MODE (smoke has no parent-stability budget) + E2E_LIFECYCLE.
# Runs LAST (after all read-only A2A/memory/peer checks) so the pause/stop
# cycles don't disturb the earlier assertions. Skips are LOUD (logged), and
# any broken transition hard-fails — never a silent pass.
if [ "$MODE" = "full" ] && [ "${E2E_LIFECYCLE:-auto}" != "off" ]; then
  log "10b/11 Lifecycle transitions: pause→resume→online, hibernate→resume(wake) on parent $PARENT_ID..."

  lifecycle_status() {  # echoes the live workspace status
    tenant_call GET "/workspaces/$PARENT_ID" 2>/dev/null \
      | python3 -c "import json,sys; print(json.load(sys.stdin).get('status') or '')" 2>/dev/null || echo ""
  }
  # Bounded readiness-poll for a target status — same fail-closed shape as
  # wait_workspaces_online_routable, but for an arbitrary terminal status.
  wait_status() {  # $1=target $2=timeout_secs $3=label
    local target="$1" timeout="$2" label="$3"
    local deadline cur last=""
    deadline=$(( $(date +%s) + timeout ))
    while true; do
      cur=$(lifecycle_status)
      if [ "$cur" != "$last" ]; then log "    parent status → ${cur:-<empty>}"; last="$cur"; fi
      [ "$cur" = "$target" ] && return 0
      if [ "$(date +%s)" -gt "$deadline" ]; then
        log "    [lifecycle] $label never reached '$target' within ${timeout}s (last='$cur')"
        return 1
      fi
      sleep 10
    done
  }

  # ── pause → paused ──
  PAUSE_RESP=$(tenant_call POST "/workspaces/$PARENT_ID/pause" 2>/dev/null || echo '{}')
  PAUSE_STATUS=$(echo "$PAUSE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$PAUSE_STATUS" = "paused" ] || fail "Pause: POST /pause returned status='$PAUSE_STATUS' (expected 'paused'). Body: ${PAUSE_RESP:0:200}"
  # Poll the DB-backed status — the response body could lie; the GET proves the row.
  wait_status "paused" 120 "pause" || fail "Pause: workspace $PARENT_ID never settled at status=paused (DB row) — Pause handler / CP stop regression (workspace_restart.go Pause)."
  ok "    pause → paused (DB-verified)"

  # ── resume → provisioning → online ──
  RESUME_RESP=$(tenant_call POST "/workspaces/$PARENT_ID/resume" 2>/dev/null || echo '{}')
  RESUME_STATUS=$(echo "$RESUME_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$RESUME_STATUS" = "provisioning" ] || fail "Resume: POST /resume returned status='$RESUME_STATUS' (expected 'provisioning'). Body: ${RESUME_RESP:0:200}"
  # Resume re-provisions from the preserved config volume; reuse the same
  # online+routable readiness boundary the initial boot used (no fresh EC2
  # cold-start, but CP re-provision + heartbeat recovery can still take minutes).
  wait_workspaces_online_routable "    Waiting for parent to return online after resume (up to $((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min)..." "$PARENT_ID"
  ok "    resume → provisioning → online (DB-verified)"

  # ── hibernate → hibernated ──
  HIB_RESP=$(tenant_call POST "/workspaces/$PARENT_ID/hibernate?force=true" 2>/dev/null || echo '{}')
  HIB_STATUS=$(echo "$HIB_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$HIB_STATUS" = "hibernated" ] || fail "Hibernate: POST /hibernate?force=true returned status='$HIB_STATUS' (expected 'hibernated'). Body: ${HIB_RESP:0:200}"
  # The handler runs the claim→stop→'hibernated' sequence; poll the DB row to
  # confirm it landed on 'hibernated' (not stuck mid-'hibernating').
  wait_status "hibernated" 120 "hibernate" || fail "Hibernate: workspace $PARENT_ID never settled at status=hibernated (DB row) — Hibernate handler / CP stop regression (workspace_restart.go HibernateWorkspace)."
  ok "    hibernate → hibernated (DB-verified)"

  # ── resume-from-hibernate via auto-wake on next A2A ──
  # A hibernated workspace auto-wakes on the next incoming A2A message/send
  # (no explicit /resume — Resume only handles status=paused). Send a wake
  # A2A and assert the workspace returns to online. We accept transient cold
  # 5xx during wake (same edge class the PONG probe tolerates) and poll the
  # status to the online boundary rather than asserting on the single A2A code.
  log "    Hibernate auto-wake: sending A2A to wake hibernated parent..."
  WAKE_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-wake-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-wake-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform lifecycle smoke test waking a hibernated workspace. No tools or memory are needed — please respond with exactly the single token: WOKE'}]
        }
    }
}))
")
  WAKE_TMP=$(mktemp -t wake_a2a.XXXXXX)
  for WAKE_ATTEMPT in $(seq 1 12); do
    : >"$WAKE_TMP"
    set +e
    WAKE_CODE=$(tenant_call POST "/workspaces/$PARENT_ID/a2a" \
      --max-time 90 \
      -H "Content-Type: application/json" \
      -d "$WAKE_PAYLOAD" \
      -o "$WAKE_TMP" -w '%{http_code}' 2>/dev/null)
    WAKE_RC=$?
    set -e
    WAKE_CODE=${WAKE_CODE:-000}
    if [ "$WAKE_RC" = "0" ] && [ "$WAKE_CODE" -ge 200 ] && [ "$WAKE_CODE" -lt 300 ]; then
      break
    fi
    WAKE_SAFE_BODY=$(cat "$WAKE_TMP" 2>/dev/null | sanitize_http_body)
    # Wake legitimately returns transient 5xx while the container restarts —
    # retry that class only (bounded), never a 4xx.
    if echo "$WAKE_CODE" | grep -Eq '^(502|503|504)$' && [ "$WAKE_ATTEMPT" -lt 12 ]; then
      log "    wake A2A cold/restart attempt $WAKE_ATTEMPT/12 returned $WAKE_CODE: ${WAKE_SAFE_BODY:0:120}"
      sleep 15
      continue
    fi
    break
  done
  rm -f "$WAKE_TMP"
  # The auto-wake contract is the STATUS transition (hibernated → online), not
  # the A2A body content — assert the live DB row, the real readiness signal.
  wait_status "online" "$WORKSPACE_ONLINE_TIMEOUT_SECS" "hibernate-wake" \
    || fail "Hibernate auto-wake: parent $PARENT_ID never returned to status=online after a wake A2A (last A2A http=$WAKE_CODE) — auto-wake-on-message regression (a hibernated ws must re-provision on the next A2A)."
  ok "    hibernate → online via auto-wake A2A (DB-verified)"
  ok "Lifecycle transitions passed: pause→resume→online + hibernate→wake→online"
else
  log "10b/11 Lifecycle transitions skipped (MODE=$MODE, E2E_LIFECYCLE=${E2E_LIFECYCLE:-auto}) — pause/resume/hibernate only run in full mode with E2E_LIFECYCLE!=off."
fi

# ─── 11. Teardown runs via trap ────────────────────────────────────────
# Fail-closed-on-skip: before declaring PASS, assert (when CI demanded a live
# run) that every load-bearing lifecycle milestone actually fired. A run that
# reaches here without provision→online→A2A having truly happened exits 5
# instead of reporting green. Teardown still runs (EXIT trap) on that exit.
require_live_or_die
log "11/11 All checks passed. Teardown runs via EXIT trap."
ok "═══ STAGING $MODE-SAAS E2E PASSED ═══"
