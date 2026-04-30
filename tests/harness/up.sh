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
