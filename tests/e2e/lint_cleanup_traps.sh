#!/usr/bin/env bash
# lint_cleanup_traps.sh — regression gate for the OSS-shape program's
# "all E2E tests must have proper cleanup" bar (RFC #2873).
#
# Asserts:
#   1. every shell file under tests/e2e/ that calls `mktemp` ALSO
#      installs an `EXIT` trap somewhere in the file.
#   2. every staging tenant E2E script that provisions a real org uses a
#      slug prefix caught by sweep-stale-e2e-orgs.yml and installs an
#      EXIT trap.
#
# These are the minimum-viable guarantees that scratch files and real
# staging EC2 tenants converge back to zero when an assertion or curl
# exits the script non-zero.
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
repo_root="$(cd ../.. && pwd)"

violations=0
for f in test_*.sh; do
  if grep -qE '\bmktemp\b' "$f"; then
    if ! grep -qE 'trap[[:space:]]+.*EXIT' "$f"; then
      echo "::error file=tests/e2e/$f::has 'mktemp' but no 'trap … EXIT' — scratch will leak when test exits non-zero. Pattern: TMPDIR_E2E=\$(mktemp -d -t prefix-XXX); trap 'rm -rf \"\$TMPDIR_E2E\"' EXIT INT TERM"
      violations=$((violations + 1))
    fi
  fi
done

if ! python3 - "$repo_root" <<'PY'
import re
import sys
from pathlib import Path

repo = Path(sys.argv[1])
e2e_dir = repo / "tests" / "e2e"
sweeper = repo / ".gitea" / "workflows" / "sweep-stale-e2e-orgs.yml"

errors: list[str] = []
sweeper_text = sweeper.read_text()

required_sweeper_prefixes = ('"e2e-"', '"rt-e2e-"')
for prefix in required_sweeper_prefixes:
    if prefix not in sweeper_text:
        errors.append(
            f"::error file=.gitea/workflows/sweep-stale-e2e-orgs.yml::"
            f"missing stale-org sweeper prefix {prefix}"
        )

slug_assignment_re = re.compile(r'^\s*SLUG=(["\'])(?P<value>.+?)\1', re.MULTILINE)
covered_prefixes = ("e2e-", "rt-e2e-")

for path in sorted(e2e_dir.glob("test_*staging*.sh")):
    text = path.read_text()
    creates_org = "/cp/admin/orgs" in text and re.search(r"\bPOST\b", text)
    deletes_org = "/cp/admin/tenants" in text and re.search(r"\bDELETE\b", text)
    if not (creates_org or deletes_org):
        continue

    rel = path.relative_to(repo)
    if not re.search(r"trap\s+.*\bEXIT\b", text):
        errors.append(
            f"::error file={rel}::staging tenant E2E touches CP org lifecycle "
            "but has no EXIT trap for teardown"
        )

    assignments = [m.group("value") for m in slug_assignment_re.finditer(text)]
    if not assignments:
        errors.append(
            f"::error file={rel}::staging tenant E2E touches CP org lifecycle "
            "but has no quoted SLUG=... assignment for scoped cleanup"
        )
        continue

    for value in assignments:
        literal_prefix = re.split(r"[$`]", value, maxsplit=1)[0]
        if not literal_prefix:
            errors.append(
                f"::error file={rel}::SLUG assignment starts with dynamic data "
                f"({value!r}); use a fixed e2e-* or rt-e2e-* prefix so "
                "sweep-stale-e2e-orgs can reap orphans"
            )
            continue
        if not literal_prefix.startswith(covered_prefixes):
            errors.append(
                f"::error file={rel}::SLUG prefix {literal_prefix!r} is not "
                "covered by sweep-stale-e2e-orgs.yml; use e2e-* or rt-e2e-*"
            )

if errors:
    print("\n".join(errors))
    raise SystemExit(1)

print("✓ staging tenant E2E slug prefixes are covered by sweep-stale-e2e-orgs and use EXIT traps")
PY
then
  violations=$((violations + 1))
fi

if [ "$violations" -gt 0 ]; then
  echo "::error::$violations shell E2E file(s) leak scratch on early exit. See above."
  exit 1
fi

echo "✓ all $(grep -lE '\bmktemp\b' test_*.sh | wc -l | tr -d ' ') shell E2E files with mktemp also install an EXIT trap"
