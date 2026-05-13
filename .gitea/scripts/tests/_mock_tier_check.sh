#!/usr/bin/env bash
# Mock sop-tier-check.sh for sop-tier-refire tests.
#
# Exits 0 ("PASS") if $MOCK_TIER_RESULT == "pass", else exits 1.
# This lets the refire tests cover the success + failure status-POST
# paths without invoking the real sop-tier-check.sh (which uses bash 4+
# associative arrays — known parser bug on macOS bash 3.2 dev box).

set -euo pipefail

case "${MOCK_TIER_RESULT:-pass}" in
  pass)
    echo "::notice::mock tier-check: PASS"
    exit 0
    ;;
  fail_no_label)
    echo "::error::mock tier-check: no tier label"
    exit 1
    ;;
  fail_no_approvals)
    echo "::error::mock tier-check: no approving reviews"
    exit 1
    ;;
  *)
    echo "::error::mock tier-check: unknown MOCK_TIER_RESULT=${MOCK_TIER_RESULT:-}"
    exit 2
    ;;
esac
