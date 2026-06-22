#!/usr/bin/env bash
# Collision-proof slug SUFFIX generator for staging E2E harnesses (core#2782).
#
# ROOT CAUSE (Researcher RCA #100639): staging Platform Boot fails at
# POST /cp/admin/orgs HTTP 409 because the harness creates platform
# orgs with COLLIDING slugs against stale tenant state. The prior
# head -c 32 truncation in test_staging_full_saas.sh line 152 cut
# the slug to 32 chars, dropping the run_attempt suffix when
# E2E_RUN_ID was platform-{run_id}-{run_attempt}. Two runs
# (e.g. run_id 3606 attempt 1 + 3606 attempt 2, OR two parallel
# jobs on the same day) produced the same truncated slug, hence 409.
#
# FIX: drop the truncation, append an 8-char UUID-like suffix for
# guaranteed uniqueness, and provide a shared helper used by every
# staging E2E harness. The infra purge of existing stale slugs is
# a separate owner/ops action (out of scope here per the ticket).
#
# Usage: the literal prefix MUST be in the caller so
# lint_cleanup_traps.sh can verify the SLUG= assignment starts with
# a covered e2e-* or rt-e2e-* prefix (see #11510).
#
#   source tests/e2e/lib/collision-proof-slug.sh
#   SLUG="e2e-smoke-$(make_collision_proof_slug_suffix "$E2E_RUN_ID" 11)"
#   assert_collision_proof_slug "$SLUG" || fail "..."
#
# The returned suffix is <date>-<sanitized_run_id>-<uuid>. The 8-char
# uuid is sourced from /proc/sys/kernel/random/uuid on Linux, fallback
# to two RANDOM draws on macOS. 32 bits of entropy is enough to
# defeat the original collision class.
#
# Asserts the full slug is collision-proof via assert_collision_proof_slug.
# Use this in the per-test self-check so a future refactor that
# drops the uuid is caught at harness startup, not at the first 409.
#
# core#60: the FULL slug must also fit in CP_ORG_SLUG_MAX_LEN (31 chars,
# the CP's org-slug cap; the org-create endpoint rejects longer slugs
# with HTTP 400, which under CURL_COMMON's --fail-with-body + set -e
# aborts the harness before the body-logging line can run). The
# helper truncates the run_id segment (NOT the uuid anchor) so the
# collision-proof guarantee is preserved.

set -uo pipefail

# CP_ORG_SLUG_MAX_LEN is the CP's org-slug character cap (regex
# ^[a-z][a-z0-9-]{2,31}$: a leading char plus 2-31 additional =
# 32-char absolute max). The org-create endpoint rejects
# longer slugs with HTTP 400 in practice per the staging 400s
# in run 363934, core#60. The #65 e2e-peer-visibility lane
# hit a 33-char slug like `e2e-pv-20260614-364043-2-e560b630`
# that needed this exact cap to keep the prefix+uuid+date
# layout below the regex's 32-char ceiling.
: "${CP_ORG_SLUG_MAX_LEN:=32}"

# make_collision_proof_slug_suffix <run_id> [prefix_len]
#   1: Run id (typically E2E_RUN_ID from the workflow; falls back
#      to a wall-clock+PID value when empty).
#   2: Optional length of the caller's literal prefix in the
#      SLUG=... assignment. When supplied, the suffix budget is
#      computed precisely (CP_ORG_SLUG_MAX_LEN - prefix_len - 19,
#      where 19 = 1 separator + 8 date + 1 separator + 1 separator
#      + 8 uuid). When omitted, the helper uses a conservative
#      default of 11 (the "e2e-smoke-" prefix length).
#
#   Echoes a collision-proof SUFFIX of the form
#   <date>-<sanitized_run_id>-<uuid>, lowercased, with non-
#   alphanumerics stripped (except -). The 8-char uuid is ALWAYS
#   preserved at the END of the suffix; the prefix (date + run_id)
#   is truncated if needed to fit CP_ORG_SLUG_MAX_LEN.
make_collision_proof_slug_suffix() {
  local run_id="${1:-}"
  local prefix_len="${2:-11}"

  if [ -z "$run_id" ]; then
    run_id="$(date +%H%M%S)-$$"
  fi

  local date_part
  date_part="$(date +%Y%m%d)"

  local uuid_short
  if [ -r /proc/sys/kernel/random/uuid ]; then
    uuid_short="$(cat /proc/sys/kernel/random/uuid | tr -d '-' | head -c 8)"
  else
    uuid_short="$(printf '%04x%04x' $RANDOM $RANDOM)"
  fi

  # Suffix layout: <date:8> + - + <run_id:N> + - + <uuid:8> = N+18 chars.
  # The caller's literal prefix already includes its trailing separator,
  # so the full slug is <prefix_len> + (N+18) = prefix_len + N + 18.
  # Cap: prefix_len + N + 18 <= CP_ORG_SLUG_MAX_LEN
  #      => N <= CP_ORG_SLUG_MAX_LEN - prefix_len - 18
  local run_id_budget=$(( CP_ORG_SLUG_MAX_LEN - prefix_len - 18 ))
  if [ "$run_id_budget" -lt 1 ]; then
    echo "make_collision_proof_slug_suffix: caller prefix (${prefix_len} chars) too long for CP_ORG_SLUG_MAX_LEN=${CP_ORG_SLUG_MAX_LEN}; date (8 chars) + uuid anchor (8 chars) + 2 separators = 18 chars minimum after the prefix, no room for run_id segment. Shorten the prefix literal in the SLUG= assignment." >&2
    return 1
  fi

  local sanitized_run_id
  sanitized_run_id="$(printf '%s' "$run_id" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c "$run_id_budget")"
  printf '%s-%s-%s' "$date_part" "$sanitized_run_id" "$uuid_short"
}

# assert_collision_proof_slug <slug> asserts the FULL slug ends in
# an 8-char uuid suffix AND fits in CP_ORG_SLUG_MAX_LEN. Use this
# in the per-test self-check so a future refactor that drops the
# uuid OR exceeds the CP cap is caught at harness startup, not at
# the first 400/409.
assert_collision_proof_slug() {
  local slug="$1"
  if ! printf '%s' "$slug" | grep -qE -- '-[0-9a-f]{8}$'; then
    echo "FAIL: slug '$slug' is not collision-proof, missing 8-char hex uuid suffix at end" >&2
    return 1
  fi
  if [ "${#slug}" -lt 24 ]; then
    echo "FAIL: slug '$slug' is too short to be collision-proof, len=${#slug} want >=24" >&2
    return 1
  fi
  if [ "${#slug}" -gt "${CP_ORG_SLUG_MAX_LEN}" ]; then
    echo "FAIL: slug '$slug' is too long, len=${#slug} max=${CP_ORG_SLUG_MAX_LEN}, CP /cp/admin/orgs rejects with HTTP 400" >&2
    return 1
  fi
  return 0
}
