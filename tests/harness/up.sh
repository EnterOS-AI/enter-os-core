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

echo "[harness] starting cp-stub + postgres + redis + tenant + cf-proxy ..."
docker compose -f compose.yml up -d --wait

# Sudo-free reachability: cf-proxy/nginx routes by Host header (matches
# production CF tunnel), so replays target loopback :8080 with a Host
# header rather than depending on /etc/hosts resolution. _curl.sh
# centralises this. Legacy /etc/hosts users still work — the BASE env
# var override accepts either shape.
echo ""
echo "[harness] up."
echo "          Tenant via cf-proxy:  http://localhost:8080/health"
echo "                                 (Host: harness-tenant.localhost)"
echo "          cp-stub:               internal-only via compose net"
echo ""
echo "          Quick check:"
echo "            curl -H 'Host: harness-tenant.localhost' http://localhost:8080/health"
echo ""
echo "Next: ./seed.sh   # mint admin token + register sample workspaces"
