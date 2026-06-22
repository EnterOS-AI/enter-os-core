#!/bin/bash
# Post-rebuild setup — run after docker compose up -d --build
# Inserts global secrets that the provisioner injects into every workspace container.
# Without these, agents can't call MiniMax or push to GitHub.
#
# Required env vars (set in .env or export before running):
#   MINIMAX_API_KEY  — MiniMax M2.7 API key
#   GITHUB_PAT       — GitHub fine-grained PAT (366-day)
#   ADMIN_TOKEN      — platform admin token

set -euo pipefail

DB_CONTAINER="${DB_CONTAINER:-molecule-monorepo-postgres-1}"
DB_USER="${DB_USER:-dev}"
DB_NAME="${DB_NAME:-molecule}"
PLATFORM_URL="${PLATFORM_URL:-http://127.0.0.1:8080}"

# Source .env if it exists (picks up ADMIN_TOKEN, MINIMAX_API_KEY, GITHUB_PAT)
if [ -f .env ]; then
    set -a; source .env; set +a
fi

# Validate required secrets
if [ -z "${MINIMAX_API_KEY:-}" ]; then
    echo "ERROR: MINIMAX_API_KEY not set. Add to .env or export it."
    exit 1
fi
if [ -z "${GITHUB_PAT:-}" ]; then
    echo "ERROR: GITHUB_PAT not set. Add to .env or export it."
    exit 1
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
    echo "ERROR: ADMIN_TOKEN not set. Add to .env or export it."
    exit 1
fi

echo "=== Waiting for platform health ==="
until curl -s --max-time 5 "$PLATFORM_URL/health" >/dev/null 2>&1; do
    echo "  waiting..."
    sleep 3
done
echo "  platform up"

echo "=== Inserting global secrets ==="
docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -c "
INSERT INTO global_secrets (key, encrypted_value, encryption_version) VALUES
('ANTHROPIC_BASE_URL', 'https://api.minimax.io/anthropic', 0),
('ANTHROPIC_AUTH_TOKEN', '$MINIMAX_API_KEY', 0),
('ANTHROPIC_MODEL', 'MiniMax-M2.7', 0),
('ANTHROPIC_SMALL_FAST_MODEL', 'MiniMax-M2.7', 0),
('API_TIMEOUT_MS', '3000000', 0),
('CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC', '1', 0),
('GITHUB_TOKEN', '$GITHUB_PAT', 0)
ON CONFLICT (key) DO UPDATE SET encrypted_value = EXCLUDED.encrypted_value;
"
echo "  7 global secrets set"

echo "=== Importing org template ==="
curl -s --max-time 600 -X POST "$PLATFORM_URL/org/import" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"dir":"molecule-dev"}' | head -1
echo ""
echo "  import complete"

echo "=== Done ==="
echo "Run: http://127.0.0.1:3000 for canvas"
