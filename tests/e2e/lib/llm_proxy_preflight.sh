#!/usr/bin/env bash
# LLM-proxy preflight helper for completion-gated e2e lanes (core#2675).
#
# PURPOSE
# =======
# Before booting workspaces (an expensive, multi-minute operation), confirm
# the staging LLM proxy can serve a cheap completion. The 2026-06-12 staging
# LLM outage (~21:10-21:38Z) produced 4 identical red CI lanes — Staging SaaS
# x3 + Local Provision — with no machine-readable signal distinguishing
# "dependency down" from "real code bug." Triage required forensic log-diffing
# across lanes and (per the issue) initially mis-attributed an unrelated
# deploy-path bug to the outage.
#
# This preflight fast-fails the lane with a DISTINCT, machine-readable status
# description prefix `DEP-DOWN:staging-llm` so the redgate-reporter can:
#   1. file ONE incident issue for the dependency outage (dedup), and
#   2. let operators skip the lane's workspace-boot logic while the
#      dependency is being restored.
#
# The convention (status description prefix + per-run dedup) is the whole
# deliverable; the actual LLM-proxy endpoint is configurable via env so the
# same helper works across lanes with different proxy URLs (e.g. the
# staging SaaS stack uses a different LLM proxy than the local-provision
# dev proxy).
#
# CONVENTIONS
# ===========
#   - Source this lib AFTER the host script defines fail()/ok()/log().
#   - Call `llm_proxy_preflight` (no args). It reads E2E_LLM_PROXY_URL
#     (or falls back to deriving one from MOLECULE_CP_URL) and exits the
#     whole lane on failure.
#   - The status description prefix `DEP-DOWN:staging-llm` is the SSOT —
#     `redgate-reporter` parses this and dedups. Do NOT change the prefix
#     without coordinating the redgate-reporter's parser.
#
# STATUS CODES
# ============
#   0   preflight OK (the proxy is reachable and returned an HTTP response)
#   70  DEP-DOWN:staging-llm (proxy unreachable, slow, or returned a 5xx)
#   71  E2E_LLM_PROXY_URL not set and the URL could not be derived
#
# Why distinct exit codes: the redgate-reporter and the workflow's notify
# step can use them to differentiate "infrastructure down" from "config
# missing" (the latter is operator error and should not dedup against
# live dependency outages).
#
# SEMANTICS NOTE (#76 root cause, 2026-06-13):
#   The preflight sends an UNauthenticated probe. A healthy staging LLM proxy
#   that requires auth correctly returns 401. Previously any non-200 status
#   (including 401) was classified as DEP-DOWN, causing fleet-wide false
#   staging-down incidents. The preflight only needs to prove REACHABILITY:
#   any HTTP response (including 401/403/404) means the proxy is up. Only
#   transport failures (connection refused, timeout) or 5xx server errors
#   classify as DEP-DOWN.

# e2e_llm_proxy_preflight
#   Source the lib's `llm_proxy_preflight` function. Returns 0 on success,
#   70/71 on the dedicated DEP-DOWN / config-missing cases.
llm_proxy_preflight() {
  local proxy_url="${E2E_LLM_PROXY_URL:-}"
  local timeout_secs="${E2E_LLM_PROXY_TIMEOUT:-30}"

  if [ -z "$proxy_url" ]; then
    # Derive from the CP URL when not set. The platform-managed LLM proxy
    # is exposed at <cp_url>/api/v1/internal/llm/openai/v1; the staging
    # instance lives at staging-api.moleculesai.app. E2E_LLM_PROXY_URL
    # override stays available for lanes that point at a different proxy
    # (local provision uses the local workspace-server's built-in proxy).
    if [ -n "${MOLECULE_CP_URL:-}" ]; then
      proxy_url="${MOLECULE_CP_URL%/}/api/v1/internal/llm/openai/v1/chat/completions"
    fi
  fi

  if [ -z "$proxy_url" ]; then
    # Config-missing is NOT a dependency-down condition — it is operator
    # error (an E2E_LANE was wired without setting E2E_LLM_PROXY_URL or
    # MOLECULE_CP_URL). Emit a distinct CONFIG-MISSING prefix so the
    # redgate-reporter dedups separately: DEP-DOWN dedups against
    # live dependency outages; CONFIG-MISSING dedups against the same
    # misconfiguration across runs/lanes. Do NOT change the prefix
    # without coordinating the redgate-reporter's parser.
    echo "::error::CONFIG-MISSING:staging-llm-proxy-url E2E_LLM_PROXY_URL is unset and could not be derived from MOLECULE_CP_URL"
    return 71
  fi

  # Cheap, auth-less reachability probe: minimal token count, no streaming.
  # The model name is a no-op for reachability; the bare slug avoids a
  # provider-specific 400 on proxies that validate model IDs.
  local body
  body=$(cat <<'JSON'
{"model":"MiniMax-M2.7","max_tokens":1,"messages":[{"role":"user","content":"pong"}]}
JSON
)

  local tmpfile http_code
  tmpfile=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$tmpfile'" RETURN

  http_code=$(curl -sS -o "$tmpfile" -w "%{http_code}" \
    --max-time "$timeout_secs" \
    -H "Content-Type: application/json" \
    -X POST \
    -d "$body" \
    "$proxy_url" 2>/dev/null) || http_code="000"

  # #76 semantics fix: the preflight only needs to prove the proxy is
  # reachable and speaking HTTP. An auth-required proxy returns 401; a
  # mis-routed probe returns 404 — both mean the proxy is UP. Only
  # transport failures (http_code=000) or 5xx server errors mean DOWN.
  if [ "$http_code" = "000" ] || [[ "$http_code" == 5* ]]; then
    # NOTE: the prefix `DEP-DOWN:staging-llm` is the SSOT that the
    # redgate-reporter parses for dedup. Do not edit without coordinating
    # with the redgate-reporter's parser in molecule-ci.
    echo "::error::DEP-DOWN:staging-llm preflight failed: proxy=$proxy_url http_code=$http_code body=$(head -c 500 "$tmpfile" 2>/dev/null)"
    return 70
  fi

  return 0
}
