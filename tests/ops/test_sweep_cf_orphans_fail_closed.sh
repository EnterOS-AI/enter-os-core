#!/usr/bin/env bash
# Regression test for scripts/ops/sweep-cf-orphans.sh — verifies the
# live-org fetch is fail-closed. A non-2xx response, invalid JSON, or a
# response missing the 'orgs' array must abort the sweep BEFORE any
# Cloudflare DNS records are listed or classified as orphans.
set -uo pipefail

SCRIPT="${SCRIPT:-scripts/ops/sweep-cf-orphans.sh}"

PASS=0
FAIL=0

run_case() {
  local name="$1" cp_exit="$2" cp_body="$3"
  local expect_abort="${4:-true}"   # true = must stop before AWS/CF boundary
  local tmp
  tmp=$(mktemp -d -t cf-orphans-fail-closed-XXXXXX)

  # Generate a URL-aware curl mock. CF token/zone preflight and the CF DNS
  # list must return valid JSON so the test can prove a bad CP orgs response
  # aborts at the live-org fetch boundary, not during preflight or after
  # reaching AWS/CF classification.
  cat > "$tmp/curl" <<'MOCK'
#!/usr/bin/env bash
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
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
    echo '{"success":true,"result":[{"id":"rec1","name":"api.moleculesai.app","type":"A","created_on":"2026-06-20T00:00:00Z"}]}'
    echo 'reached' > "$CF_SENTINEL"
    exit 0
    ;;
  */zones/*)
    echo '{"success":true,"result":{"id":"zone"}}'
    exit 0
    ;;
  */cp/admin/orgs*)
    __CP_BODY__
    exit __CP_EXIT__
    ;;
  *)
    echo '{"success":true,"result":[]}'
    echo 'reached' > "$CF_SENTINEL"
    exit 0
    ;;
esac
MOCK
  # Substitute the test-case body and exit code. Use printf/sed to avoid
  # shell quoting issues with JSON in the heredoc.
  printf '%s\n' "$cp_body" > "$tmp/cp_body.txt"
  sed -i "s|__CP_BODY__|cat \"$tmp/cp_body.txt\"|g; s|__CP_EXIT__|$cp_exit|g" "$tmp/curl"
  chmod +x "$tmp/curl"

  # Mock aws cli: required by the script. Returns valid empty EC2 JSON in the
  # happy path; writes a sentinel if reached so fail-closed cases prove AWS
  # gather was not entered.
  cat > "$tmp/aws" <<'MOCK'
#!/usr/bin/env bash
echo "reached" > "$AWS_SENTINEL"
echo '{"Reservations":[]}'
exit 0
MOCK
  chmod +x "$tmp/aws"

  local out="$tmp/out" err="$tmp/err"
  PATH="$tmp:$PATH" \
    CF_API_TOKEN=tok \
    CF_ZONE_ID=zone \
    CP_ADMIN_API_TOKEN=tok-prod \
    CP_STAGING_ADMIN_API_TOKEN=tok-staging \
    AWS_ACCESS_KEY_ID=ak \
    AWS_SECRET_ACCESS_KEY=sk \
    CF_SENTINEL="$tmp/cf_reached" \
    AWS_SENTINEL="$tmp/aws_reached" \
    bash "$SCRIPT" --execute > "$out" 2> "$err"
  local actual_exit=$?
  local case_fail=0

  if [ "$expect_abort" = "true" ]; then
    # Fail-closed cases: script must abort at the CP live-org fetch,
    # before AWS EC2 gather or CF DNS list/classify/delete.
    if [ "$actual_exit" -eq 0 ]; then
      echo "  ✗ $name: exited 0 instead of aborting" >&2
      case_fail=1
    fi
    if [ -f "$tmp/cf_reached" ]; then
      echo "  ✗ $name: CF sentinel exists — sweep reached DNS list/classify" >&2
      case_fail=1
    fi
    if [ -f "$tmp/aws_reached" ]; then
      echo "  ✗ $name: AWS sentinel exists — sweep reached EC2 gather" >&2
      case_fail=1
    fi
    if grep -qE '== Sweep plan ==|would delete:|orphan-' "$out" "$err" 2>/dev/null; then
      echo "  ✗ $name: output contains sweep plan / orphan classification" >&2
      case_fail=1
    fi
  else
    # Happy-path control: valid empty orgs arrays must pass the fetch guard
    # and reach both AWS EC2 gather and Cloudflare DNS listing.
    if [ ! -f "$tmp/cf_reached" ]; then
      echo "  ✗ $name: CF sentinel missing — sweep did not reach DNS list" >&2
      case_fail=1
    fi
    if [ ! -f "$tmp/aws_reached" ]; then
      echo "  ✗ $name: AWS sentinel missing — sweep did not reach EC2 gather" >&2
      case_fail=1
    fi
    if [ "$actual_exit" -ne 0 ]; then
      echo "  ✗ $name: expected exit 0 after empty DNS list, got $actual_exit" >&2
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

echo "Test: sweep-cf-orphans live-org fetch fail-closed"
echo

run_case "prod API returns 500"                          22 '{"error":"internal"}'        true
run_case "prod API returns malformed JSON"               0  'this is not json'             true
run_case "prod API returns JSON without orgs"            0  '{"foo":"bar"}'                true
run_case "prod API returns orgs as string"               0  '{"orgs":"not-an-array"}'      true
run_case "prod API returns valid empty orgs (proceeds)"  0  '{"orgs":[]}'                  false

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
