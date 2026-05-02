#!/usr/bin/env bash
# Bring the production-shape harness up.
#
# Usage: ./up.sh [--rebuild]
#
# Always operates in tests/harness/ regardless of where it's invoked
# from — test scripts under tests/harness/replays/ source it via the
# absolute path, so cd-ing first prevents compose-context surprises.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

REBUILD=false
for arg in "$@"; do
    case "$arg" in
        --rebuild) REBUILD=true ;;
    esac
done

# Generate a per-run encryption key. The tenant runs with
# MOLECULE_ENV=production (intentional, to replay prod-shape bugs), and
# crypto.InitStrict() refuses to boot without SECRETS_ENCRYPTION_KEY.
# Generate fresh so:
#   - No key-shaped string lives in the repo (avoids muscle-memorying a
#     hardcoded value into other places + secret-scanner false positives).
#   - Each harness lifetime gets a unique key, mimicking prod's per-tenant
#     isolation. Persistence across runs isn't required — the harness DB
#     is wiped on every ./down.sh.
# Honor a caller-supplied value if already exported (lets a debug session
# pin a key for reproducibility).
if [ -z "${SECRETS_ENCRYPTION_KEY:-}" ]; then
    SECRETS_ENCRYPTION_KEY=$(openssl rand -base64 32)
    export SECRETS_ENCRYPTION_KEY
fi

if [ "$REBUILD" = true ]; then
    docker compose -f compose.yml build --no-cache tenant cp-stub
fi

echo "[harness] starting redis + cp-stub + tenant-alpha + tenant-beta + cf-proxy ..."
docker compose -f compose.yml up -d --wait

# Sudo-free reachability: cf-proxy/nginx routes by Host header to the
# right tenant container (matches production CF tunnel: same URL,
# different Host = different tenant). Replays target loopback :8080
# with a per-tenant Host header. _curl.sh centralises the helper
# functions (curl_alpha_admin, curl_beta_admin, etc.).
echo ""
echo "[harness] up. Multi-tenant topology:"
echo "          tenant-alpha:  Host: harness-tenant-alpha.localhost"
echo "          tenant-beta:   Host: harness-tenant-beta.localhost"
echo "          legacy alias:  Host: harness-tenant.localhost → alpha"
echo ""
echo "          Quick check (no /etc/hosts needed):"
echo "            curl -H 'Host: harness-tenant-alpha.localhost' http://localhost:8080/health"
echo "            curl -H 'Host: harness-tenant-beta.localhost'  http://localhost:8080/health"
echo ""
echo "Next: ./seed.sh   # register parent+child workspaces in BOTH tenants"
