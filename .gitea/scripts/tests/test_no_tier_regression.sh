#!/usr/bin/env bash
set -euo pipefail
# Anti-regression gate for #2403: fail if any SOP tier artifact reappears.

cd "$(dirname "$0")/../../.."

fail=0

# 1. Deleted workflow files must stay deleted
for f in .gitea/workflows/sop-tier-check.yml .gitea/workflows/sop-tier-refire.yml; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (must stay deleted per #2403)" >&2
    fail=1
  fi
done

# 2. Deleted script files must stay deleted
for f in .gitea/scripts/sop-tier-check.sh .gitea/scripts/sop-tier-refire.sh; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (must stay deleted per #2403)" >&2
    fail=1
  fi
done

# 3. No tier branching logic in gate_check.py
if grep -qE '_get_pr_tier|TIER_AGENTS' tools/gate-check-v3/gate_check.py; then
  echo "FAIL: tier branching reappeared in gate_check.py" >&2
  fail=1
fi

# 4. No _is_tier_low_pending_ok in merge queue
if grep -q '_is_tier_low_pending_ok' .gitea/scripts/gitea-merge-queue.py; then
  echo "FAIL: tier soft-fail reappeared in gitea-merge-queue.py" >&2
  fail=1
fi

# 5. No sop-tier-check context references in workflow YAML
if grep -rI --exclude-dir='__pycache__' 'sop-tier-check' .gitea/workflows/; then
  echo "FAIL: sop-tier-check context reappeared in workflows" >&2
  fail=1
fi

# 6. No SOP_TIER_CHECK_TOKEN references in workflow YAML or scripts
if grep -rI --exclude-dir='__pycache__' --exclude='test_no_tier_regression.sh' 'SOP_TIER_CHECK_TOKEN' .gitea/workflows/ .gitea/scripts/; then
  echo "FAIL: SOP_TIER_CHECK_TOKEN reference reappeared (use SOP_CHECKLIST_GATE_TOKEN)" >&2
  fail=1
fi

# 7. qa-review and security-review must have labeled/unlabeled triggers (#2139)
for f in .gitea/workflows/qa-review.yml .gitea/workflows/security-review.yml; do
  if ! grep -q 'labeled, unlabeled' "$f"; then
    echo "FAIL: $f missing labeled/unlabeled triggers (#2139)" >&2
    fail=1
  fi
done

# 8. qa-review and security-review must NOT have review.state guard (#2159)
for f in .gitea/workflows/qa-review.yml .gitea/workflows/security-review.yml; do
  if grep -q 'github.event.review.state' "$f"; then
    echo "FAIL: $f has review.state guard reappeared (#2159)" >&2
    fail=1
  fi
done

if [ "$fail" -eq 1 ]; then
  echo "TIER_REGRESSION_DETECTED" >&2
  exit 1
fi

echo "PASS: no tier regression detected"
