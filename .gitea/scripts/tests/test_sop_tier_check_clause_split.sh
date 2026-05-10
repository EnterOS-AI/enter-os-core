#!/usr/bin/env bash
# Regression test for #229 — sop-tier-check tier:low OR-clause splitter.
#
# Bug (PR #225 → still broken after PR #231):
#   Line ~289 of sop-tier-check.sh used:
#     _clause=$(echo "$_raw_clause" | tr -d '()' | tr ',' '\n' | tr -d '[:space:]' | grep -v '^$')
#   `tr -d '[:space:]'` strips the newlines that `tr ',' '\n'` just
#   inserted, collapsing "engineers,managers,ceo" into a single token
#   "engineersmanagersceo". The for-loop then iterates ONCE on a name
#   that matches no team, so every tier:low PR fails:
#     ::error::clause [engineers/managers/ceo]: FAIL — no approving
#     reviewer belongs to any of these teamsengineersmanagersceo
#   (note also: missing separators in the error string is bug #2 —
#    `_clause_names` used "${var:+, }$x" which OVERWRITES per iteration).
#
# Fix shape (this PR):
#   _no_parens=${_raw_clause//[()]/}
#   _clause=${_no_parens//,/ }    # comma -> space, bash word-split iterates
#   _clause_names="${_clause_names}${_clause_names:+, }${_t}"  # APPEND, not overwrite
#
# This test extracts the splitter logic and asserts it produces the right
# token list for each of the three tier expressions live in the script.

set -euo pipefail

PASS=0
FAIL=0

assert_eq() {
  local label="$1"
  local expected="$2"
  local got="$3"
  if [ "$expected" = "$got" ]; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label"
    echo "        expected: <$expected>"
    echo "        got:      <$got>"
    FAIL=$((FAIL + 1))
  fi
}

# ----- Splitter under test (mirrors the fixed sop-tier-check.sh block) -----
split_clause() {
  local raw="$1"
  local no_parens=${raw//[()]/}
  local clause=${no_parens//,/ }
  local out=""
  for _t in $clause; do
    out="${out}${out:+|}$_t"
  done
  echo "$out"
}

echo "test: tier:low OR-clause splits to 3 tokens"
assert_eq "tier:low" "engineers|managers|ceo" "$(split_clause "engineers,managers,ceo")"

echo "test: tier:medium AND-expression — bash word-split on \$EXPR yields 5 tokens"
EXPR="managers AND engineers AND qa???,security???"
out=""
for _raw in $EXPR; do
  out="${out}${out:+ ; }$(split_clause "$_raw")"
done
assert_eq "tier:medium" "managers ; AND ; engineers ; AND ; qa???|security???" "$out"

echo "test: tier:high single-team OR-clause"
assert_eq "tier:high" "ceo" "$(split_clause "ceo")"

echo "test: paren-wrapped OR-set unwraps + splits"
assert_eq "paren OR" "managers|ceo" "$(split_clause "(managers,ceo)")"

# ----- _clause_names accumulator (was overwriting per iteration) -----
acc=""
for t in engineers managers ceo; do
  acc="${acc}${acc:+, }${t}"
done
assert_eq "_clause_names append" "engineers, managers, ceo" "$acc"

# ----- _failed_clauses / _passed_clauses accumulator across raw clauses -----
acc=""
for c in clauseA clauseB clauseC; do
  acc="${acc}${acc:+, }${c}"
done
assert_eq "_failed_clauses append" "clauseA, clauseB, clauseC" "$acc"

# ----- End-to-end OR-gate: simulate APPROVER_TEAMS[core-lead]=' managers ' -----
# The script's case pattern is *${_t}* with a space-padded value.
APPROVER_TEAMS_VAL=" managers "
matched=""
for _t in $(split_clause "engineers,managers,ceo" | tr '|' ' '); do
  case "$APPROVER_TEAMS_VAL" in
    *${_t}*) matched="$_t"; break ;;
  esac
done
assert_eq "OR-gate matches managers" "managers" "$matched"

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
