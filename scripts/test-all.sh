#!/usr/bin/env bash
# test-all.sh — Run all test suites for the molecule-monorepo monorepo.
# Exit code: 0 if all suites pass, 1 if any suite fails.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
RESET='\033[0m'

declare -A RESULTS
declare -A DURATIONS

run_suite() {
  local name="$1"
  local dir="$2"
  local cmd="$3"

  echo ""
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
  echo -e "${BOLD}  $name${RESET}"
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"

  local start
  start=$(date +%s)

  if (cd "$dir" && eval "$cmd"); then
    RESULTS["$name"]="PASS"
  else
    RESULTS["$name"]="FAIL"
  fi

  local end
  end=$(date +%s)
  DURATIONS["$name"]=$((end - start))
}

# ──────────────────────────────────────────────────────────────────────────────
# 1. workspace-template — Python / pytest
# ──────────────────────────────────────────────────────────────────────────────
run_suite "workspace-template (Python)" \
  "$REPO_ROOT/workspace-template" \
  "python -m pytest"

# ──────────────────────────────────────────────────────────────────────────────
# 2. platform — Go
# ──────────────────────────────────────────────────────────────────────────────
if command -v go &>/dev/null; then
  run_suite "platform (Go)" \
    "$REPO_ROOT/platform" \
    "go test ./... -coverprofile=/tmp/molecule_go_coverage.out && go tool cover -func=/tmp/molecule_go_coverage.out | tail -1"
else
  echo -e "${YELLOW}  ⚠  go not found — skipping platform (Go) tests${RESET}"
  RESULTS["platform (Go)"]="SKIP"
  DURATIONS["platform (Go)"]=0
fi

# ──────────────────────────────────────────────────────────────────────────────
# 3. canvas — TypeScript / Vitest
# ──────────────────────────────────────────────────────────────────────────────
if command -v npm &>/dev/null; then
  run_suite "canvas (TypeScript/Vitest)" \
    "$REPO_ROOT/canvas" \
    "npm test -- --reporter=verbose 2>&1"
else
  echo -e "${YELLOW}  ⚠  npm not found — skipping canvas tests${RESET}"
  RESULTS["canvas (TypeScript/Vitest)"]="SKIP"
  DURATIONS["canvas (TypeScript/Vitest)"]=0
fi

# ──────────────────────────────────────────────────────────────────────────────
# 4. mcp-server — TypeScript / Jest
# ──────────────────────────────────────────────────────────────────────────────
if command -v npm &>/dev/null; then
  run_suite "mcp-server (TypeScript/Jest)" \
    "$REPO_ROOT/mcp-server" \
    "npm test 2>&1"
else
  echo -e "${YELLOW}  ⚠  npm not found — skipping mcp-server tests${RESET}"
  RESULTS["mcp-server (TypeScript/Jest)"]="SKIP"
  DURATIONS["mcp-server (TypeScript/Jest)"]=0
fi

# ──────────────────────────────────────────────────────────────────────────────
# Summary
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}╔══════════════════════════════════════════════════╗${RESET}"
echo -e "${BOLD}║              TEST SUITE SUMMARY                  ║${RESET}"
echo -e "${BOLD}╠══════════════════════════════════════════════════╣${RESET}"

OVERALL=0
for suite in "workspace-template (Python)" "platform (Go)" "canvas (TypeScript/Vitest)" "mcp-server (TypeScript/Jest)"; do
  result="${RESULTS[$suite]:-SKIP}"
  dur="${DURATIONS[$suite]:-0}"

  case "$result" in
    PASS)
      icon="${GREEN}✓${RESET}"
      ;;
    FAIL)
      icon="${RED}✗${RESET}"
      OVERALL=1
      ;;
    SKIP)
      icon="${YELLOW}⊘${RESET}"
      ;;
  esac

  printf "${BOLD}║${RESET}  %b  %-38s %3ds  ${BOLD}║${RESET}\n" \
    "$icon" "$suite" "$dur"
done

echo -e "${BOLD}╚══════════════════════════════════════════════════╝${RESET}"
echo ""

if [ $OVERALL -eq 0 ]; then
  echo -e "${GREEN}${BOLD}All test suites passed.${RESET}"
else
  echo -e "${RED}${BOLD}One or more test suites failed.${RESET}"
fi

exit $OVERALL
