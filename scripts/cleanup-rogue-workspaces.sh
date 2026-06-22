#!/usr/bin/env bash
# cleanup-rogue-workspaces.sh — delete stale test workspaces matching
# well-known placeholder ID patterns (#17).
#
# Usage:
#   bash scripts/cleanup-rogue-workspaces.sh               # against http://localhost:8080
#   MOLECULE_URL=http://host:8080 bash scripts/cleanup-rogue-workspaces.sh
#
# Deletes any workspace whose id OR name starts with one of the
# hard-coded test prefixes. Also force-removes the matching Docker
# container (`ws-<id[:12]>`) in case Docker's unless-stopped policy
# kept it restarting on a missing config.yaml.
#
# Safe to run repeatedly — idempotent. Non-matching workspaces are left
# untouched.

set -euo pipefail

PLATFORM_URL="${MOLECULE_URL:-http://localhost:8080}"

echo "Scanning ${PLATFORM_URL}/workspaces for rogue test workspaces..."

payload="$(curl -sS --fail "${PLATFORM_URL}/workspaces" || true)"
if [[ -z "${payload}" ]]; then
  echo "  platform unreachable at ${PLATFORM_URL}"
  exit 1
fi

# Extract {id\tname} for workspaces whose id or name starts with a test prefix.
# Patterns are passed via env so the heredoc doesn't need shell interpolation.
matches="$(PAYLOAD="${payload}" python3 - <<'PY'
import json, os
data = json.loads(os.environ["PAYLOAD"])
rows = data if isinstance(data, list) else data.get("workspaces", [])
patterns = ("aaaaaaaa-", "bbbbbbbb-", "cccccccc-", "test-ws-")
for w in rows:
    wid = w.get("id", "") or ""
    name = w.get("name", "") or ""
    if any(wid.startswith(p) or name.startswith(p) for p in patterns):
        print(f"{wid}\t{name}")
PY
)"

if [[ -z "${matches}" ]]; then
  echo "  no rogue workspaces found"
  exit 0
fi

while IFS=$'\t' read -r wid wname; do
  [[ -z "${wid}" ]] && continue
  short="${wid:0:12}"
  echo "  deleting workspace ${wid} (${wname})"
  curl -sS -X DELETE "${PLATFORM_URL}/workspaces/${wid}" >/dev/null || true
  if command -v docker >/dev/null 2>&1; then
    docker rm -f "ws-${short}" >/dev/null 2>&1 || true
  fi
done <<<"${matches}"

echo "done."
