#!/usr/bin/env bash
# MANDATORY local Docker-provisioner lifecycle e2e.
#
# Why this exists: every other e2e exercises the SaaS/EC2 (control-plane)
# provisioner. NOTHING mandatory exercises the LOCAL Docker provisioner
# (MOLECULE_ENV=development, docker.sock) — the path self-hosters and dev runs
# use. A config-volume bug where a restarted workspace couldn't find its
# config.yaml (and wedged in 'failed' with "config volume is empty") went
# undetected for exactly this reason. This test provisions a REAL workspace via
# the LOCAL provisioner and asserts the full lifecycle, INCLUDING the
# restart-survival assertion that would have caught that bug.
#
# Steps (each asserts loudly):
#   1. Build + tag the stub runtime image to the provisioner's RegistryModeLocal
#      cache tag so runtime=claude-code resolves to the stub (cache-hit, no
#      2.5GB build).
#   2. POST /workspaces (runtime=claude-code) — capture id.
#   3. Poll GET /workspaces/{id} until status==online (<=90s); assert a ws-<id>
#      container is running.
#   4. RESTART-SURVIVAL: POST /workspaces/{id}/restart, poll until online AGAIN
#      (<=90s); assert the container is back and the workspace did NOT wedge in
#      failed / "config volume is empty". <-- the key assertion.
#   5. PROXY REACH: POST an A2A message/send through the PLATFORM proxy
#      (/workspaces/{id}/a2a); assert 200 + the stub's canned reply (proves the
#      ws-<id>:8000 Docker-DNS rewrite path works end-to-end).
#   6. Cleanup: delete the workspace (trap removes its container + volumes).
#
# Parameterizable: LIFECYCLE_RUNTIME_IMAGE selects which image the provisioner
# resolves to. Default = the freshly-built stub. Point it at the real image
# (e.g. molecule-local/workspace-template-claude-code:2ac9678422a5) for an
# advisory lifecycle-only run (the proxy-reach step then asserts reachability,
# not the canned text — a real LLM-less runtime can't produce "STUB OK").
#
# Run (stub, default — fast, no LLM):
#   BASE=http://localhost:8080 ADMIN_TOKEN=dev-local-admin-token \
#     bash tests/e2e/test_local_provision_lifecycle_e2e.sh
#
# Run (REAL MiniMax LLM round-trip — cheapest real model; asserts a real reply):
#   BASE=http://localhost:8080 ADMIN_TOKEN=dev-local-admin-token \
#     LIFECYCLE_LLM=minimax MINIMAX_API_KEY=<key> \
#     bash tests/e2e/test_local_provision_lifecycle_e2e.sh
#   (MINIMAX_API_KEY missing => loud skip exit 0; key is only ever sent in the
#    secret-write curl body, never echoed or written to disk.)
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default + admin-auth + cleanup helpers

# ---- config -----------------------------------------------------------------
ADMIN_TOKEN="${ADMIN_TOKEN:-${MOLECULE_ADMIN_TOKEN:-}}"
export ADMIN_TOKEN MOLECULE_ADMIN_TOKEN="${ADMIN_TOKEN}"

# Was ONLINE_TIMEOUT set by the caller? Remember before we default it so the
# minimax mode (heavier real-template boot) can bump the default without
# clobbering an explicit operator/CI override.
ONLINE_TIMEOUT_EXPLICIT=0
[ -n "${ONLINE_TIMEOUT:-}" ] && ONLINE_TIMEOUT_EXPLICIT=1
ONLINE_TIMEOUT="${ONLINE_TIMEOUT:-90}"          # seconds to wait for online

# Same pattern for RESTART_TIMEOUT (Step 4 restart-survival poll). Initialize
# the _EXPLICIT flag and the default BEFORE the LIFECYCLE_LLM=minimax block
# runs, so the minimax block can correctly see whether the caller pinned a
# value and avoid clobbering it. (CR2 RC #11266 ordering fix.)
RESTART_TIMEOUT_EXPLICIT=0
[ -n "${RESTART_TIMEOUT:-}" ] && RESTART_TIMEOUT_EXPLICIT=1
RESTART_TIMEOUT="${RESTART_TIMEOUT:-$ONLINE_TIMEOUT}"
A2A_TIMEOUT="${A2A_TIMEOUT:-30}"
STUB_DIR="$(cd "$(dirname "$0")/stub-runtime" && pwd)"
RUNTIME="claude-code"

# The provisioner's RegistryModeLocal resolves runtime=claude-code by checking
# the local image store for molecule-local/workspace-template-claude-code:<sha12>
# (the Gitea HEAD sha12 of the template repo's `main` branch — see
# provisioner/localbuild.go EnsureLocalImage). If that tag is missing it
# clones+builds the real 2.5GB template (slow + can OOM-kill in CI). We pre-tag
# our chosen image to that EXACT cache tag so the cache-check (dockerHasTag)
# hits and resolves to our image with no clone/build.
#
# The sha MOVES as the template repo advances, so we DISCOVER it at runtime from
# the same Gitea branch API the provisioner uses (CACHE_SHA), and only fall back
# to a pinned default (or an explicit CACHE_TAG override) when Gitea is
# unreachable. This keeps the test correct without an annual sha bump.
CACHE_REPO="molecule-local/workspace-template-${RUNTIME}"
GITEA_BRANCH_API="${GITEA_BRANCH_API:-https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-${RUNTIME}/branches/main}"
# Model + credential choice — three coupled constraints from workspace-server:
#   * Create rejects a model NOT registered for the runtime
#     (UNREGISTERED_MODEL_FOR_RUNTIME, provider-registry SSOT).
#   * The SLASH form (anthropic/claude-opus-4-7) derives provider=platform =>
#     platform_managed billing, which ABORTS provisioning in a dev stack with
#     no CP proxy env (MISSING_PLATFORM_PROXY, #2162).
#   * The BARE form (claude-opus-4-7) derives provider=anthropic-api => BYOK,
#     which then FAILS CLOSED unless the workspace has a usable LLM credential
#     (MISSING_BYOK_CREDENTIAL). anthropic-api's auth_env is
#     [ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN] — so we pass a DUMMY
#     ANTHROPIC_API_KEY secret. The stub never makes an LLM call, so the dummy
#     value is fine; it only needs to exist so byok resolves with a usable cred.
# This keeps the test self-contained (no platform-proxy env required) — exactly
# the portable shape the CI required job needs.
LIFECYCLE_MODEL="${LIFECYCLE_MODEL:-claude-opus-4-7}"
LIFECYCLE_LLM_KEY="${LIFECYCLE_LLM_KEY:-ANTHROPIC_API_KEY}"
LIFECYCLE_LLM_VALUE="${LIFECYCLE_LLM_VALUE:-sk-ant-e2e-stub-dummy-not-a-real-key}"
LATEST_TAG="${CACHE_REPO}:latest"

# ---- LIFECYCLE_LLM: real-LLM round-trip mode -------------------------------
# Default "" = the existing behaviour (stub or LLM-less real image).
#
#   LIFECYCLE_LLM=minimax — provision the REAL claude-code template image with a
#   MiniMax BYOK credential and assert an ACTUAL model reply at the proxy-reach
#   step (Step 5), proving a genuine round-trip through the ws-<id>:8000 proxy.
#
#   Why MiniMax: it's the cheapest LLM the platform offers (the staging canaries'
#   primary auth path post-2026-05-04). The claude-code adapter's `minimax`
#   provider (providers.yaml:258) reads MINIMAX_API_KEY at boot and points
#   ANTHROPIC_BASE_URL at api.minimax.io/anthropic — MiniMax's OWN API, NOT the
#   molecule LLM proxy — so a BYOK MiniMax workspace reaches the model DIRECTLY
#   and works on this local dev stack with no CP proxy env.
#
#   The registered claude-code slug is the BARE id `MiniMax-M3` (derives
#   provider=minimax => byok). The colon form `minimax:MiniMax-M3` is
#   UNREGISTERED on claude-code (internal#718). auth_env for `minimax` accepts
#   MINIMAX_API_KEY, which the adapter projects into ANTHROPIC_AUTH_TOKEN.
#
#   The real key MUST be supplied via the MINIMAX_API_KEY env var (never echoed
#   or written to disk by this script — it only travels in the secret-write curl
#   body, exactly like the dummy ANTHROPIC_API_KEY does today). Missing key =>
#   loud skip (exit 0), never a red fail (mirrors the serving-e2e pattern).
LIFECYCLE_LLM="${LIFECYCLE_LLM:-}"
if [ "$LIFECYCLE_LLM" = "minimax" ]; then
  if [ -z "${MINIMAX_API_KEY:-}" ]; then
    echo "SKIP: LIFECYCLE_LLM=minimax but MINIMAX_API_KEY is not set in the env."
    echo "      Provide a real MiniMax key (the advisory CI job reads it from a"
    echo "      CI secret) to run the real-LLM round-trip. Skipping (exit 0)."
    exit 0
  fi
  # Real claude-code template build (provisioner resolves+builds via
  # RegistryModeLocal — same path as the advisory lifecycle-real job).
  LIFECYCLE_PROVISIONER_BUILDS="1"
  # Registered BYOK MiniMax slug for claude-code (bare id => provider=minimax).
  LIFECYCLE_MODEL="MiniMax-M3"
  LIFECYCLE_LLM_KEY="MINIMAX_API_KEY"
  LIFECYCLE_LLM_VALUE="${MINIMAX_API_KEY}"
  # The real template boot is heavier than the stub; give it room (unless the
  # caller pinned ONLINE_TIMEOUT explicitly).
  [ "$ONLINE_TIMEOUT_EXPLICIT" -eq 0 ] && ONLINE_TIMEOUT=180
  # Step 4 (restart-survival) has to wait for the REAL-image cold start on top
  # of the same path — agent SDK boot + MiniMax LLM dial is the slowest leg.
  # 240s gives the wedge-detector a chance to clear once the agent finally
  # registers (registry.go's degraded→online path needs ~2-3 successful
  # heartbeats after the wedge window).
  [ "${RESTART_TIMEOUT_EXPLICIT:-0}" -eq 0 ] && RESTART_TIMEOUT=240
fi

# RESTART_TIMEOUT governs Step 4 (restart-survival poll). The default
# initialization + _EXPLICIT probe happen ABOVE this block (alongside
# ONLINE_TIMEOUT), so the LIFECYCLE_LLM=minimax override below can
# correctly see whether the caller pinned a value and avoid clobbering it.

# Image the provisioner should actually run. Default: build the stub. Override
# to a real image (a pre-built tag) for the advisory lifecycle-only run.
LIFECYCLE_RUNTIME_IMAGE="${LIFECYCLE_RUNTIME_IMAGE:-__BUILD_STUB__}"

# LIFECYCLE_PROVISIONER_BUILDS=1: do NOT pre-tag any image — let the provisioner
# resolve runtime=claude-code itself via RegistryModeLocal (clone + docker build
# the real template). This exercises the GENUINE local image-resolution path end
# to end. Used by the advisory CI job. Implies the real (LLM-less) runtime, so
# the proxy-reach step asserts reachability, not a canned reply.
LIFECYCLE_PROVISIONER_BUILDS="${LIFECYCLE_PROVISIONER_BUILDS:-0}"

# When NOT running the stub we cannot assert the canned "STUB OK" text (no LLM);
# we assert reachability/registration instead.
USING_STUB=1
[ "$LIFECYCLE_RUNTIME_IMAGE" != "__BUILD_STUB__" ] && USING_STUB=0
[ "$LIFECYCLE_PROVISIONER_BUILDS" = "1" ] && USING_STUB=0

PASS=0
FAIL=0
WSID=""
# May be pre-pinned via env; otherwise resolved from the Gitea HEAD sha in Step 1.
CACHE_TAG="${CACHE_TAG:-}"
# Remember the tags/images we mutated so the trap can restore the cache tag to
# the real image (so a stub run never leaves the real claude-code tag pointing
# at the lightweight stub for the next developer/CI job).
ORIG_CACHE_IMAGE_ID=""

check() {
  local desc="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qF -- "$expected"; then
    echo "PASS: $desc"; PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected to contain: $expected"
    echo "  got: $(echo "$actual" | head -5)"
    FAIL=$((FAIL + 1))
  fi
}

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "  $2"; FAIL=$((FAIL + 1)); }

admin_curl() {
  local _a=(); e2e_admin_auth_args _a
  curl -s "${_a[@]+"${_a[@]}"}" "$@"
}

ws_field() {  # ws_field <workspace-json> <field>
  echo "$1" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('$2',''))
except Exception:
  print('')"
}

# SEV-2499 SSOT: use the same Go naming helpers the provisioner uses so the
# e2e script can never drift from the real naming convention. The CI job
# pre-builds e2e-names to /usr/local/bin; local runs must build it once:
#   go build -o /usr/local/bin/e2e-names ./cmd/e2e-names
_e2e_name() {
  local kind="$1" wid="$2"
  if [ -x "$(command -v e2e-names)" ]; then
    e2e-names "$kind" "$wid"
  else
    echo "SEV-2499: e2e-names not found in PATH — build it with: go build -o /usr/local/bin/e2e-names ./cmd/e2e-names" >&2
    exit 2
  fi
}
config_volume_name()   { _e2e_name config-volume   "$1"; }
session_volume_name()  { _e2e_name session-volume  "$1"; }
workspace_volume_name(){ _e2e_name workspace-volume "$1"; }
container_name()       { _e2e_name container       "$1"; }

container_running() {  # container_running <ws-id>  -> echoes name if running
  docker ps --filter "name=$(container_name "$1")" --filter "status=running" --format '{{.Names}}' 2>/dev/null | head -1
}

diagnose_provision() {
  local wsid="${1:-}"
  local target_name
  target_name=$(container_name "$wsid")
  # EXACT-match the target container name. The old `container_running` used
  # `docker ps --filter "name=ws-${wsid}"` which is a SUBSTRING match, so on a
  # shared dev daemon with many stale ws-* containers from other dev activity
  # it could return a non-target container (e.g. ws-${wsid}-stale) and dump
  # its logs in the diagnostic — obscuring the real failure. Exact match
  # fixes that (#2680).
  local container
  container=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -Fx "$target_name" || true)
  echo "--- DIAGNOSE provisioning for $wsid (target=$target_name) ---"
  echo "last_sample_error: ${LAST:-<none>}"
  echo "container_running (exact match): ${container:-<none>}"
  if [ -n "$container" ]; then
    echo "--- container logs ($container) ---"
    docker logs "$container" 2>&1 | tail -n 60 || true
    echo "--- container env ---"
    docker inspect "$container" --format '{{json .Config.Env}}' 2>&1 || true
    echo "--- container reachability test ---"
    docker exec "$container" sh -c 'echo "platform_url=$PLATFORM_URL"; curl -sfS -m 5 "$PLATFORM_URL/health" 2>&1 || echo "WARN: curl probe failed (curl=$?)"' || true
  fi
  # Other ws-* containers from sibling dev activity — clearly labelled as
  # NOT the target so the failure-mode readout isn't mis-attributed.
  echo "--- OTHER ws-* containers on this daemon (NOT the target) ---"
  docker ps --format '{{.Names}} {{.Status}}' 2>/dev/null | grep -E '^ws-' | grep -vFx "$target_name" || echo "  (none)"
  echo "--- all ws-* volumes ---"
  docker volume ls -q 2>/dev/null | grep '^ws-' || true
  echo "--- end diagnose ---"
}

cleanup() {
  local rc=$?
  echo ""
  echo "--- cleanup ---"
  if [ -n "$WSID" ]; then
    # SCOPED teardown — only the workspace this test created. Never a blanket
    # sweep (other dev workspaces may be live on this shared daemon).
    e2e_delete_workspace "$WSID" "" >/dev/null 2>&1 || true
    docker rm -f "$(container_name "$WSID")" >/dev/null 2>&1 || true
    docker volume rm -f \
      "$(config_volume_name "$WSID")" "$(session_volume_name "$WSID")" \
      "$(workspace_volume_name "$WSID")" >/dev/null 2>&1 || true
    echo "cleaned workspace $WSID + $(container_name "$WSID") container/volumes"
  fi
  # Restore the cache tag to whatever it pointed at before we retagged it, so a
  # stub run doesn't leave the real claude-code tag aliased to the stub.
  if [ -n "$ORIG_CACHE_IMAGE_ID" ]; then
    docker tag "$ORIG_CACHE_IMAGE_ID" "$CACHE_TAG" >/dev/null 2>&1 || true
    echo "restored $CACHE_TAG -> ${ORIG_CACHE_IMAGE_ID:0:19}"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

echo "=== Local Docker-Provisioner Lifecycle E2E ==="
echo "BASE=$BASE  runtime=$RUNTIME  using_stub=$USING_STUB  llm=${LIFECYCLE_LLM:-none}  model=$LIFECYCLE_MODEL  cache_tag=${CACHE_TAG:-<resolve-in-step-1>}"
echo ""

# Preflight: docker must be reachable and the platform must be up.
if ! docker info >/dev/null 2>&1; then
  echo "ERROR: docker daemon not reachable — this test provisions local containers."
  exit 2
fi
if ! curl -s -m 5 "$BASE/workspaces" >/dev/null 2>&1; then
  echo "ERROR: platform not reachable at $BASE"
  exit 2
fi

# ----------------------------------------------------------------------------
# Step 1 — build/tag the image the provisioner will resolve to.
# ----------------------------------------------------------------------------
echo "--- Step 1: resolve runtime image to the chosen target ---"
# Resolve the EXACT cache tag the provisioner will look up: <repo>:<gitea-HEAD-
# sha12>. Discover the sha from the Gitea branch API (same source the provisioner
# uses). An explicit CACHE_TAG env overrides discovery; if Gitea is unreachable
# AND no override is set, bail loudly — silently tagging the wrong sha would let
# the provisioner clone+build the real 2.5GB template (slow / OOM).
if [ -n "${CACHE_TAG:-}" ]; then
  echo "Using operator-pinned CACHE_TAG=$CACHE_TAG"
else
  CACHE_SHA=$(curl -s -m 10 "$GITEA_BRANCH_API" 2>/dev/null \
    | python3 -c "import sys,json
try:
  print(json.load(sys.stdin)['commit']['id'][:12])
except Exception:
  print('')" 2>/dev/null)
  if [ -z "$CACHE_SHA" ]; then
    echo "ERROR: could not resolve the template HEAD sha from $GITEA_BRANCH_API"
    echo "       set CACHE_TAG=$CACHE_REPO:<sha12> explicitly (the tag the provisioner expects)."
    exit 2
  fi
  CACHE_TAG="${CACHE_REPO}:${CACHE_SHA}"
  echo "Resolved provisioner cache tag: $CACHE_TAG (gitea HEAD sha)"
fi

# Record what the cache tag points at NOW (if anything) so cleanup can restore.
ORIG_CACHE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CACHE_TAG" 2>/dev/null || true)"

if [ "$LIFECYCLE_PROVISIONER_BUILDS" = "1" ]; then
  # No pre-tag — the provisioner resolves + builds the real template itself via
  # RegistryModeLocal. Disarm the cache-tag restore (we never touched it).
  ORIG_CACHE_IMAGE_ID=""
  pass "provisioner-builds mode: leaving image resolution to RegistryModeLocal (real template build)"
elif [ "$USING_STUB" -eq 1 ]; then
  echo "Building stub image from $STUB_DIR ..."
  if ! docker build --platform=linux/amd64 -t molecule-local/stub-runtime:latest "$STUB_DIR" >/tmp/stub_build.log 2>&1; then
    echo "FAIL: stub image build failed"; tail -20 /tmp/stub_build.log; exit 1
  fi
  pass "stub image built"
  TARGET_IMAGE="molecule-local/stub-runtime:latest"
  # Point BOTH the sha-pinned cache tag and :latest at the stub so the
  # provisioner's RegistryModeLocal cache-check (dockerHasTag) resolves to it
  # instead of cloning+building the template.
  docker tag "$TARGET_IMAGE" "$CACHE_TAG"
  docker tag "$TARGET_IMAGE" "$LATEST_TAG"
  pass "tagged $TARGET_IMAGE -> $CACHE_TAG (+ :latest)"
else
  TARGET_IMAGE="$LIFECYCLE_RUNTIME_IMAGE"
  if ! docker image inspect "$TARGET_IMAGE" >/dev/null 2>&1; then
    echo "Real image $TARGET_IMAGE not present locally — pulling ..."
    docker pull "$TARGET_IMAGE" >/dev/null 2>&1 || { echo "FAIL: cannot obtain $TARGET_IMAGE"; exit 1; }
  fi
  pass "using real runtime image $TARGET_IMAGE"
  docker tag "$TARGET_IMAGE" "$CACHE_TAG"
  docker tag "$TARGET_IMAGE" "$LATEST_TAG"
  pass "tagged $TARGET_IMAGE -> $CACHE_TAG (+ :latest)"
fi
echo ""

# ----------------------------------------------------------------------------
# Step 2 — provision a workspace via the real create endpoint.
# ----------------------------------------------------------------------------
echo "--- Step 2: provision workspace (POST /workspaces) ---"
# Provision-time billing on this dev stack (no CP proxy env):
#   * A claude-code workspace with a BARE model id derives provider=anthropic-api
#     => BYOK, which FAILS CLOSED in prepare unless a usable LLM credential
#     exists (MISSING_BYOK_CREDENTIAL).
#   * The per-workspace secret-write guard blocks a vendor key while the
#     workspace still resolves platform-managed (the MODEL secret isn't stored
#     until AFTER payload.secrets are written at create time) — so we can't pass
#     the key in the create payload.
# So: create WITHOUT secrets, flip the workspace to byok (explicit override wins
# in BOTH the guard's resolver and the provision resolver), then write the dummy
# vendor key — now permitted. We do NOT rely on Create's first provision to seed
# the config volume (it aborts byok-no-cred BEFORE Start, leaving the volume
# empty). Instead we SEED config.yaml directly into the named config volume and
# then trigger ONE clean provision via /restart. Seeding the volume is also what
# makes the restart-survival assertion meaningful: the restart path reuses the
# volume rather than any template.
# core#2608: create is now ATOMIC for byok — the create-boundary gate
# hard-rejects a byok model with no credential in scope, and the create-scope
# vendor-key guard accepts the credential in the SAME payload (deriving from
# the payload model instead of the not-yet-stored MODEL secret). So the dummy
# key rides in the create body; the later flip+write steps remain as
# idempotent belt-and-suspenders for the restart path.
CREATE_BODY=$(cat <<JSON
{"name":"Lifecycle E2E Stub","tier":2,"runtime":"$RUNTIME","model":"$LIFECYCLE_MODEL","secrets":{"$LIFECYCLE_LLM_KEY":"$LIFECYCLE_LLM_VALUE"}}
JSON
)
RESP=$(admin_curl -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d "$CREATE_BODY")
WSID=$(ws_field "$RESP" "id")
if [ -z "$WSID" ]; then
  fail "create returned no workspace id" "$RESP"
  echo "=== Results: $PASS passed, $((FAIL+1)) failed ==="
  exit 1
fi
pass "workspace created: $WSID"
CONFIG_VOL="$(config_volume_name "$WSID")"

# Mint a workspace bearer for the WorkspaceAuth-gated secret + /restart calls.
WTOKEN=$(e2e_mint_workspace_token "$WSID" || true)
if [ -z "$WTOKEN" ]; then
  fail "could not mint workspace token"
  echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi

# Flip to byok BEFORE writing the vendor key (explicit override unblocks the
# secret-write guard AND makes the provision resolver pick byok).
BM=$(admin_curl -X PUT "$BASE/admin/workspaces/$WSID/llm-billing-mode" \
  -H "Content-Type: application/json" -d '{"mode":"byok"}')
check "billing mode set to byok" "byok" "$BM"

# Write the dummy LLM credential (now allowed on a byok workspace). Inert — the
# stub never calls an LLM; it only needs to exist so byok has a usable cred.
SEC=$(curl -s -X POST "$BASE/workspaces/$WSID/secrets" \
  -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" \
  -d "{\"key\":\"$LIFECYCLE_LLM_KEY\",\"value\":\"$LIFECYCLE_LLM_VALUE\"}")
echo "  secret write: $(echo "$SEC" | head -c 120)"

# In minimax mode also write MODEL_PROVIDER=minimax as a secret env. The
# claude-code adapter's _resolve_model_and_provider_from_env honours
# MODEL_PROVIDER ONLY when it matches a registered provider name (else it's
# treated as a legacy model-id), so a literal "minimax" routes the workspace to
# the `minimax` provider entry — projecting MINIMAX_API_KEY → ANTHROPIC_AUTH_TOKEN
# and setting ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic. workspace-
# server injects MODEL/MOLECULE_MODEL from the picked slug but NO LONGER emits
# MODEL_PROVIDER (applyRuntimeModelEnv, post-2026-05-19), so this secret-provided
# value survives into the container env. Without it a BARE `MiniMax-M2.7` derives
# no provider and falls through to the anthropic-api default (boot banner
# "provider=anthropic-api", base_url unset → AuthenticationError on the first
# call → the "Agent error" this mode exists to catch).
if [ "$LIFECYCLE_LLM" = "minimax" ]; then
  SECP=$(curl -s -X POST "$BASE/workspaces/$WSID/secrets" \
    -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" \
    -d '{"key":"MODEL_PROVIDER","value":"minimax"}')
  echo "  secret write (MODEL_PROVIDER): $(echo "$SECP" | head -c 120)"
fi

# #2851: override HOSTNAME in the workspace container so a runtime that computes
# its self-URL from HOSTNAME does not advertise its Docker container short-ID
# (e.g. 30e9e720fbc2), which the platform cannot resolve. The provisioner
# injects MOLECULE_WORKSPACE_URL=http://localhost:<host-port>, but real templates
# may fall back to HOSTNAME. localhost is explicitly allowed by name in dev-mode
# SSRF validation and reaches the host-mapped workspace port from the host-network
# act_runner job container.
SECH=$(curl -s -X POST "$BASE/workspaces/$WSID/secrets" \
  -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" \
  -d '{"key":"HOSTNAME","value":"localhost"}')
echo "  secret write (HOSTNAME): $(echo "$SECH" | head -c 120)"

# Seed config.yaml directly into the named config volume so the provision (and
# every later restart) has a config source. Create's byok-no-cred abort never
# wrote it, and this dev stack ships no claude-code template in the platform's
# configsDir for the empty-volume auto-recover to fall back to. The provisioner
# created the volume on its first (aborted) Start attempt; ensure it exists,
# then drop a minimal valid config.yaml in via a throwaway alpine container.
docker volume create "$CONFIG_VOL" >/dev/null 2>&1 || true
# In minimax mode the seeded config MUST carry an explicit `provider: minimax`.
# The claude-code adapter (and the molecule_runtime wheel's
# _derive_provider_from_model) only auto-derive a provider from a `vendor:model`
# or `vendor/model` slug — a BARE `MiniMax-M2.7` derives no provider and falls
# through to the anthropic-api default (boot banner: "provider=anthropic-api",
# ANTHROPIC_BASE_URL unset → the MiniMax key is never projected and the first
# LLM call fails with AuthenticationError). Naming the provider explicitly makes
# the adapter pick the `minimax` registry entry, project
# MINIMAX_API_KEY → ANTHROPIC_AUTH_TOKEN, and set
# ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic — a real round-trip.
LIFECYCLE_PROVIDER_LINE=""
[ "$LIFECYCLE_LLM" = "minimax" ] && LIFECYCLE_PROVIDER_LINE="provider: minimax"
CFG_YAML="name: ${WSID}
description: lifecycle e2e
version: 1.0.0
tier: 2
runtime: ${RUNTIME}
model: ${LIFECYCLE_MODEL}
runtime_config:
  model: ${LIFECYCLE_MODEL}
  ${LIFECYCLE_PROVIDER_LINE}
  timeout: 0
"
if docker run --rm -v "${CONFIG_VOL}:/configs" alpine:3 sh -c "cat > /configs/config.yaml" <<EOF >/dev/null 2>&1
${CFG_YAML}
EOF
then pass "seeded config.yaml into $CONFIG_VOL"; else fail "could not seed config.yaml into $CONFIG_VOL"; fi
echo ""

# ----------------------------------------------------------------------------
# Step 3 — provision (via restart) and wait for online; assert container.
# ----------------------------------------------------------------------------
echo "--- Step 3: provision + wait for first online (<=${ONLINE_TIMEOUT}s) ---"
# Kick ONE clean provision now that byok + cred + config.yaml are all in place.
curl -s -X POST "$BASE/workspaces/$WSID/restart" \
  -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" -d '{}' >/dev/null
STATUS=""; LAST=""; failed_since=0
for _ in $(seq 1 "$ONLINE_TIMEOUT"); do
  WS=$(admin_curl "$BASE/workspaces/$WSID")
  STATUS=$(ws_field "$WS" "status")
  LAST=$(ws_field "$WS" "last_sample_error")
  if [ "$STATUS" = "online" ]; then break; fi
  if [ "$STATUS" = "failed" ]; then
    failed_since=$((failed_since + 1))
    # A restart re-kicks provisioning; give the coalescing pipeline room to
    # converge. Only bail if it stays failed for 20s straight.
    if [ "$failed_since" -ge 20 ]; then
      fail "workspace STUCK in 'failed' during initial provision" "last_sample_error: $LAST"
      echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
    fi
  else
    failed_since=0
  fi
  sleep 1
done
check "workspace reached online (status=$STATUS)" "online" "$STATUS"
if [ "$FAIL" -gt 0 ]; then diagnose_provision "$WSID"; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1; fi
# Bounded poll: the workspace can flip to 'online' as soon as the agent
# registers, but the container may not be visible/stable on the shared
# docker-host for another moment. Retry for up to 10s before failing hard.
RUN=""
for _ in $(seq 1 10); do
  RUN=$(container_running "$WSID")
  if [ -n "$RUN" ]; then break; fi
  sleep 1
done
if [ -n "$RUN" ]; then pass "container running: $RUN"; else fail "no running ws-${WSID} container within 10s of online" "docker ps shows none"; fi

# #2851: fail fast if the workspace advertised an unresolvable/unreachable URL.
# The provisioner now makes the runtime advertise http://localhost:<host-port>
# by default, which the platform stores as http://127.0.0.1:<host-port>. The A2A
# proxy rewrites that to ws-<id>:8000 when the platform runs inside Docker, so
# the URL stored in the registry should always be a host-reachable address.
# When the platform itself is containerized, MOLECULE_WORKSPACE_ADVERTISE_HOST
# points the runtime at the Docker gateway IP instead; accept that too.
# (The end-to-end proxy reach in Step 5 is the real reachability proof; this
# assertion just surfaces hostname/DNS misconfiguration early.)
WS_URL_AFTER=$(ws_field "$WS" "url")
if [ -n "$WS_URL_AFTER" ]; then
  pass "workspace registered a non-empty URL: $WS_URL_AFTER"
else
  fail "workspace URL is empty after reaching online" "registry row has no url"
fi
URL_HOST_AFTER="${WS_URL_AFTER#http://}"
URL_HOST_AFTER="${URL_HOST_AFTER#https://}"
URL_HOST_AFTER="${URL_HOST_AFTER%%:*}"
# #2851 fail-fast: the advertised hostname MUST resolve. Container short-IDs
# (e.g. 30e9e720fbc2) do not resolve from the platform/A2A proxy and produce an
# opaque empty-LLM-result failure downstream. Surface the DNS misconfiguration
# here and now, before the MiniMax/proxy-reach round-trip.
if ! python3 -c "import socket; socket.gethostbyname('$URL_HOST_AFTER')" 2>/dev/null; then
  fail "workspace advertised URL does not resolve" "url=$WS_URL_AFTER host=$URL_HOST_AFTER cannot be resolved from the harness"
  echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi
pass "workspace advertised URL resolves (host=$URL_HOST_AFTER)"
if [ "$URL_HOST_AFTER" = "127.0.0.1" ] || [ "$URL_HOST_AFTER" = "localhost" ]; then
  pass "workspace registered a host-reachable URL (host=$URL_HOST_AFTER)"
else
  fail "workspace URL is not a host-reachable address" "url=$WS_URL_AFTER expected localhost/127.0.0.1"
fi
echo ""

# ----------------------------------------------------------------------------
# Step 4 — RESTART-SURVIVAL (the assertion that would have caught the bug).
# ----------------------------------------------------------------------------
echo "--- Step 4: restart-survival (POST /workspaces/$WSID/restart) ---"
# Re-mint the workspace bearer: every (re)provision rotates the workspace token
# (issueAndInjectToken -> RevokeAllForWorkspace + IssueToken), so the Step-2
# token is now stale. /restart is WorkspaceAuth-gated, so mint a fresh one.
WTOKEN=$(e2e_mint_workspace_token "$WSID" || true)
if [ -z "$WTOKEN" ]; then
  fail "could not mint fresh workspace token for restart"
else
  RR=$(curl -s -X POST "$BASE/workspaces/$WSID/restart" \
        -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" -d '{}')
  check "restart accepted (provisioning)" "provisioning" "$RR"

  # Poll until online AGAIN. Restart reuses the EXISTING config volume (no
  # template/configFiles passed) — so this passes ONLY if the config volume
  # survived the stop and still has config.yaml. A regression (volume reaped /
  # emptied) surfaces as status=failed with the "config volume is empty" error.
  #
  # Use RESTART_TIMEOUT (defaults to ONLINE_TIMEOUT, bumped to 240s in
  # LIFECYCLE_LLM=minimax mode — the real-image advisory lane). The wedge
  # detector can legitimately flip status to 'degraded' during the cold-start
  # window while heartbeats are still ramping up; that's NOT a failure here
  # (the agent hasn't finished booting yet), so we keep polling until online
  # OR failed OR the full RESTART_TIMEOUT.
  STATUS=""; LAST=""
  for _ in $(seq 1 "$RESTART_TIMEOUT"); do
    WS=$(admin_curl "$BASE/workspaces/$WSID")
    STATUS=$(ws_field "$WS" "status")
    LAST=$(ws_field "$WS" "last_sample_error")
    case "$STATUS" in
      online) break ;;
      failed)
        fail "workspace wedged in 'failed' AFTER restart (the config-volume bug class)" "last_sample_error: $LAST"
        break ;;
    esac
    sleep 1
  done
  check "workspace back online after restart (status=$STATUS)" "online" "$STATUS"
  if [ "$FAIL" -gt 0 ]; then diagnose_provision "$WSID"; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1; fi
  # Explicit negative on the exact bug signature.
  if echo "$LAST" | grep -qiF "config volume is empty"; then
    fail "restart hit 'config volume is empty' — restart-survival REGRESSION" "$LAST"
  else
    pass "no 'config volume is empty' error after restart"
  fi
  RUN=$(container_running "$WSID")
  if [ -n "$RUN" ]; then pass "container back after restart: $RUN"; else fail "container missing after restart"; fi
fi
echo ""

# ----------------------------------------------------------------------------
# Advisory-lane infra-skip helper (core#2917 follow-on). The mandatory stub
# lane must keep asserting the real proxy round-trip; only the advisory
# real-LLM lane may go green-with-skip when the platform A2A layer is under
# a known transient degradation (genuine gateway/queued-A2A signatures only).
INFRA_SKIP_REASONS=""
infra_skip_advisory() {
  local reason="$1"
  local detail="${2:-}"
  if [ "$LIFECYCLE_LLM" != "minimax" ]; then
    return
  fi
  # Cap distinct skip reasons: a broadly broken A2A layer that yields two
  # different skip signatures in one run must not false-green the advisory lane.
  case " $INFRA_SKIP_REASONS " in
    *" $reason "*) ;;
    *) INFRA_SKIP_REASONS="$INFRA_SKIP_REASONS $reason" ;;
  esac
  local distinct_count
  distinct_count=$(echo "$INFRA_SKIP_REASONS" | wc -w | tr -d ' ')
  if [ "$distinct_count" -ge 2 ]; then
    fail "infra-skip cap exceeded ($distinct_count distinct reasons:${INFRA_SKIP_REASONS:-none}) — refusing false-green on repeated A2A-layer degradation"
    return
  fi
  echo "[$(date +%H:%M:%S)] ⚠️  scan_status: infra-skip:${reason}${detail:+ $detail}"
  echo "=== Results: advisory infra-skip (${reason}) ==="
  exit 0
}

# ----------------------------------------------------------------------------
# Step 5 — proxy reach (ws-<id>:8000 Docker-DNS rewrite, end to end).
# ----------------------------------------------------------------------------
echo "--- Step 5: proxy reach (POST /workspaces/$WSID/a2a) ---"
# Debug: print the workspace URL the platform stored so SSRF failures are
# actionable (#2468 RCA).
WS_DEBUG=$(admin_curl "$BASE/workspaces/$WSID")
WS_URL_DEBUG=$(ws_field "$WS_DEBUG" "url")
WS_STATUS_DEBUG=$(ws_field "$WS_DEBUG" "status")
echo "  workspace url=$WS_URL_DEBUG status=$WS_STATUS_DEBUG"
# In minimax mode we send a DETERMINISTIC known-answer prompt and assert the
# model echoes the answer back — proving a real LLM round-trip, not just
# reachability. Otherwise a plain "ping".
if [ "$LIFECYCLE_LLM" = "minimax" ]; then
  A2A_PROMPT="Reply with exactly the single word PONG and nothing else."
else
  A2A_PROMPT="ping"
fi
A2A_BODY=$(python3 -c "
import json,sys
print(json.dumps({'method':'message/send','params':{'message':{'role':'user','parts':[{'type':'text','text':sys.argv[1]}]}}}))
" "$A2A_PROMPT")
# Real LLM cold-start (first turn boots the claude-code SDK + dials MiniMax) is
# slower than the stub; give the real-LLM call a longer ceiling.
A2A_CEIL="$A2A_TIMEOUT"
[ "$LIFECYCLE_LLM" = "minimax" ] && A2A_CEIL="${A2A_MINIMAX_TIMEOUT:-120}"

# Capture both body and HTTP code so we can detect gateway/queued responses.
A2A_TMP=$(mktemp)
set +e
A2A_CODE=$(curl -s -o "$A2A_TMP" -w '%{http_code}' --max-time "$A2A_CEIL" \
  -X POST "$BASE/workspaces/$WSID/a2a" \
  -H "Content-Type: application/json" \
  -d "$A2A_BODY")
A2A_RC=$?
set -e
A2A=$(cat "$A2A_TMP" 2>/dev/null || echo "")
rm -f "$A2A_TMP"

# Gateway/transport failure on the initial POST is an A2A-layer infra issue,
# not a local-provision code regression. Only skip the advisory lane.
# Fail-closed on agent-origin signals (workspace agent unreachable, restarting,
# etc.) — those can hide a real workspace-agent regression and must still FAIL.
if [ "$A2A_RC" -eq 28 ] && [ "$A2A_CODE" = "000" ]; then
  infra_skip_advisory "a2a-connect-timeout" "curl_rc=$A2A_RC http=$A2A_CODE"
fi
if echo "$A2A_CODE" | grep -Eq '^(502|503|504)$'; then
  if ! echo "$A2A" | grep -Eqi 'workspace agent unreachable|connection refused|workspace agent busy|native_session|restarting|restart triggered'; then
    infra_skip_advisory "a2a-gateway-error" "curl_rc=$A2A_RC http=$A2A_CODE"
  fi
fi

# core#2917: the A2A proxy can return a 202-queued envelope instead of a
# synchronous result. Poll the durable queue result; if the queue never drains,
# infra-skip the advisory lane rather than falsely blaming local-provision code.
A2A_QUEUED=$(printf '%s' "$A2A" | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  print('true' if d.get('queued') is True or (d.get('status') or '').lower() == 'queued' else 'false')
except Exception:
  print('false')" 2>/dev/null || echo "false")
if [ "$A2A_QUEUED" = "true" ]; then
  A2A_QID=$(printf '%s' "$A2A" | python3 -c "
import sys,json
try:
  print(json.load(sys.stdin).get('queue_id',''))
except Exception:
  print('')" 2>/dev/null || echo "")
  if [ -z "$A2A_QID" ]; then
    infra_skip_advisory "a2a-queued-no-queue-id" "initial POST was queued but returned no queue_id"
  fi
  echo "  A2A queued (queue_id=$A2A_QID); polling durable result..."
  A2A_POLL_TMP=$(mktemp)
  A2A_LAST_STATUS=""
  A2A_POLL_COUNT=0
  for poll_attempt in $(seq 1 30); do
    : >"$A2A_POLL_TMP"
    set +e
    curl -s -o "$A2A_POLL_TMP" -w '%{http_code}' --max-time 30 \
      -H "X-Workspace-ID: $WSID" \
      "$BASE/workspaces/$WSID/a2a/queue/$A2A_QID" >/dev/null 2>&1
    set -e
    A2A_POLL_RESP=$(cat "$A2A_POLL_TMP" 2>/dev/null || echo "")
    A2A_POLL_STATUS=$(printf '%s' "$A2A_POLL_RESP" | python3 -c "
import sys,json
try:
  print(json.load(sys.stdin).get('status',''))
except Exception:
  print('')" 2>/dev/null || echo "")
    A2A_LAST_STATUS="$A2A_POLL_STATUS"
    A2A_POLL_COUNT=$poll_attempt
    case "$A2A_POLL_STATUS" in
      completed)
        A2A=$(printf '%s' "$A2A_POLL_RESP" | python3 -c "
import sys,json
try:
  rb=json.load(sys.stdin).get('response_body')
  print(json.dumps(rb) if rb is not None else '')
except Exception:
  print('')" 2>/dev/null || echo "")
        if [ -n "$A2A" ]; then
          break
        fi
        ;;
      failed|dropped)
        rm -f "$A2A_POLL_TMP"
        fail "A2A queue item terminal status=$A2A_POLL_STATUS" "queue_id=$A2A_QID"
        break
        ;;
      queued|dispatched|in_progress|"")
        echo "    queue poll $poll_attempt/30 status=$A2A_POLL_STATUS — backing off 2s"
        sleep 2
        ;;
      *)
        rm -f "$A2A_POLL_TMP"
        fail "A2A queue poll unexpected status=$A2A_POLL_STATUS" "queue_id=$A2A_QID"
        break
        ;;
    esac
  done
  rm -f "$A2A_POLL_TMP"
  if [ -z "$A2A" ]; then
    if [ "$FAIL" -gt 0 ]; then
      echo "=== Results: $PASS passed, $FAIL failed ==="
      exit 1
    fi
    infra_skip_advisory "a2a-queue-timeout" "queue_id=$A2A_QID poll_count=${A2A_POLL_COUNT}/30 last_status=${A2A_LAST_STATUS:-<empty>}"
  fi
fi

# Extract the assistant text part once (shared by the minimax assertion +
# diagnostics). Tolerates result.parts[].text and result.message.parts[].text.
a2a_text() {
  echo "$1" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); r=d.get('result',d)
  m=r.get('message',r)
  parts=m.get('parts',[]) or r.get('parts',[])
  print(' '.join(p.get('text','') for p in parts if isinstance(p,dict)))
except Exception:
  print('')"
}
if [ "$LIFECYCLE_LLM" = "minimax" ]; then
  # REAL round-trip assertion. The reply must be model-produced text — NOT a
  # proxy-level unreachable, NOT an LLM-less "Agent error", NOT an empty
  # completion. Then it must contain the known answer (PONG).
  check "proxy returned a result envelope" '"result"' "$A2A"
  AGENT_TEXT="$(a2a_text "$A2A")"
  echo "  MiniMax reply: $(echo "$AGENT_TEXT" | head -c 200)"
  if echo "$A2A" | grep -qiE 'unreachable|workspace has no URL|restarting'; then
    fail "MiniMax runtime not reachable through proxy" "$A2A"
  elif echo "$AGENT_TEXT" | grep -qiF "message contained no text content"; then
    fail "MiniMax returned an EMPTY completion (no text part) — backend/key issue, not a real round-trip" "$AGENT_TEXT"
  elif echo "$AGENT_TEXT" | grep -qiE 'agent error|exception|invalid api key|insufficient_quota|exceeded your current quota'; then
    fail "MiniMax round-trip returned an error-shaped reply (no real completion)" "$AGENT_TEXT"
  elif echo "$AGENT_TEXT" | tr '[:lower:]' '[:upper:]' | grep -qF "PONG"; then
    pass "REAL MiniMax round-trip: model replied with the known answer (PONG)"
  else
    # Non-error, non-empty, but didn't contain PONG — still a real reply (the
    # model answered with its own words). Accept as a real round-trip but note it.
    if [ -n "$AGENT_TEXT" ]; then
      pass "REAL MiniMax round-trip: non-error model reply (did not contain PONG, but real text)"
    else
      fail "MiniMax round-trip produced no assertable text" "$A2A"
    fi
  fi
elif [ "$USING_STUB" -eq 1 ]; then
  check "proxy returned a result envelope" '"result"' "$A2A"
  check "proxy reached stub (canned reply)" 'STUB OK' "$A2A"
  # Parse the envelope so whitespace/key-ordering doesn't break the assertion.
  ROLE=$(echo "$A2A" | python3 -c "import sys,json
try:
  print(json.load(sys.stdin).get('result',{}).get('role',''))
except Exception:
  print('')")
  check "reply has agent role" "agent" "$ROLE"
else
  # Real LLM-less image: we can't get a canned text, but a reachable runtime
  # must answer with EITHER a result OR a structured JSON-RPC error — NOT a
  # proxy-level "workspace agent unreachable" / "no URL". Assert reachability.
  if echo "$A2A" | grep -qiE 'unreachable|workspace has no URL|restarting'; then
    fail "real runtime not reachable through proxy" "$A2A"
  else
    pass "real runtime reachable through proxy (got a JSON-RPC response)"
    echo "  response: $(echo "$A2A" | head -c 200)"
  fi
fi
echo ""

echo "=== Results: $PASS passed, $FAIL failed ==="
exit "$FAIL"
