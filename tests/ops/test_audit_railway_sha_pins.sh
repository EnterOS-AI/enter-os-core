#!/usr/bin/env bash
# Regression test for scripts/ops/audit-railway-sha-pins.sh — pins the
# drift-detection regex's behavior against a curated set of should-flag
# and should-pass values. A future regex tweak that weakens detection
# (e.g. drops the substring branch, narrows the SHA length, etc.) fails
# loud here.
set -uo pipefail

# Same regex as the audit script. Keep these two locked in step.
DRIFT_REGEX='(staging|main|prod|production)-[a-f0-9]{6,}|[:=]v?[0-9]+\.[0-9]+\.[0-9]+([^a-z0-9]|$)'

PASS=0
FAIL=0

assert() {
  local label="$1" value="$2" want="$3"  # want = "flag" or "pass"
  local hit
  if echo "$value" | grep -qE "$DRIFT_REGEX"; then
    hit="flag"
  else
    hit="pass"
  fi
  if [ "$hit" = "$want" ]; then
    echo "  ✓ $label"
    PASS=$((PASS+1))
  else
    echo "  ✗ $label: value=$value  expected=$want  got=$hit" >&2
    FAIL=$((FAIL+1))
  fi
}

echo "Test: drift-detection regex"
echo

# ── should FLAG ────────────────────────────────────────────────────────
echo "Should flag (drift-prone):"
assert "branch-SHA suffix (the 2026-04-24 incident)"  "ghcr.io/molecule/tenant:staging-a14cf86"          flag
assert "main-SHA suffix"                              "ghcr.io/molecule/tenant:main-d3adb33f"            flag
assert "prod-SHA suffix"                              "ghcr.io/molecule/tenant:prod-cafef00d"            flag
assert "production-SHA suffix"                        "ghcr.io/molecule/tenant:production-1234567890ab"  flag
assert "semver tag :v1.2.3"                           "ghcr.io/molecule/tenant:v1.2.3"                   flag
assert "semver tag :1.2.3 (no v)"                     "ghcr.io/molecule/tenant:1.2.3"                    flag
assert "semver patch-zero :v2.0.0"                    "ghcr.io/molecule/tenant:v2.0.0"                   flag
assert "semver in middle of value"                    "TEMPLATE_PIN=v0.1.16/extra"                       flag
assert "branch-SHA as part of longer value"           "image=foo:staging-abc1234,other=bar"              flag

# ── should PASS ────────────────────────────────────────────────────────
echo
echo "Should pass (floating / unrelated):"
assert "floating tag :staging-latest"                 "ghcr.io/molecule/tenant:staging-latest"           pass
assert "floating tag :main"                           "ghcr.io/molecule/tenant:main"                     pass
assert "floating tag :latest"                         "ghcr.io/molecule/tenant:latest"                   pass
assert "URL"                                          "https://api.moleculesai.app/v1"                   pass
assert "secret-shaped string"                         "cfut_loLRZGHCF0ySpUeESUL0OB"                      pass
assert "human name"                                   "Hongming Wang"                                    pass
assert "uuid"                                         "a034108e-da16-d131-ef7f-766b923ef464"             pass
assert "AWS ARN"                                      "arn:aws:secretsmanager:us-east-2:123:secret/foo"  pass
assert "short hash (under 6 chars)"                   "ghcr.io/molecule/tenant:staging-abc12"            pass
assert "version field, not tag (no leading colon)"    "version 1.2.3 of the api"                         pass
assert "AMI id"                                       "ami-0abcd1234efgh5678"                            pass

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" = "0" ]
