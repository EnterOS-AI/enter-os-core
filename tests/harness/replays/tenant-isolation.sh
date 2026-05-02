#!/usr/bin/env bash
# Replay for cross-tenant isolation — TenantGuard middleware MUST 404
# any request whose X-Molecule-Org-Id (or Fly-Replay state, or
# same-origin Canvas trust) doesn't match the tenant container's
# configured MOLECULE_ORG_ID.
#
# Why this matters in production:
#   - One Cloudflare tunnel front-doors every tenant subdomain.
#   - DNS/routing layer can mis-direct a request (CF cache poisoning,
#     misconfigured CNAME, internal traffic mirror).
#   - TenantGuard is the last-line defense — it 404s any request whose
#     declared org doesn't match what the tenant binary was provisioned
#     with. Returning 404 (not 403) is intentional: the existence of a
#     tenant on this machine must not be probable by an outsider.
#
# What this replay catches:
#   - A regression where TenantGuard accidentally allows requests with
#     a different org id (e.g. someone removes the strict equality check).
#   - cf-proxy routing-by-Host bug that sends alpha's request to beta's
#     container (the negative test would suddenly succeed).
#   - Allowlist drift — if /workspaces is added to tenantGuardAllowlist
#     it would silently be cross-tenant readable.
#
# Phases:
#   A. Positive controls — each tenant accepts its own valid creds.
#   B. Org-header mismatch — alpha-org header at beta's URL → 404.
#   C. Reverse — beta-org header at alpha's URL → 404.
#   D. Right URL, wrong org header (typo) → 404.
#   E. Bearer present but no org header → 404 (TenantGuard rejects).
#   F. Per-tenant DB isolation — alpha's /workspaces enumerates only
#      alpha workspaces; beta's only beta. Confirms cf-proxy + TenantGuard
#      really did partition the request to the right backing DB.
#   G. Allowlisted /health stays public on both tenants (sanity check —
#      a regression that put /health behind the guard would 404 too).

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
cd "$HARNESS_ROOT"

if [ ! -f .seed.env ]; then
    echo "[replay] no .seed.env — running ./seed.sh first..."
    ./seed.sh
fi
# shellcheck source=/dev/null
source .seed.env
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

PASS=0
FAIL=0

assert_status() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        printf "  PASS %s (HTTP %s)\n" "$desc" "$actual"
        PASS=$((PASS + 1))
    else
        printf "  FAIL %s\n    expected HTTP %s, got HTTP %s\n" "$desc" "$expected" "$actual" >&2
        FAIL=$((FAIL + 1))
    fi
}

# ─── Phase A: positive controls ────────────────────────────────────────
echo "[replay] A. positive controls — each tenant accepts its own valid creds"

ALPHA_OWN=$(curl_alpha_admin -o /dev/null -w '%{http_code}' "$BASE/workspaces")
assert_status "A1: alpha creds at alpha returns 200" "200" "$ALPHA_OWN"

BETA_OWN=$(curl_beta_admin -o /dev/null -w '%{http_code}' "$BASE/workspaces")
assert_status "A2: beta creds at beta returns 200" "200" "$BETA_OWN"

# ─── Phase B: alpha creds at beta's URL → 404 ──────────────────────────
echo ""
echo "[replay] B. alpha-org header at beta's URL — TenantGuard must 404"

CROSS_AB=$(curl_alpha_creds_at_beta -o /tmp/iso-ab.json -w '%{http_code}' "$BASE/workspaces")
assert_status "B1: alpha-org header at beta URL → 404" "404" "$CROSS_AB"

# Body must be a generic 404 — never reveal that beta exists or that
# the org check fired (TenantGuard is intentionally indistinguishable
# from "no such route" to an outside scanner).
B_BODY=$(cat /tmp/iso-ab.json)
if echo "$B_BODY" | grep -qiE "tenant|org|forbidden|denied"; then
    printf "  FAIL B2: 404 body leaks tenant/org/auth keywords (info disclosure)\n    body: %s\n" "$B_BODY" >&2
    FAIL=$((FAIL + 1))
else
    printf "  PASS B2: 404 body has no tenant/org leak\n"
    PASS=$((PASS + 1))
fi

# ─── Phase C: beta creds at alpha's URL → 404 ──────────────────────────
echo ""
echo "[replay] C. beta-org header at alpha's URL — TenantGuard must 404"

CROSS_BA=$(curl_beta_creds_at_alpha -o /tmp/iso-ba.json -w '%{http_code}' "$BASE/workspaces")
assert_status "C1: beta-org header at alpha URL → 404" "404" "$CROSS_BA"

# ─── Phase D: right URL, garbage org header ────────────────────────────
echo ""
echo "[replay] D. right URL, garbage org header → 404"

GARBAGE=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Host: ${ALPHA_HOST}" \
    -H "Authorization: Bearer ${ALPHA_ADMIN_TOKEN}" \
    -H "X-Molecule-Org-Id: not-the-right-org" \
    "$BASE/workspaces")
assert_status "D1: garbage org id at alpha URL → 404" "404" "$GARBAGE"

# ─── Phase E: bearer present but no org header at all → 404 ────────────
echo ""
echo "[replay] E. valid bearer but missing X-Molecule-Org-Id → 404"

NO_ORG=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Host: ${ALPHA_HOST}" \
    -H "Authorization: Bearer ${ALPHA_ADMIN_TOKEN}" \
    "$BASE/workspaces")
assert_status "E1: missing X-Molecule-Org-Id → 404" "404" "$NO_ORG"

# ─── Phase F: per-tenant DB isolation via list_workspaces ──────────────
echo ""
echo "[replay] F. per-tenant DB isolation via /workspaces listing"

ALPHA_LIST=$(curl_alpha_admin "$BASE/workspaces")
ALPHA_NAMES=$(echo "$ALPHA_LIST" | jq -r '.[].name' | sort | tr '\n' ',' | sed 's/,$//')
echo "[replay]   alpha tenant sees: $ALPHA_NAMES"

if [ "$ALPHA_NAMES" = "alpha-child,alpha-parent" ]; then
    printf "  PASS F1: alpha enumerates only alpha workspaces\n"
    PASS=$((PASS + 1))
else
    printf "  FAIL F1: alpha enumerated unexpected workspaces\n    expected: alpha-child,alpha-parent\n    got     : %s\n" "$ALPHA_NAMES" >&2
    FAIL=$((FAIL + 1))
fi

BETA_LIST=$(curl_beta_admin "$BASE/workspaces")
BETA_NAMES=$(echo "$BETA_LIST" | jq -r '.[].name' | sort | tr '\n' ',' | sed 's/,$//')
echo "[replay]   beta tenant sees:  $BETA_NAMES"

if [ "$BETA_NAMES" = "beta-child,beta-parent" ]; then
    printf "  PASS F2: beta enumerates only beta workspaces\n"
    PASS=$((PASS + 1))
else
    printf "  FAIL F2: beta enumerated unexpected workspaces\n    expected: beta-child,beta-parent\n    got     : %s\n" "$BETA_NAMES" >&2
    FAIL=$((FAIL + 1))
fi

# Cross-check: neither tenant's list contains the other's workspace ids.
LEAKED_INTO_ALPHA=$(echo "$ALPHA_LIST" | jq -r --arg b1 "$BETA_PARENT_ID" --arg b2 "$BETA_CHILD_ID" \
    '[.[] | select(.id == $b1 or .id == $b2)] | length')
assert_status "F3: alpha list contains zero beta workspace ids" "0" "$LEAKED_INTO_ALPHA"

LEAKED_INTO_BETA=$(echo "$BETA_LIST" | jq -r --arg a1 "$ALPHA_PARENT_ID" --arg a2 "$ALPHA_CHILD_ID" \
    '[.[] | select(.id == $a1 or .id == $a2)] | length')
assert_status "F4: beta list contains zero alpha workspace ids" "0" "$LEAKED_INTO_BETA"

# ─── Phase G: /health is allowlisted (sanity) ──────────────────────────
echo ""
echo "[replay] G. /health stays public on both tenants (TenantGuard allowlist sanity)"

ALPHA_HEALTH=$(curl -sS -o /dev/null -w '%{http_code}' -H "Host: ${ALPHA_HOST}" "$BASE/health")
assert_status "G1: alpha /health public → 200" "200" "$ALPHA_HEALTH"

BETA_HEALTH=$(curl -sS -o /dev/null -w '%{http_code}' -H "Host: ${BETA_HOST}" "$BASE/health")
assert_status "G2: beta /health public → 200" "200" "$BETA_HEALTH"

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "[replay] FAIL: $PASS pass, $FAIL fail"
    exit 1
fi
echo "[replay] PASS: $PASS/$PASS — TenantGuard isolation + per-tenant DB partitioning hold"
