#!/usr/bin/env bash
# Regression test for scripts/ops/prune_cf_e2e_dns.sh — verifies fail-closed
# behavior for Cloudflare API errors and record-selection safety.
#
# Tests:
#   1. Non-2xx CF DNS list response aborts before any delete attempt.
#   2. Malformed JSON CF DNS list response aborts before any delete attempt.
#   3. CF DNS list result that is not an array aborts before any delete attempt.
#   4. A record matching the e2e-smoke-* pattern but younger than min-age is kept.
#   5. A non-ephemeral record (api.moleculesai.app) older than min-age is kept.
#   6. Happy path: an old e2e-smoke-* record is deleted (sentinel reached).
set -uo pipefail

SCRIPT="${SCRIPT:-scripts/ops/prune_cf_e2e_dns.sh}"

PASS=0
FAIL=0

run_case() {
  local name="$1" list_exit="$2" list_body="$3" expect_delete_sentinel="$4" zone_domain="${5:-}"
  local tmp
  tmp=$(mktemp -d -t cf-e2e-prune-fail-closed-XXXXXX)
  local delete_sentinel="$tmp/delete_reached"

  # URL-aware curl mock. CF token/zone preflight always succeeds. CF DNS list
  # endpoint receives the controlled response. CF DNS delete endpoint writes a
  # sentinel if reached.
  cat > "$tmp/curl" <<'MOCK'
#!/usr/bin/env bash
url=""
method="GET"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift ;;
    https://*) url="$1" ;;
  esac
  shift
done
case "$url" in
  */user/tokens/verify)
    echo '{"success":true,"result":{"status":"active"}}'
    exit 0
    ;;
  */zones/*/dns_records*)
    if [ "$method" = "DELETE" ]; then
      echo 'reached' > "$DELETE_SENTINEL"
      echo '{"success":true,"result":{"id":"deleted"}}'
      exit 0
    fi
    __LIST_BODY__
    exit __LIST_EXIT__
    ;;
  */zones/*)
    echo '{"success":true,"result":{"id":"zone"}}'
    exit 0
    ;;
  *)
    echo 'reached' > "$DELETE_SENTINEL"
    echo '{"success":true,"result":{"id":"deleted"}}'
    exit 0
    ;;
esac
MOCK
  printf '%s\n' "$list_body" > "$tmp/list_body.txt"
  sed -i "s|__LIST_BODY__|cat \"\$LIST_BODY_FILE\"|g; s|__LIST_EXIT__|$list_exit|g" "$tmp/curl"
  chmod +x "$tmp/curl"

  local out="$tmp/out" err="$tmp/err"
  # Export paths so the mock script can find the list body file and sentinel.
  export DELETE_SENTINEL="$delete_sentinel"
  export LIST_BODY_FILE="$tmp/list_body.txt"
  # Allow the single-record happy-path case to delete 100% of matched records.
  export MAX_DELETE_PCT=100
  PATH="$tmp:$PATH" \
    CF_API_TOKEN=tok \
    CF_ZONE_ID=zone \
    PRUNE_MIN_AGE_HOURS=1 \
    PRUNE_ZONE_DOMAIN="$zone_domain" \
    bash "$SCRIPT" --apply > "$out" 2> "$err"
  local actual_exit=$?
  local case_fail=0

  if [ "$expect_delete_sentinel" = "true" ]; then
    # Happy path: script must reach delete and exit 0.
    if [ ! -f "$delete_sentinel" ]; then
      echo "  ✗ $name: delete sentinel missing — prune did not reach delete step" >&2
      case_fail=1
    fi
    if [ "$actual_exit" -ne 0 ]; then
      echo "  ✗ $name: expected exit 0, got $actual_exit" >&2
      case_fail=1
    fi
  else
    # Fail-closed / keep cases: delete sentinel must NOT be written.
    if [ -f "$delete_sentinel" ]; then
      echo "  ✗ $name: delete sentinel exists — prune reached delete step unexpectedly" >&2
      case_fail=1
    fi
    if [ "$expect_delete_sentinel" = "false-abort" ] && [ "$actual_exit" -eq 0 ]; then
      echo "  ✗ $name: expected non-zero exit for abort case, got 0" >&2
      case_fail=1
    fi
    if [ "$expect_delete_sentinel" = "false-keep" ] && [ "$actual_exit" -ne 0 ]; then
      echo "  ✗ $name: expected exit 0 for keep case, got $actual_exit" >&2
      case_fail=1
    fi
  fi

  if [ "$case_fail" -eq 0 ]; then
    echo "  ✓ $name"
    PASS=$((PASS + 1))
  else
    echo "    stdout:" >&2
    sed 's/^/      /' "$out" >&2
    echo "    stderr:" >&2
    sed 's/^/      /' "$err" >&2
    FAIL=$((FAIL + 1))
  fi

  rm -rf "$tmp"
}

echo "Test: prune_cf_e2e_dns fail-closed boundary"
echo

# Bad CF list responses must abort before delete.
run_case "CF DNS list returns 500"            55  '{"success":false,"errors":[{"code":1000}]}'  false-abort
run_case "CF DNS list returns malformed JSON"  0   'this is not json'                              false-abort
run_case "CF DNS list returns non-array result" 0 '{"success":true,"result":{"id":"rec1"}}'      false-abort

# Helper to build a DNS list result with one record, given created_on ISO string
# and optional zone domain (default: staging.moleculesai.app, the observed
# domain for leaked e2e-smoke/e2e-tmpl records).
make_list() {
  local created_on="$1" zone_domain="${2:-staging.moleculesai.app}"
  cat <<JSON
{"success":true,"result":[{"id":"rec1","name":"e2e-smoke-20260622-1234-abcdef12.${zone_domain}","type":"A","created_on":"$created_on"}],"result_info":{"page":1,"total_pages":1,"per_page":100,"count":1}}
JSON
}

old_ts=$(python3 -c "from datetime import datetime,timezone,timedelta; print((datetime.now(timezone.utc)-timedelta(hours=2)).isoformat().replace('+00:00','Z'))")

# Too-new record must be kept, not deleted.
new_ts=$(python3 -c "from datetime import datetime,timezone,timedelta; print((datetime.now(timezone.utc)-timedelta(minutes=5)).isoformat().replace('+00:00','Z'))")
run_case "e2e-smoke record too new" 0 "$(make_list "$new_ts")" false-keep

# Non-ephemeral old record must be kept.
run_case "non-ephemeral old record kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/api/')" false-keep

# Near-miss names without the required hyphen must be kept (CR2 safety blocker).
run_case "e2e-smokeprod (no hyphen) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-smokeprod/')" false-keep
run_case "e2e-tmplprod (no hyphen) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-tmplprod/')" false-keep
run_case "e2e-smoketest-keep (extra chars before hyphen) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-smoketest-keep/')" false-keep
run_case "e2e-tmplate-keep (extra chars before hyphen) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-tmplate-keep/')" false-keep
run_case "e2e-smoke (no hyphen suffix) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-smoke/')" false-keep
run_case "prod-e2e-smoke-x (does not start with prefix) kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/prod-e2e-smoke-x/')" false-keep

# Zone-domain coverage (Researcher RC 13130 correctness blocker).
# Default PRUNE_ZONE_DOMAIN is staging.moleculesai.app, matching observed leaks.
run_case "old e2e-smoke staging subdomain deleted (default)" 0 "$(make_list "$old_ts")" true

# Apex domain is NOT matched when PRUNE_ZONE_DOMAIN is staging only.
run_case "apex e2e-smoke kept when staging-only" 0 "$(make_list "$old_ts" moleculesai.app)" false-keep staging.moleculesai.app

# A record under a different subdomain is NOT matched.
run_case "dev-subdomain e2e-smoke kept" 0 "$(make_list "$old_ts" dev.moleculesai.app)" false-keep staging.moleculesai.app

# Explicit apex zone domain still works when requested.
run_case "old e2e-smoke apex domain deleted" 0 "$(make_list "$old_ts" moleculesai.app)" true moleculesai.app

# Comma-separated zone domains match both apex and staging.
run_case "multi-zone matches staging record" 0 "$(make_list "$old_ts")" true "moleculesai.app,staging.moleculesai.app"
run_case "multi-zone matches apex record" 0 "$(make_list "$old_ts" moleculesai.app)" true "moleculesai.app,staging.moleculesai.app"

# Near-miss under the staging zone is still kept (safety guard).
run_case "e2e-smoketest-keep under staging kept" 0 "$(make_list "$old_ts" | sed 's/e2e-smoke-20260622-1234-abcdef12/e2e-smoketest-keep/')" false-keep
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
