#!/usr/bin/env bash
# Drift-prevention guard: SEV #2499 class (KI-013 container/volume naming).
#
# KI-013 removed 12-char UUID truncation from container/volume names.
# E2E scripts and provisioning shells must use FULL workspace IDs when
# referencing containers and volumes. ANY truncation of a workspace ID in
# a container/volume naming context is a regression risk.
#
# Catches:
#   - Bash substring truncation: ${VAR:0:N} (any N)
#   - cut truncation: cut -c1-N or cut -c 1-N
#   - awk substr truncation: substr(...,1,N)
#   - Any other :0:N pattern
#
# Only flags lines that also touch ws-* / docker / container / volume context,
# so legitimate truncation (e.g. SHA short hashes) is not falsely rejected.
#
# Scans ALL .sh files under tests/e2e/ and .gitea/scripts/.
# Run: bash .gitea/scripts/lint-e2e-ki013-container-names.sh
set -euo pipefail

ERR=0

# Patterns that truncate a string to N characters (N = 1-19).
# We allow N >= 20 because that's unlikely to be a workspace-ID truncation.
TRUNC_PATS=(
  # Bash ${VAR:0:N}
  '\$\{[A-Za-z_][A-Za-z0-9_]*:0:[1-9][0-9]?\}'
  # cut -c1-N or cut -c 1-N
  'cut[[:space:]]+-c[[:space:]]*1-[1-9][0-9]?'
  'cut[[:space:]]+-c1-[1-9][0-9]?'
  # awk substr($0,1,N) or substr(var,1,N)
  'substr\([^,]+,1,[1-9][0-9]?\)'
)

# Context keywords: if a line matches a truncation pattern AND contains one of
# these, it's flagged. This avoids false positives on unrelated truncation
# (e.g. git short SHAs, timestamp formatting).
CONTEXT_RE='ws-|docker|container|volume|DOCKER|CONTAINER|VOLUME'

check_file() {
  local f="$1"
  local line_num=0
  while IFS= read -r line; do
    line_num=$((line_num + 1))
    # Only inspect lines that touch container/volume/workspace naming.
    if ! echo "$line" | grep -qE "$CONTEXT_RE"; then
      continue
    fi
    for pat in "${TRUNC_PATS[@]}"; do
      if echo "$line" | grep -qE "$pat"; then
        echo "::error::SEV-2499 drift guard: possible workspace-ID truncation in container/volume name"
        echo "::error::file=$f,line=$line_num"
        echo "::error::  $line"
        ERR=1
        break  # one error per line is enough
      fi
    done
  done < "$f"
}

# Scan e2e scripts and provisioning scripts.
while IFS= read -r -d '' f; do
  check_file "$f"
done < <(find tests/e2e .gitea/scripts -type f -name '*.sh' -print0)

if [ "$ERR" -ne 0 ]; then
  echo ""
  echo "FAIL: Workspace-ID truncation detected in container/volume naming context."
  echo "      KI-013 requires FULL workspace IDs. See SEV #2499 for RCA."
  echo "      Update the flagged lines to use the complete ID."
  exit 1
fi

echo "PASS: No workspace-ID truncation in container/volume naming context."
