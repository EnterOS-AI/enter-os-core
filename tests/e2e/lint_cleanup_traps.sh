#!/usr/bin/env bash
# lint_cleanup_traps.sh — regression gate for the OSS-shape program's
# "all E2E tests must have proper cleanup" bar (RFC #2873).
#
# Asserts: every shell file under tests/e2e/ that calls `mktemp` ALSO
# installs an `EXIT` trap somewhere in the file. The trap is the
# minimum-viable guarantee that scratch files won't leak when an
# assertion or curl exits the script non-zero.
#
# Why this lints (instead of the test runner enforcing): shell scripts
# can't easily be wrapped by an outer harness without breaking the
# `WSID=… ./test_x.sh` invocation contract. Static gate is the cheap
# defense.
#
# Usage:
#   tests/e2e/lint_cleanup_traps.sh
#
# Exits non-zero if any test_*.sh has unmatched mktemp/trap. CI invokes
# it from the existing Shellcheck (E2E scripts) workflow.

set -euo pipefail

cd "$(dirname "$0")"

violations=0
for f in test_*.sh; do
  if grep -qE '\bmktemp\b' "$f"; then
    if ! grep -qE 'trap[[:space:]]+.*EXIT' "$f"; then
      echo "::error file=tests/e2e/$f::has 'mktemp' but no 'trap … EXIT' — scratch will leak when test exits non-zero. Pattern: TMPDIR_E2E=\$(mktemp -d -t prefix-XXX); trap 'rm -rf \"\$TMPDIR_E2E\"' EXIT INT TERM"
      violations=$((violations + 1))
    fi
  fi
done

if [ "$violations" -gt 0 ]; then
  echo "::error::$violations shell E2E file(s) leak scratch on early exit. See above."
  exit 1
fi

echo "✓ all $(grep -lE '\bmktemp\b' test_*.sh | wc -l | tr -d ' ') shell E2E files with mktemp also install an EXIT trap"
