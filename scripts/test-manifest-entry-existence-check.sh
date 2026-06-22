#!/usr/bin/env bash
# scripts/test-manifest-entry-existence-check.sh
#
# Regression tests for scripts/manifest-entry-existence-check.sh.
# Verifies the retry loop fails closed on persistent non-200 statuses
# (500, 403, network failures) and succeeds when retries eventually return 200.
#
# Run: bash scripts/test-manifest-entry-existence-check.sh
# Expected: "All N tests passed" + exit 0.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/manifest-entry-existence-check.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

# ─────────────────────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────────────────────

run_script() {
    # Args: <fixture-file> [extra-env...]
    local fixture="$1"
    shift
    set +e
    env \
        GITEA_HOST="git.example.com" \
        GITEA_TOKEN="test-token" \
        PATH="$TMP:$PATH" \
        "$@" \
        bash "$SCRIPT" "$fixture" 2>&1
    local rc=$?
    set -e
    echo "EXIT_CODE=$rc"
}

assert_match() {
    local name="$1" got="$2" pattern="$3"
    if printf '%s' "$got" | grep -qE "$pattern"; then
        PASS=$((PASS + 1))
        printf '  ✓ %s\n' "$name"
    else
        FAIL=$((FAIL + 1))
        printf '  ✗ %s\n    want pattern: %s\n    got:\n%s\n' "$name" "$pattern" "$got"
    fi
}

assert_not_match() {
    local name="$1" got="$2" pattern="$3"
    if printf '%s' "$got" | grep -qE "$pattern"; then
        FAIL=$((FAIL + 1))
        printf '  ✗ %s\n    bad pattern matched: %s\n    got:\n%s\n' "$name" "$pattern" "$got"
    else
        PASS=$((PASS + 1))
        printf '  ✓ %s\n' "$name"
    fi
}

# ─────────────────────────────────────────────────────────────────────────────
# Mock curl
# ─────────────────────────────────────────────────────────────────────────────

# The mock curl reads MOCK_MODE to decide what status to return.
# It accepts the same flags the script uses and echoes the status code.
# jq is also mocked so tests run on hosts without jq installed.
mkdir -p "$TMP"

cat > "$TMP/jq" <<'EOF'
#!/usr/bin/env python3
import json, re, sys
# The checker invokes jq as: jq -r ".<category> | length" or jq -r ".<category>[N].<field>".
# The query is always the last argument; -r can be ignored for the mock.
query = sys.argv[-1]
obj = json.load(sys.stdin)

# Support queries used by the script: .<category> | length  and  .<category>[N].<field>
m = re.fullmatch(r'\.([A-Za-z_][A-Za-z0-9_]*)\s*\|\s*length', query)
if m:
    print(len(obj.get(m.group(1), [])))
    sys.exit(0)

m = re.fullmatch(r'\.([A-Za-z_][A-Za-z0-9_]*)\[(\d+)\]\.([A-Za-z_][A-Za-z0-9_]*)', query)
if m:
    cat = obj.get(m.group(1), [])
    idx = int(m.group(2))
    field = m.group(3)
    if idx < len(cat):
        print(cat[idx].get(field, ''))
    else:
        print('')
    sys.exit(0)

print(json.dumps(obj))
EOF
chmod +x "$TMP/jq"

cat > "$TMP/curl" <<'EOF'
#!/usr/bin/env bash
# Mock curl for manifest-entry-existence-check tests.
# Returns the status stored in MOCK_MODE for every URL.
set -euo pipefail
mode="${MOCK_MODE-200}"
# Consume and ignore flags; the script always passes -sS -o /dev/null -w etc.
while [ "$#" -gt 0 ]; do
    case "$1" in
        -s|-S|-o|--max-time) shift 2 ;;
        -w|-H) shift 2 ;;
        *) URL="$1"; shift ;;
    esac
done
printf '%s\n' "$mode"
EOF
chmod +x "$TMP/curl"

# ─────────────────────────────────────────────────────────────────────────────
# Fixtures
# ─────────────────────────────────────────────────────────────────────────────

cat > "$TMP/all-good.json" <<'EOF'
{
  "plugins": [
    {"name": "plugin-a", "repo": "molecule-ai/plugin-a"}
  ],
  "workspace_templates": [
    {"name": "template-a", "repo": "molecule-ai/template-a"}
  ],
  "org_templates": []
}
EOF

cat > "$TMP/mixed.json" <<'EOF'
{
  "plugins": [
    {"name": "plugin-a", "repo": "molecule-ai/plugin-a"},
    {"name": "plugin-b", "repo": "molecule-ai/plugin-b"}
  ],
  "workspace_templates": [],
  "org_templates": []
}
EOF

# ─────────────────────────────────────────────────────────────────────────────
# Test cases
# ─────────────────────────────────────────────────────────────────────────────

echo "1. All entries return HTTP 200 — clean exit"
got=$(MOCK_MODE=200 run_script "$TMP/all-good.json")
assert_match "all-good-success-message" "$got" "All .* manifest entries resolve"
assert_match "all-good-exit-zero" "$got" "EXIT_CODE=0"

echo
echo "2. Persistent HTTP 404 — fails loudly"
got=$(MOCK_MODE=404 run_script "$TMP/all-good.json")
assert_match "404-reports-entry" "$got" "does not exist on Gitea \(404\)"
assert_match "404-exit-one" "$got" "EXIT_CODE=1"

echo
echo "3. Persistent HTTP 500 after retries — fails closed"
got=$(MOCK_MODE=500 run_script "$TMP/all-good.json")
assert_match "500-reports-last-code" "$got" "last HTTP 500"
assert_match "500-exit-one" "$got" "EXIT_CODE=1"
assert_match "500-attempts-three" "$got" "attempt 3"

echo
echo "4. Persistent HTTP 403 after retries — fails closed"
got=$(MOCK_MODE=403 run_script "$TMP/all-good.json")
assert_match "403-reports-last-code" "$got" "last HTTP 403"
assert_match "403-exit-one" "$got" "EXIT_CODE=1"

echo
echo "5. Empty HTTP code (network/gateway failure) — fails closed"
got=$(MOCK_MODE="" run_script "$TMP/all-good.json")
assert_match "empty-code-reports-failure" "$got" "could not be validated after 3 attempts"
assert_match "empty-code-exit-one" "$got" "EXIT_CODE=1"

echo
echo "6. Mixed entries with one 404 and one 200 — counts correctly"
# Use a per-URL mock: plugin-a 200, plugin-b 404
cat > "$TMP/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$#" -gt 0 ]; do
    case "$1" in
        -s|-S|-o|--max-time) shift 2 ;;
        -w|-H) shift 2 ;;
        *) URL="$1"; shift ;;
    esac
done
case "${URL:-}" in
    *plugin-b*) printf '404\n' ;;
    *)          printf '200\n' ;;
esac
EOF
chmod +x "$TMP/curl"
got=$(run_script "$TMP/mixed.json")
assert_match "mixed-reports-404" "$got" "does not exist on Gitea \(404\)"
assert_match "mixed-reports-count" "$got" "1 of 2 manifest entries are broken"
assert_match "mixed-exit-one" "$got" "EXIT_CODE=1"
assert_not_match "mixed-does-not-report-500" "$got" "last HTTP 500"

# ─────────────────────────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────────────────────────

echo
echo "─────────────────────────────────────────────"
echo "Tests:  $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
echo "All tests passed."
