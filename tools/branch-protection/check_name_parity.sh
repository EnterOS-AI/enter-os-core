#!/usr/bin/env bash
# tools/branch-protection/check_name_parity.sh — assert every required-
# check name listed in apply.sh maps to a workflow job whose "always
# emits this status" shape is intact.
#
# Closes #144 / encodes the saved memory
# feedback_branch_protection_check_name_parity:
#
#   "Path filters (e.g., detect-changes → conditional skip) silently
#    break branch protection because no job emits the protected
#    sentinel status when path-filter returns false."
#
# Two safe shapes for a required-check job:
#
#   1. Single-job-with-per-step-if (path-filter case):
#      The workflow has NO top-level `paths:` filter; the always-running
#      job has steps gated on `if: needs.<gate>.outputs.<flag> == 'true'`
#      so the no-op step alone fires when paths exclude the commit.
#      Used by ci.yml's Platform/Canvas/Python/Shellcheck and by
#      e2e-api.yml / e2e-staging-canvas.yml / runtime-prbuild-compat.yml.
#
#   2. Aggregator-with-needs+always() (matrix-refactor case):
#      An aggregator job named after the protected check `needs:` the
#      matrix children + uses `if: always()` + checks each child's
#      result. (Not currently in this repo but supported.)
#
# Unsafe shape this script catches:
#   - Workflow has top-level `paths:` filter AND the protected check
#     name is on a single job. When paths-filter excludes a commit, the
#     workflow doesn't fire — branch protection waits forever.
#
# Exit codes:
#   0 — every required check name has at least one safe-shape match
#   1 — a required name has no match OR matches an unsafe shape
#   2 — script-internal error (apply.sh missing, awk failure, etc.)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKFLOWS_DIR="$REPO_ROOT/.github/workflows"
APPLY_SH="$SCRIPT_DIR/apply.sh"

if [[ ! -f "$APPLY_SH" ]]; then
  echo "check_name_parity: missing apply.sh at $APPLY_SH" >&2
  exit 2
fi
if [[ ! -d "$WORKFLOWS_DIR" ]]; then
  echo "check_name_parity: missing .github/workflows at $WORKFLOWS_DIR" >&2
  exit 2
fi

# ─── Extract the union of required check names from apply.sh ──────
# apply.sh has STAGING_CHECKS and MAIN_CHECKS heredocs; union them so
# we audit any name that gates EITHER branch. Filters out blank lines
# and the heredoc end marker. Sorted + uniq so the audit output is stable.
#
# Captures the heredoc end-marker dynamically from the `<<'MARKER'`
# token on the opening line — the token can be `EOF` (production
# apply.sh), `EOF2` (test fixtures with nested heredocs), or any other
# bash-legal identifier. Without dynamic extraction, test fixtures
# with nested heredocs would either skip-capture (wrong end marker)
# or capture the inner end marker as a stray check name.
#
# Two-step approach to keep awk-portable across BSD awk (macOS) and
# gawk (Linux): grep finds the heredoc-opening lines, sed extracts the
# marker, then awk does the capture. Pure-awk attempts hit BSD-vs-GNU
# regex/variable-init differences that regress silently — this shape
# stays in POSIX-portable territory.
extract_heredoc_block() {
  local file="$1"
  local marker="$2"
  awk -v marker="$marker" '
    $0 ~ "<<.?" marker { capture=1; next }
    $0 == marker && capture { capture=0; next }
    capture && NF { print }
  ' "$file"
}

# Find every heredoc-end marker used in apply.sh (typically just EOF
# in the production script, but EOF2 / TAG / ABC are all valid in
# fixtures or future expansions). Each marker maps to one or more
# heredoc blocks; we union all of them.
markers=$(grep -E "<<['\"]?[A-Za-z0-9_]+['\"]?[[:space:]]*\\|\\|" "$APPLY_SH" \
  | sed -E "s/.*<<['\"]?([A-Za-z0-9_]+)['\"]?.*/\\1/" \
  | sort -u)

required_names=""
while IFS= read -r marker; do
  [[ -z "$marker" ]] && continue
  block=$(extract_heredoc_block "$APPLY_SH" "$marker")
  if [[ -n "$block" ]]; then
    required_names+="$block"$'\n'
  fi
done <<< "$markers"

required_names=$(printf '%s' "$required_names" | sort -u | sed '/^$/d')

if [[ -z "$required_names" ]]; then
  echo "check_name_parity: failed to extract required check names from apply.sh" >&2
  exit 2
fi

# ─── For each required name, find the workflow file that owns it ──
# A workflow "owns" a name if any `name:` line in the file equals the
# required name. We look at job-level names AND the workflow-level
# `name:` (the latter prefixes "Analyze" jobs in codeql.yml).
#
# Then we check whether the owning workflow has a top-level `paths:`
# filter. The unsafe shape is:
#   - top-level paths: filter present
#   - AND the named job is gated only at the workflow level (no per-
#     step `if:` gates)
#
# Distinguishing "no `paths:` filter" from "paths: filter + per-step
# gating" requires parsing the YAML semantics. We do it heuristically:
#
#   - "no top-level paths:"     → safe by construction (workflow always
#                                  fires)
#   - "paths: present"          → check that the matching job has at
#                                  least one `if: needs.<x>.outputs`
#                                  step gate. If yes, that's the
#                                  single-job-with-per-step-if shape.
#                                  If no, flag as unsafe.
#
# Heuristic so it stays a portable bash + awk + grep tool — full YAML
# parsing would need yq which isn't a dependency. The known unsafe
# shape (workflow-level paths: AND no per-step if-gates) is what we're
# trying to catch.

failed=0
declare -a unsafe_findings=()

while IFS= read -r name; do
  [[ -z "$name" ]] && continue
  # Find every workflow file that contains a job with `name: <name>` or
  # whose top-level workflow `name:` plus matrix substitution would
  # produce <name>. Need to be careful about quoting — YAML allows
  # `name: Foo`, `name: "Foo"`, `name: 'Foo'`. Strip quotes.
  matches=()
  while IFS= read -r f; do
    # Look for an exact `name:` match (anywhere in the file). The
    # workflow-level name line is at column 0; job-level names are
    # indented. Either is acceptable for parity — what matters is
    # whether the EMITTED check-run name is the one we required.
    # Strip surrounding quotes/whitespace before comparing.
    if awk -v want="$name" '
      /^[[:space:]]*name:[[:space:]]*/ {
        line = $0
        sub(/^[[:space:]]*name:[[:space:]]*/, "", line)
        # Strip surrounding " or '\''
        gsub(/^["\047]|["\047]$/, "", line)
        # Strip trailing whitespace + comment
        sub(/[[:space:]]*#.*$/, "", line)
        sub(/[[:space:]]+$/, "", line)
        if (line == want) found = 1
      }
      END { exit !found }
    ' "$f"; then
      matches+=("$f")
    fi
  done < <(find "$WORKFLOWS_DIR" -name '*.yml' -o -name '*.yaml')

  if [[ ${#matches[@]} -eq 0 ]]; then
    # Special case — Analyze (go/javascript-typescript/python) is
    # generated by codeql.yml's matrix expansion of `Analyze (${{
    # matrix.language }})`. Don't flag those as missing if codeql.yml
    # exists with the expected base name.
    case "$name" in
      "Analyze (go)"|"Analyze (javascript-typescript)"|"Analyze (python)")
        # shellcheck disable=SC2016
        # The literal `${{ matrix.language }}` is the GHA template
        # syntax we're searching FOR — not a shell expansion. SC2016
        # would have us add quotes that defeat the search.
        if [[ -f "$WORKFLOWS_DIR/codeql.yml" ]] && \
           grep -q 'name: Analyze (${{[[:space:]]*matrix.language[[:space:]]*}})' "$WORKFLOWS_DIR/codeql.yml"; then
          matches=("$WORKFLOWS_DIR/codeql.yml")
        fi
        ;;
    esac
  fi

  if [[ ${#matches[@]} -eq 0 ]]; then
    unsafe_findings+=("MISSING: required check name '$name' has no matching workflow job")
    failed=1
    continue
  fi

  # For each owning workflow, classify safe vs unsafe.
  for f in "${matches[@]}"; do
    rel="${f#"$REPO_ROOT"/}"
    # Heuristic: does the workflow have a top-level `paths:` filter?
    # Top-level here means under the `on:` key, not under jobs.<x>.if.
    # Workflow-level paths filters appear at indent depth 4 (under
    # `push:` or `pull_request:`). Job-level `if:` paths-filter doesn't
    # block the workflow from firing.
    has_top_paths=0
    if awk '
      # Track whether we are inside the `on:` block. The `on:` block
      # starts at column 0 (`on:` key) and ends when the next column-0
      # key appears.
      /^on:[[:space:]]*$/ { in_on = 1; next }
      /^[a-zA-Z]/ && in_on { in_on = 0 }
      in_on && /^[[:space:]]+paths:[[:space:]]*$/ { print "yes"; exit }
      in_on && /^[[:space:]]+paths:[[:space:]]*\[/ { print "yes"; exit }
    ' "$f" | grep -q yes; then
      has_top_paths=1
    fi

    if [[ "$has_top_paths" -eq 0 ]]; then
      # Safe: workflow always fires. If there are inner per-step if-
      # gates (single-job-with-per-step-if pattern), the no-op step
      # produces SUCCESS for the protected name — branch-protection-clean.
      continue
    fi

    # Unsafe candidate — has top-level paths: AND we need to verify
    # the per-step if-gate pattern is absent. Look for any `if:`
    # referencing a paths-filter / detect-changes output inside the
    # owning job's body. If at least one is present, classify as the
    # single-job-with-per-step-if pattern (safe).
    #
    # The regex is intentionally anchored loosely — actual workflow
    # YAML writes per-step if-gates as `      - if: needs.X.outputs.Y`
    # (with the `-` step-marker between the leading spaces and the
    # `if`). Anchoring on `^[[:space:]]+if:` would miss those.
    if grep -qE "if:[[:space:]]+needs\.[a-zA-Z_-]+\.outputs\." "$f"; then
      # Per-step if-gates exist. Combined with top-level paths: this
      # would be a buggy mix (the workflow might still skip entirely
      # when paths exclude). Flag as unsafe — the safe pattern omits
      # the top-level paths: filter altogether and gates per-step.
      unsafe_findings+=("UNSAFE-MIX: $rel has top-level paths: AND per-step if-gates — when paths exclude the commit, the workflow doesn't fire and the required check '$name' is silently absent. Drop the top-level paths: filter; keep the per-step if-gates.")
      failed=1
    else
      # Top-level paths: with no per-step if-gates: the canonical
      # check-name parity bug.
      unsafe_findings+=("UNSAFE-PATH-FILTER: $rel has top-level paths: filter and no per-step if-gates. When paths exclude the commit, no job emits the required check '$name' — branch protection waits forever. Either drop the paths: filter and add per-step if-gates against a detect-changes output, or add an aggregator-with-needs+always() job that emits '$name'.")
      failed=1
    fi
  done
done <<< "$required_names"

if [[ "$failed" -eq 0 ]]; then
  echo "check_name_parity: OK — every required check name maps to a safe workflow shape."
  exit 0
fi

echo "check_name_parity: FOUND $((${#unsafe_findings[@]})) issue(s):" >&2
for finding in "${unsafe_findings[@]}"; do
  echo "  - $finding" >&2
done
exit 1
