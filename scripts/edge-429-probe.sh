#!/usr/bin/env bash
# edge-429-probe.sh — capture 429 origin (workspace-server vs CF/Vercel edge)
# during a simulated canvas-burst against a tenant subdomain.
#
# Issue molecule-core#62. The post-#60 verification step asks an
# operator with CF/Vercel dashboard access to confirm whether the
# layout-chunk 429s observed in DevTools were:
#   (a) workspace-server bucket overflow (closes once #60 deploys), or
#   (b) actual edge-layer rate-limiting (CF or Vercel).
#
# This script doesn't need dashboard access. It reproduces the burst
# pattern locally and dumps every 429's response shape so the operator
# can distinguish (a) from (b) by inspection: workspace-server emits a
# JSON body, CF emits HTML, Vercel emits a different HTML. Headers tell
# the same story (cf-ray vs x-vercel-*).
#
# Usage:
#   ./scripts/edge-429-probe.sh <tenant-host> [--burst N] [--waves N] [--pause SECS] [--out file]
#
# Example:
#   ./scripts/edge-429-probe.sh hongming.moleculesai.app --burst 80 --out /tmp/edge.txt
#
# The script is read-only against the target — it only issues GETs to
# public-by-design endpoints. No mutating requests, no credential use.

set -euo pipefail

# ── Help / usage handling first, before positional capture ────────────────────
case "${1:-}" in
  -h|--help|"")
    sed -n '/^# edge-429-probe.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
esac

HOST="$1"; shift
BURST=80
WAVES=3
WAVE_PAUSE=2
OUT=""

while [ "${1:-}" != "" ]; do
  case "$1" in
    --burst) BURST="$2"; shift 2 ;;
    --waves) WAVES="$2"; shift 2 ;;
    --pause) WAVE_PAUSE="$2"; shift 2 ;;
    --out)   OUT="$2";   shift 2 ;;
    -h|--help)
      sed -n '/^# edge-429-probe.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# ── Endpoint discovery ────────────────────────────────────────────────────────
echo "→ Discovering a layout-chunk URL from canvas root..." >&2
ROOT_BODY=$(curl -fsSL --max-time 10 "https://${HOST}/" 2>/dev/null || true)
LAYOUT_PATH=$(echo "$ROOT_BODY" \
  | grep -oE '/_next/static/chunks/layout-[A-Za-z0-9_-]+\.js' \
  | head -1 || true)
if [ -z "$LAYOUT_PATH" ]; then
  LAYOUT_PATH="/_next/static/chunks/layout-probe-not-found.js"
  echo "  (no layout chunk discovered — using sentinel path; 404 on this is expected)" >&2
else
  echo "  layout chunk: $LAYOUT_PATH" >&2
fi

# Probe URL: a generic activity endpoint. The rate-limiter middleware
# runs BEFORE workspace-id validation, so unauth/invalid-id requests
# still hit the bucket.
ACTIVITY_PATH="/workspaces/00000000-0000-0000-0000-000000000000/activity?probe=edge-429"

# ── Fire one curl, write a single-line JSON-ish status record to stdout ──────
# Inlined into xargs as a heredoc-style command rather than a function so
# the function-export pitfalls (some shells lose `export -f` across xargs)
# don't apply. Each output line is a parseable record; failed curls emit
# a curl_err record so request volume is preserved.
TMP_RESULTS="$(mktemp -t edge-429-probe.XXXXXX)"
trap 'rm -f "$TMP_RESULTS"' EXIT

run_burst() {
  # $1 = path; $2 = label; $3 = wave_id
  local path="$1" label="$2" wave="$3"
  local i
  for i in $(seq 1 "$BURST"); do
    {
      out=$(curl -sS --max-time 10 -o /dev/null \
        -w 'status=%{http_code} size=%{size_download} time=%{time_total} server=%{header.server} cf_ray=%{header.cf-ray} x_vercel=%{header.x-vercel-id} retry_after=%{header.retry-after} content_type=%{header.content-type} x_ratelimit_limit=%{header.x-ratelimit-limit} x_ratelimit_remaining=%{header.x-ratelimit-remaining} x_ratelimit_reset=%{header.x-ratelimit-reset}\n' \
        "https://${HOST}${path}" 2>/dev/null) || out="status=curl_err"
      printf 'label=%s-%s-%s %s\n' "$label" "$wave" "$i" "$out" >> "$TMP_RESULTS"
    } &
  done
  wait
}

emit() {
  if [ -n "$OUT" ]; then
    printf '%s\n' "$*" >> "$OUT"
  else
    printf '%s\n' "$*"
  fi
}

if [ -n "$OUT" ]; then : > "$OUT"; fi

emit "# edge-429-probe report"
emit "# host=$HOST burst=$BURST waves=$WAVES pause=${WAVE_PAUSE}s"
emit "# layout_path=$LAYOUT_PATH"
emit "# activity_path=$ACTIVITY_PATH"
emit "# generated=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
emit ""

for wave in $(seq 1 "$WAVES"); do
  emit "## wave $wave"
  : > "$TMP_RESULTS"
  run_burst "$LAYOUT_PATH" "layout" "$wave"
  run_burst "$ACTIVITY_PATH" "activity" "$wave"
  while read -r line; do
    emit "  $line"
  done < "$TMP_RESULTS"
  if [ "$wave" -lt "$WAVES" ]; then
    sleep "$WAVE_PAUSE"
  fi
done

emit ""
emit "## summary — how to read the report"
emit "#   status=429 + content_type starts with application/json + x_ratelimit_limit set"
emit "#     => workspace-server bucket overflow. Closes when #60 deploys."
emit "#   status=429 + cf_ray set + content_type=text/html"
emit "#     => Cloudflare WAF / rate-limit. Audit dashboard rules per #62."
emit "#   status=429 + x_vercel set + content_type=text/html"
emit "#     => Vercel edge / Bot Fight Mode. Audit Vercel project per #62."
emit "#   status=429 with no server/cf_ray/x_vercel"
emit "#     => corporate proxy or VPN. Not actionable in this repo."

if [ -n "$OUT" ]; then
  echo "→ Report written to $OUT" >&2
  # Match only data lines (begin with two-space indent + "label="),
  # not the summary's reference text which also mentions "status=429".
  # grep -c outputs "0" + exits 1 when zero matches; `|| true` masks
  # the exit status so set -e doesn't trip without losing the count.
  total=$(grep -c '^  label=' "$OUT" 2>/dev/null || true)
  total429=$(grep -c '^  label=.*status=429' "$OUT" 2>/dev/null || true)
  total=${total:-0}
  total429=${total429:-0}
  echo "→ Totals: ${total429} of ${total} requests returned 429" >&2
  if [ "${total429}" -gt 0 ]; then
    echo "→ Per-label 429 counts:" >&2
    grep '^  label=.*status=429' "$OUT" \
      | sed -E 's/^  label=([^-]+).*/  \1/' \
      | sort | uniq -c >&2
  fi
fi
