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

if [ "$REBUILD" = true ]; then
    docker compose -f compose.yml build --no-cache tenant cp-stub
fi

echo "[harness] starting cp-stub + postgres + redis + tenant + cf-proxy ..."
docker compose -f compose.yml up -d --wait

echo "[harness] /etc/hosts entry for harness-tenant.localhost..."
if ! grep -q '^127\.0\.0\.1[[:space:]]\+harness-tenant\.localhost' /etc/hosts; then
    echo "  (skip — your /etc/hosts may not resolve *.localhost. If tests fail with"
    echo "   'getaddrinfo' errors, add: 127.0.0.1 harness-tenant.localhost)"
fi

echo ""
echo "[harness] up. Tenant: http://harness-tenant.localhost:8080/health"
echo "                     http://harness-tenant.localhost:8080/buildinfo"
echo "          cp-stub:    http://localhost (internal-only via compose net)"
echo ""
echo "Next: ./seed.sh   # mint admin token + register sample workspaces"
