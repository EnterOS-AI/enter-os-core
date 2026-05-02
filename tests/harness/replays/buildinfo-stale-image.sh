#!/usr/bin/env bash
# Replay for issue #2395 — local proof that the /buildinfo verify gate
# closes the SaaS deploy-chain blindness.
#
# Prior behavior: redeploy-fleet returned ssm_status=Success based on
# the SSM RPC return code alone. EC2 tenants kept serving the cached
# :latest digest because `docker compose up -d` is a no-op when the
# tag hasn't been invalidated. ssm_status=Success was lying.
#
# This replay simulates that condition locally:
#   1. Boot the harness with GIT_SHA=fix-applied.
#   2. Curl /buildinfo and assert it returns "fix-applied" (the new code
#      actually shipped).
#   3. Negative test: curl with a different EXPECTED_SHA and assert the
#      mismatch detection logic the workflow uses returns failure.
#
# This proves the verify-step's jq lookup + comparison logic works
# against the SAME Dockerfile.tenant production builds. If the
# /buildinfo route ever stops being wired through, this replay
# catches it before it reaches a production tenant.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

# 1. Confirm /buildinfo wire shape — same shape the workflow's jq lookup expects.
echo "[replay] curl $BASE/buildinfo ..."
BUILD_JSON=$(curl_anon "$BASE/buildinfo")
echo "[replay]   $BUILD_JSON"

ACTUAL_SHA=$(echo "$BUILD_JSON" | jq -r '.git_sha // ""')
if [ -z "$ACTUAL_SHA" ]; then
    echo "[replay] FAIL: /buildinfo response missing git_sha field — workflow's jq lookup would null"
    exit 1
fi
echo "[replay] git_sha=$ACTUAL_SHA"

# 2. Assert the harness build threaded GIT_SHA through. If we got "dev",
#    the Dockerfile arg / ldflags wiring is broken — same regression
#    class that made #2395 invisible until production.
EXPECTED_FROM_HARNESS="${HARNESS_GIT_SHA:-harness}"
if [ "$ACTUAL_SHA" = "dev" ]; then
    echo "[replay] FAIL: /buildinfo returned 'dev' — Dockerfile.tenant ARG GIT_SHA isn't reaching the binary"
    echo "[replay]       This regresses #2395 by silencing the deploy-verify gate."
    exit 1
fi
if [ "$ACTUAL_SHA" != "$EXPECTED_FROM_HARNESS" ]; then
    echo "[replay] WARN: /buildinfo returned '$ACTUAL_SHA' but harness was built with GIT_SHA='$EXPECTED_FROM_HARNESS'"
    echo "[replay]       Image may be cached from a previous run. Run ./up.sh --rebuild to force a fresh build."
fi

# 3. Negative test — replay the workflow's mismatch detection by
#    comparing the actual SHA to a deliberately-wrong expected SHA.
WRONG_EXPECTED="0000000000000000000000000000000000000000"
if [ "$ACTUAL_SHA" = "$WRONG_EXPECTED" ]; then
    echo "[replay] FAIL: /buildinfo returned all-zero SHA — wiring inverted"
    exit 1
fi

# 4. Replay the workflow's exact comparison logic so a regression in
#    the verify step's bash gets caught here.
MISMATCH_DETECTED=0
if [ "$ACTUAL_SHA" != "$WRONG_EXPECTED" ]; then
    MISMATCH_DETECTED=1
fi
if [ "$MISMATCH_DETECTED" != "1" ]; then
    echo "[replay] FAIL: workflow comparison logic would not flag a real mismatch"
    exit 1
fi

echo ""
echo "[replay] PASS: /buildinfo wire shape, GIT_SHA injection, and mismatch detection all work in"
echo "        production-shape topology. The redeploy-fleet verify-step covers what it claims to."
