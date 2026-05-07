#!/usr/bin/env bash
# tools/branch-protection/test_check_name_parity.sh — unit tests for
# check_name_parity.sh.
#
# Builds synthetic apply.sh + workflow files in a tmpdir for each case,
# invokes the script with REPO_ROOT pointing at the tmpdir, and asserts
# on exit code + stderr. Per feedback_assert_exact_not_substring we
# pin the EXACT exit code AND a substring of the stderr that names the
# offending workflow + name combo — so a "false-pass that prints the
# wrong message" still fails the test.
#
# Run locally: bash tools/branch-protection/test_check_name_parity.sh
# Run in CI:  same — added to ci.yml's shellcheck job's "E2E bash unit
#             tests" step alongside test_model_slug.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_UNDER_TEST="$SCRIPT_DIR/check_name_parity.sh"

if [[ ! -x "$SCRIPT_UNDER_TEST" ]]; then
  echo "test_check_name_parity: script under test missing or not executable: $SCRIPT_UNDER_TEST" >&2
  exit 2
fi

PASSED=0
FAILED=0

# Tracks the active tmpdir for the running case so the trap can clean
# up even when assertions abort the case mid-flight.
TMPDIR_FOR_CASE=""
trap '[[ -n "$TMPDIR_FOR_CASE" && -d "$TMPDIR_FOR_CASE" ]] && rm -rf "$TMPDIR_FOR_CASE"' EXIT

# Build a synthetic repo at $1 with apply.sh listing $2 (one name per
# line) as the staging required set + zero main required, then write
# whatever .github/workflows/* files the test case adds.
make_fake_repo() {
  local root="$1"
  local checks="$2"
  mkdir -p "$root/tools/branch-protection"
  mkdir -p "$root/.github/workflows"
  cat > "$root/tools/branch-protection/apply.sh" <<EOF
#!/usr/bin/env bash
# Stub apply.sh — only the heredoc-shaped check lists matter for the
# parity script. Other functions intentionally absent.

read -r -d '' STAGING_CHECKS <<'EOF2' || true
$checks
EOF2

read -r -d '' MAIN_CHECKS <<'EOF2' || true
$checks
EOF2
EOF
  chmod +x "$root/tools/branch-protection/apply.sh"
  # Place the script-under-test alongside its sibling apply.sh so the
  # script's REPO_ROOT walk finds the synthetic .github/workflows/.
  cp "$SCRIPT_UNDER_TEST" "$root/tools/branch-protection/check_name_parity.sh"
}

run_case() {
  local desc="$1"
  local checks="$2"
  local workflow_yaml="$3"   # contents to write
  local workflow_filename="$4"
  local expected_exit="$5"
  local expected_stderr_substring="$6"
  TMPDIR_FOR_CASE=$(mktemp -d)
  make_fake_repo "$TMPDIR_FOR_CASE" "$checks"
  printf '%s' "$workflow_yaml" > "$TMPDIR_FOR_CASE/.github/workflows/$workflow_filename"
  local stderr_file
  stderr_file=$(mktemp)
  local actual_exit=0
  bash "$TMPDIR_FOR_CASE/tools/branch-protection/check_name_parity.sh" 2>"$stderr_file" >/dev/null || actual_exit=$?
  local stderr_content
  stderr_content=$(cat "$stderr_file")
  rm "$stderr_file"
  if [[ "$actual_exit" -ne "$expected_exit" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit: $expected_exit, got: $actual_exit"
    echo "  stderr: $stderr_content"
    FAILED=$((FAILED+1))
    rm -rf "$TMPDIR_FOR_CASE"; TMPDIR_FOR_CASE=""
    return
  fi
  # Empty expected substring → no assertion on stderr (used for the
  # passing case where stderr should be empty / not interesting).
  if [[ -n "$expected_stderr_substring" ]]; then
    if ! grep -qF "$expected_stderr_substring" <<< "$stderr_content"; then
      echo "FAIL: $desc"
      echo "  expected stderr to contain: '$expected_stderr_substring'"
      echo "  actual stderr: $stderr_content"
      FAILED=$((FAILED+1))
      rm -rf "$TMPDIR_FOR_CASE"; TMPDIR_FOR_CASE=""
      return
    fi
  fi
  echo "PASS: $desc"
  PASSED=$((PASSED+1))
  rm -rf "$TMPDIR_FOR_CASE"; TMPDIR_FOR_CASE=""
}

# Case 1: safe workflow — no top-level paths: filter, single job
# emitting the required name. Should exit 0.
run_case "safe: no paths filter, job emits required name" \
  "Foo Build" \
  "$(cat <<'EOF'
name: Foo

on:
  push:
    branches: [main]
  pull_request:

jobs:
  foo:
    name: Foo Build
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
EOF
)" \
  "foo.yml" \
  0 \
  ""

# Case 2: unsafe — top-level paths: filter AND no per-step if-gates.
# This is the silent-block shape from the saved memory.
run_case "unsafe: top-level paths: filter without per-step if-gates" \
  "Bar Build" \
  "$(cat <<'EOF'
name: Bar

on:
  push:
    branches: [main]
    paths:
      - 'bar/**'
  pull_request:
    paths:
      - 'bar/**'

jobs:
  bar:
    name: Bar Build
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
EOF
)" \
  "bar.yml" \
  1 \
  "UNSAFE-PATH-FILTER"

# Case 3: required name has no emitter at all.
run_case "missing: required name not in any workflow" \
  "Nonexistent Job" \
  "$(cat <<'EOF'
name: Other

on:
  pull_request:

jobs:
  other:
    name: Other Job
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
EOF
)" \
  "other.yml" \
  1 \
  "MISSING: required check name 'Nonexistent Job'"

# Case 4: safe — top-level paths: filter is absent BUT per-step if-
# gates are present (single-job-with-per-step-if pattern, what
# ci.yml + e2e-api.yml use). Should exit 0.
run_case "safe: per-step if-gates without top-level paths" \
  "Baz Build" \
  "$(cat <<'EOF'
name: Baz

on:
  push:
    branches: [main]
  pull_request:

jobs:
  changes:
    name: Detect changes
    runs-on: ubuntu-latest
    outputs:
      baz: ${{ steps.check.outputs.baz }}
    steps:
      - id: check
        run: echo "baz=true" >> "$GITHUB_OUTPUT"

  baz:
    needs: changes
    name: Baz Build
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.baz != 'true'
        run: echo no-op
      - if: needs.changes.outputs.baz == 'true'
        run: echo real work
EOF
)" \
  "baz.yml" \
  0 \
  ""

# Case 5: unsafe-mix — top-level paths: AND per-step if-gates. The
# script flags this distinctly because the workflow may STILL skip
# entirely when paths exclude the commit (the per-step gates only
# matter if the workflow actually fires).
run_case "unsafe-mix: top-level paths: AND per-step if-gates" \
  "Qux Build" \
  "$(cat <<'EOF'
name: Qux

on:
  push:
    branches: [main]
    paths:
      - 'qux/**'
  pull_request:
    paths:
      - 'qux/**'

jobs:
  changes:
    name: Detect changes
    runs-on: ubuntu-latest
    outputs:
      qux: ${{ steps.check.outputs.qux }}
    steps:
      - id: check
        run: echo "qux=true" >> "$GITHUB_OUTPUT"

  qux:
    needs: changes
    name: Qux Build
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.qux == 'true'
        run: echo build
EOF
)" \
  "qux.yml" \
  1 \
  "UNSAFE-MIX"

# Case 6: codeql.yml matrix — required names like "Analyze (go)" are
# generated by `Analyze (${{ matrix.language }})`. Script must
# special-case match this pattern.
run_case "matrix: codeql Analyze (go) is recognised via matrix expansion" \
  "$(printf 'Analyze (go)\nAnalyze (javascript-typescript)\nAnalyze (python)')" \
  "$(cat <<'EOF'
name: CodeQL

on:
  pull_request:

jobs:
  analyze:
    name: Analyze (${{ matrix.language }})
    runs-on: ubuntu-latest
    strategy:
      matrix:
        language: [go, javascript-typescript, python]
    steps:
      - run: echo analyse
EOF
)" \
  "codeql.yml" \
  0 \
  ""

echo ""
echo "================================================"
echo "test_check_name_parity: $PASSED passed, $FAILED failed"
echo "================================================"
exit "$FAILED"
