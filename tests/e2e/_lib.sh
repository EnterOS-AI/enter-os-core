#!/usr/bin/env bash
# Common E2E helpers. Source this from every tests/e2e/*.sh.
#
# Usage:
#   source "$(dirname "$0")/_lib.sh"
#   e2e_cleanup_all_workspaces   # call at top of script
#   TOKEN=$(echo "$register_response" | e2e_extract_token)
#
# BASE defaults to http://localhost:8080. Set it before sourcing to override.

: "${BASE:=http://localhost:8080}"
export BASE

# Emit the auth_token from a /registry/register response on stdout.
# See _extract_token.py for the exact semantics.
e2e_extract_token() {
  python3 "$(dirname "${BASH_SOURCE[0]}")/_extract_token.py"
}

# Populate a curl-args array with the platform admin bearer, IF one is set.
#
# AdminAuth (workspace-server/internal/middleware/wsauth_middleware.go:161)
# fail-opens ONLY while ADMIN_TOKEN is unset AND no workspace token exists yet
# (devmode.go:50). The e2e-api CI job now sets ADMIN_TOKEN on the platform and
# exports the matching MOLECULE_ADMIN_TOKEN here, which flips fail-open OFF — so
# every admin-gated route (GET/POST/DELETE /workspaces, /events, /bundles,
# /org/import, …) now requires the EXACT ADMIN_TOKEN as bearer (Tier-2b rejects
# workspace bearers, wsauth_middleware.go:250). Helpers that hit admin routes
# (e2e_cleanup_all_workspaces, e2e_delete_workspace's default path) must send it.
#
# Guarded if-set so a bootstrap/dev platform with no admin token (fail-open)
# still works with zero auth. Mirrors e2e_mint_workspace_token's admin_auth.
#
# Usage:
#   local admin_auth=(); e2e_admin_auth_args admin_auth
#   curl -s "$BASE/workspaces" ${admin_auth[@]+"${admin_auth[@]}"}
e2e_admin_auth_args() {
  local _outname="$1"
  local _bearer="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
  if [ -n "$_bearer" ]; then
    eval "$_outname=(-H \"Authorization: Bearer \$_bearer\")"
  else
    eval "$_outname=()"
  fi
}

# Delete every workspace currently on the platform. Use at the top of a
# script so count-based assertions are reproducible across runs.
# Mint a fresh workspace auth token via the real admin endpoint.
#
# Usage:
#   TOKEN=$(e2e_mint_workspace_token "$workspace_id") || exit 1
e2e_mint_workspace_token() {
  local wid="$1"
  if [ -z "$wid" ]; then
    echo "e2e_mint_workspace_token: workspace id required" >&2
    return 2
  fi
  local body
  local admin_bearer="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
  local admin_auth=()
  [ -n "$admin_bearer" ] && admin_auth=(-H "Authorization: Bearer $admin_bearer")
  body=$(curl -s -X POST -w "\n%{http_code}" "$BASE/admin/workspaces/$wid/tokens" ${admin_auth[@]+"${admin_auth[@]}"})
  local code
  code=$(printf '%s' "$body" | tail -n1)
  local json
  json=$(printf '%s' "$body" | sed '$d')
  if [ "$code" != "201" ]; then
    echo "e2e_mint_workspace_token: got HTTP $code from POST /admin/workspaces/:id/tokens" >&2
    return 1
  fi
  printf '%s' "$json" | python3 -c "import json,sys; print(json.load(sys.stdin)['auth_token'])"
}

e2e_delete_workspace() {
  local wid="$1"
  local name="${2:-}"
  shift 2 || true
  local curl_args=("$@")
  if [ -z "$wid" ]; then
    return 0
  fi
  # DELETE /workspaces/:id and GET /workspaces/:id-for-name are both behind
  # AdminAuth (router.go:155 GET single is public, but List/Delete are gated at
  # router.go:165-167). Callers that already pass a per-workspace bearer (e.g.
  # test_api.sh's NEW_TOKEN) authenticate themselves; the cleanup-trap callers
  # in poll-mode/notify/priority pass NO curl args and rely on this fallback to
  # the platform admin bearer so the DELETE doesn't 401 once ADMIN_TOKEN is set.
  if [ "${#curl_args[@]}" -eq 0 ]; then
    e2e_admin_auth_args curl_args
  fi
  # ${curl_args[@]+"…"} guard: under `set -u` an empty array expands to an
  # "unbound variable" error on bash <4.4 (macOS 3.2, some Linux). This form
  # expands to nothing when the array is empty. Callers from the priority-
  # runtimes EXIT trap pass no extra curl args, so the array IS empty there —
  # without the guard the trap aborts non-zero AFTER the gate already passed,
  # turning a validated run RED. (Same idiom already used for CREATED_WSIDS.)
  if [ -z "$name" ]; then
    name=$(curl -s "$BASE/workspaces/$wid" ${curl_args[@]+"${curl_args[@]}"} | python3 -c "import json,sys
try:
  print(json.load(sys.stdin).get('name',''))
except Exception:
  pass" 2>/dev/null || true)
  fi
  e2e_gated_admin_op "$wid" curl -s -X DELETE "$BASE/workspaces/$wid?confirm=true" \
    -H "X-Confirm-Name: $name" ${curl_args[@]+"${curl_args[@]}"} > /dev/null || true
}

# ---------------------------------------------------------------------------
# Docker container / volume naming helpers (KI-013 / SEV-2499).
#
# KI-013 changed workspace container and volume names from truncated 12-char
# IDs to full UUIDs. These helpers are the bash SSOT for that naming scheme.
# They MUST be kept in sync with the Go equivalents in:
#   workspace-server/internal/provisioner/provisioner.go
#
#   ContainerName(workspaceID)        -> ws-<workspaceID>
#   ConfigVolumeName(workspaceID)     -> ws-<workspaceID>-configs
#   ClaudeSessionVolumeName(wsID)     -> ws-<workspaceID>-claude-sessions
#   buildWorkspaceMount(wsID)         -> ws-<workspaceID>-workspace
#
# The drift-guard script .gitea/scripts/lint-e2e-ki013-container-names.sh
# fails CI if any e2e script uses bash substring truncation in a ws-* context.
# ---------------------------------------------------------------------------

# e2e_container_name returns the Docker container name for a workspace.
# Keep in sync with provisioner.ContainerName.
e2e_container_name() {
  echo "ws-${1}"
}

# e2e_config_volume_name returns the Docker named volume for a workspace's
# /configs directory. Keep in sync with provisioner.ConfigVolumeName.
e2e_config_volume_name() {
  echo "ws-${1}-configs"
}

# e2e_session_volume_name returns the Docker named volume for a workspace's
# Claude Code session directory. Keep in sync with provisioner.ClaudeSessionVolumeName.
e2e_session_volume_name() {
  echo "ws-${1}-claude-sessions"
}

# e2e_workspace_volume_name returns the Docker named volume for a workspace's
# /workspace directory. Keep in sync with buildWorkspaceMount in provisioner.go.
e2e_workspace_volume_name() {
  echo "ws-${1}-workspace"
}

e2e_cleanup_all_workspaces() {
  # GET /workspaces (list) is AdminAuth-gated (router.go:165). Send the platform
  # admin bearer if one is set so the list doesn't 401 → empty → no cleanup.
  local _admin_auth=()
  e2e_admin_auth_args _admin_auth
  curl -s "$BASE/workspaces" ${_admin_auth[@]+"${_admin_auth[@]}"} | python3 -c "import json,sys
try:
  [print(f\"{w.get('id','')}\\t{w.get('name','')}\") for w in json.load(sys.stdin)]
except Exception:
  pass" 2>/dev/null | while IFS=$'\t' read -r _wid _name; do
    e2e_delete_workspace "$_wid" "$_name"
  done
}

# e2e_gated_admin_op runs a curl invocation, and if the platform returns
# 202/pending_approval (the admin-token path hitting approvals.IsGated — see
# workspace-server/internal/handlers/approval_gate.go), auto-approves via
# /workspaces/:id/approvals/:approvalId/decide and retries the operation.
#
# CR2 RC 10818 made the admin-token gate always-on (it was previously
# org-token-only with a rollout flag). The E2E API Smoke harness uses the
# platform admin bearer for workspace CRUD (create/delete), so Delete now
# returns 202 pending_approval — which broke the smoke's
# "DELETE /workspaces/:id" + "List after delete (count=1)" + "All deleted
# (count=0)" assertions (job 468330, exitcode 6, 1m4s, NOT infra).
#
# The gate is CORRECT (delete_workspace is destructive; the reviewer's
# verdict PASSED on all 4 axes). The harness needs the auto-approve loop,
# NOT a policy.go narrowing — see CR-B 10858 / 10869 / 10870.
#
# Usage:
#   R=$(e2e_gated_admin_op <workspace_id_for_approve> <curl args...>)
#
#   The workspace_id is used ONLY to build the /approvals/:id/decide URL
#   (POST /workspaces has no workspace_id yet; pass "" and the helper
#   will skip approve-on-202 — the gate never fires for create today).
#   For DELETE/PATCH/CascadeDelete, pass the target workspace id.
#
# This helper preserves the security guarantee: the gate still fires for
# every admin-token call. The harness is the auto-approver, simulating
# the human-in-the-loop that production deployments use.
e2e_gated_admin_op() {
  local _wid="$1"; shift
  local _curl_args=("$@")
  local _resp
  _resp=$("${_curl_args[@]}")
  # Detect pending_approval — Python parses + emits the approval_id on stdout
  # if present, empty string otherwise. JSON parse failure (e.g. 502 HTML) is
  # treated as "not gated" so the smoke fails on real errors rather than
  # silently retrying.
  local _approval_id
  _approval_id=$(printf '%s' "$_resp" | python3 -c "import json,sys
try:
  d=json.load(sys.stdin)
  if d.get('status')=='pending_approval':
    print(d.get('approval_id',''))
except Exception:
  pass" 2>/dev/null || true)
  if [ -n "$_approval_id" ] && [ -n "$_wid" ]; then
    # Auto-approve via the same admin bearer. 200 with approval_id consumed.
    local _admin_auth=()
    e2e_admin_auth_args _admin_auth
    curl -s -X POST "$BASE/workspaces/$_wid/approvals/$_approval_id/decide" \
      -H "Content-Type: application/json" \
      -d '{"decision":"approved","decided_by":"e2e-api-smoke"}' \
      ${_admin_auth[@]+"${_admin_auth[@]}"} > /dev/null || true
    # Retry the original operation. The gate's consume-once
    # (approval_gate.go UPDATE … RETURNING id) means the SECOND call finds
    # the now-approved request and proceeds (returns true from gateDestructive).
    _resp=$("${_curl_args[@]}")
  fi
  printf '%s' "$_resp"
}
