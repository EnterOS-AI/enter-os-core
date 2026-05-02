# Sourceable helper for harness replays. Centralises the
# curl-against-cf-proxy pattern so scripts don't depend on /etc/hosts.
#
# Production CF tunnel routes by Host header, not by DNS — the request
# URL is to a public CF endpoint and the Host header carries the
# per-tenant identity. We replay the same shape locally:
#
#   curl -H "Host: harness-tenant.localhost" http://localhost:8080/health
#
# This matches what cf-proxy/nginx.conf already routes (`server_name
# *.localhost localhost`) and avoids the macOS /etc/hosts requirement
# that previously gated the harness behind a sudo step.
#
# Backwards-compatible: if /etc/hosts resolves harness-tenant.localhost
# (the legacy path), the bare URL still works because the helper falls
# back to that. New scripts SHOULD use the helper functions.
#
# Usage:
#   HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$HERE/../_curl.sh"     # from replays/<name>.sh
#   curl_admin "$BASE/health"
#   curl_anon  "$BASE/health"

# Bind to the cf-proxy's loopback port — the proxy front-doors every
# tenant and routes by Host header, exactly like production's CF tunnel.
: "${BASE:=http://localhost:8080}"
: "${TENANT_HOST:=harness-tenant.localhost}"
: "${ADMIN_TOKEN:=harness-admin-token}"
: "${ORG_ID:=harness-org}"

# Anonymous request — only Host header (no auth). Use for /health,
# /buildinfo, and any other route that's intentionally public.
curl_anon() {
    curl -sS -H "Host: ${TENANT_HOST}" "$@"
}

# Admin-token request — full SaaS auth shape. Sets the bearer token,
# tenant org header (activates TenantGuard middleware), and a default
# JSON Content-Type. Replays admin paths exactly the way CP does in
# production, so any TenantGuard / strict-auth bug surfaces locally.
curl_admin() {
    curl -sS \
        -H "Host: ${TENANT_HOST}" \
        -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        -H "X-Molecule-Org-Id: ${ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# Workspace-scoped request — uses a per-workspace bearer minted from
# /admin/workspaces/:id/test-token. The platform's auth.go middleware
# accepts this bearer for the workspace's own routes, so this is the
# right shape for replays that exercise an in-workspace tool calling
# back to the platform (chat_history, list_peers, etc).
#
# Caller must export WORKSPACE_TOKEN before invoking.
curl_workspace() {
    : "${WORKSPACE_TOKEN:?WORKSPACE_TOKEN must be set — mint via /admin/workspaces/:id/test-token}"
    curl -sS \
        -H "Host: ${TENANT_HOST}" \
        -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
        -H "X-Molecule-Org-Id: ${ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# Direct postgres exec — for replays that need to seed activity_logs
# rows or read DB state that has no public HTTP route. Wraps the
# `docker compose exec` pattern so replays can stay shell-only.
#
# SECRETS_ENCRYPTION_KEY is set to a placeholder so compose's `:?must
# be set` interpolation guard (which gates running the harness without
# up.sh) doesn't trip on `exec` — exec only reaches an already-running
# service so the env var is irrelevant, but compose still validates
# the file. The placeholder is never written anywhere or used by any
# service.
psql_exec() {
    SECRETS_ENCRYPTION_KEY="${SECRETS_ENCRYPTION_KEY:-exec-placeholder}" \
    docker compose -f "${HARNESS_COMPOSE:-$(dirname "${BASH_SOURCE[0]}")/compose.yml}" \
        exec -T postgres \
        psql -U harness -d molecule -At "$@"
}
