# Sourceable helper for harness replays. Centralises the
# curl-against-cf-proxy pattern so scripts don't depend on /etc/hosts.
#
# Production CF tunnel routes by Host header, not by DNS — the request
# URL is to a public CF endpoint and the Host header carries the
# per-tenant identity. We replay the same shape locally:
#
#   curl -H "Host: harness-tenant-alpha.localhost" http://localhost:8080/health
#
# This matches what cf-proxy/nginx.conf already routes (`server_name
# *.localhost` + `map $host $tenant_upstream`) and avoids the macOS
# /etc/hosts requirement that previously gated the harness behind a
# sudo step.
#
# Multi-tenant since Phase 2: alpha and beta tenants run in parallel.
# `curl_alpha_admin` and `curl_beta_admin` target each tenant's URL
# with that tenant's ADMIN_TOKEN + MOLECULE_ORG_ID. The legacy
# `curl_admin` is aliased to alpha for backwards compat with the
# pre-Phase-2 single-tenant replays.
#
# Usage:
#   HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$HERE/../_curl.sh"     # from replays/<name>.sh
#   curl_alpha_admin "$BASE/health"
#   curl_beta_admin  "$BASE/health"

# Bind to the cf-proxy's loopback port — the proxy front-doors every
# tenant and routes by Host header, exactly like production's CF tunnel.
: "${BASE:=http://localhost:8080}"

# Per-tenant identity. Each pair must match the corresponding tenant
# container's environment in compose.yml or auth/TenantGuard will fail
# in non-obvious ways (401 vs 403 vs silent route to wrong tenant).
: "${ALPHA_HOST:=harness-tenant-alpha.localhost}"
: "${ALPHA_ADMIN_TOKEN:=harness-admin-token-alpha}"
: "${ALPHA_ORG_ID:=harness-org-alpha}"

: "${BETA_HOST:=harness-tenant-beta.localhost}"
: "${BETA_ADMIN_TOKEN:=harness-admin-token-beta}"
: "${BETA_ORG_ID:=harness-org-beta}"

# Legacy single-tenant aliases — pre-Phase-2 replays use these without
# knowing the topology grew. They map to alpha. New replays should use
# the explicit alpha/beta variants for clarity.
: "${TENANT_HOST:=$ALPHA_HOST}"
: "${ADMIN_TOKEN:=$ALPHA_ADMIN_TOKEN}"
: "${ORG_ID:=$ALPHA_ORG_ID}"

# ─── Anonymous (no auth) ──────────────────────────────────────────────

# Anonymous request to alpha. Use for /health, /buildinfo, etc.
curl_alpha_anon() {
    curl -sS -H "Host: ${ALPHA_HOST}" "$@"
}

# Anonymous request to beta.
curl_beta_anon() {
    curl -sS -H "Host: ${BETA_HOST}" "$@"
}

# Legacy alias for single-tenant replays.
curl_anon() {
    curl -sS -H "Host: ${TENANT_HOST}" "$@"
}

# ─── Admin-token requests ─────────────────────────────────────────────

# Admin-token request to alpha tenant. SaaS-shape auth: bearer token,
# tenant org header (TenantGuard activates), JSON content type.
curl_alpha_admin() {
    curl -sS \
        -H "Host: ${ALPHA_HOST}" \
        -H "Authorization: Bearer ${ALPHA_ADMIN_TOKEN}" \
        -H "X-Molecule-Org-Id: ${ALPHA_ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# Admin-token request to beta tenant.
curl_beta_admin() {
    curl -sS \
        -H "Host: ${BETA_HOST}" \
        -H "Authorization: Bearer ${BETA_ADMIN_TOKEN}" \
        -H "X-Molecule-Org-Id: ${BETA_ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# Legacy alias.
curl_admin() {
    curl_alpha_admin "$@"
}

# ─── Cross-tenant negative-test helpers ───────────────────────────────
# These exist to MAKE WRONG calls — replays use them to assert
# TenantGuard rejects them. Names spell out what's mismatched.

# alpha bearer + alpha org, but talking to beta's URL. TenantGuard
# should reject because the org header doesn't match beta's MOLECULE_ORG_ID.
curl_alpha_creds_at_beta() {
    curl -sS \
        -H "Host: ${BETA_HOST}" \
        -H "Authorization: Bearer ${ALPHA_ADMIN_TOKEN}" \
        -H "X-Molecule-Org-Id: ${ALPHA_ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# beta bearer + beta org, but talking to alpha's URL.
curl_beta_creds_at_alpha() {
    curl -sS \
        -H "Host: ${ALPHA_HOST}" \
        -H "Authorization: Bearer ${BETA_ADMIN_TOKEN}" \
        -H "X-Molecule-Org-Id: ${BETA_ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# ─── Workspace-scoped (per-workspace bearer) ──────────────────────────

# Workspace-scoped request to alpha — uses a per-workspace bearer
# minted from /admin/workspaces/:id/test-token. Caller must export
# WORKSPACE_TOKEN.
curl_workspace() {
    : "${WORKSPACE_TOKEN:?WORKSPACE_TOKEN must be set — mint via /admin/workspaces/:id/test-token}"
    curl -sS \
        -H "Host: ${TENANT_HOST}" \
        -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
        -H "X-Molecule-Org-Id: ${ORG_ID}" \
        -H "Content-Type: application/json" \
        "$@"
}

# ─── Postgres exec (per-tenant) ───────────────────────────────────────

# Direct postgres exec — for replays that need to seed activity_logs
# rows or read DB state that has no public HTTP route.
#
# SECRETS_ENCRYPTION_KEY placeholder lets compose validate without
# requiring up.sh's per-run key (exec doesn't actually use it but
# compose validates the file).
psql_exec_alpha() {
    SECRETS_ENCRYPTION_KEY="${SECRETS_ENCRYPTION_KEY:-exec-placeholder}" \
    docker compose -f "${HARNESS_COMPOSE:-$(dirname "${BASH_SOURCE[0]}")/compose.yml}" \
        exec -T postgres-alpha \
        psql -U harness -d molecule -At "$@"
}

psql_exec_beta() {
    SECRETS_ENCRYPTION_KEY="${SECRETS_ENCRYPTION_KEY:-exec-placeholder}" \
    docker compose -f "${HARNESS_COMPOSE:-$(dirname "${BASH_SOURCE[0]}")/compose.yml}" \
        exec -T postgres-beta \
        psql -U harness -d molecule -At "$@"
}

# Legacy alias — single-tenant replays default to alpha's DB.
psql_exec() {
    psql_exec_alpha "$@"
}
