#!/usr/bin/env bash
# check-cascade-list-vs-manifest.sh — structural drift gate for the
# publish-runtime cascade list vs manifest.json workspace_templates.
#
# WHY: PR #2536 pruned the manifest to 4 supported runtimes; PR #2556
# realigned the cascade list to match. The underlying drift hazard
# (cascade-list ≠ manifest) was unguarded — the data fix didn't prevent
# recurrence. This script is the structural gate that does.
#
# Behavior-based per project pattern: derives the expected set from
# manifest.json and the actual set from the workflow YAML, fails on
# any divergence in either direction.
#
#   missing-from-cascade  → templates in manifest that publish-runtime.yml
#                            won't auto-rebuild on a new wheel publish
#                            (the codex-stuck-on-stale-runtime bug class)
#   extra-in-cascade      → cascade dispatches to deprecated templates
#                            (the wasted-API-calls + dead-CI-noise class)
#
# Suffix mapping: manifest names map to GHCR repos via
#   {name without -default suffix} → molecule-ai-workspace-template-<suffix>
# That's the same map publish-runtime.yml's TEMPLATES variable iterates.
#
# Exit:
#   0  cascade matches manifest exactly
#   1  drift detected (script prints the diff)
#   2  bad usage / missing inputs

set -eu

MANIFEST="${1:-manifest.json}"
WORKFLOW="${2:-.github/workflows/publish-runtime.yml}"

if [ ! -f "$MANIFEST" ]; then
    echo "::error::manifest not found: $MANIFEST" >&2
    exit 2
fi
if [ ! -f "$WORKFLOW" ]; then
    echo "::error::workflow not found: $WORKFLOW" >&2
    exit 2
fi

# Expected cascade entries: manifest workspace_templates → suffix-only
# (strip -default tail, e.g. claude-code-default → claude-code, since
# publish-runtime.yml's TEMPLATES uses suffixes that match the
# molecule-ai-workspace-template-<suffix> repo naming).
EXPECTED=$(jq -r '.workspace_templates[].name' "$MANIFEST" \
    | sed 's/-default$//' \
    | sort -u)

# Actual cascade entries: extract from the TEMPLATES="…" line. We look
# for the line, pull the contents between the quotes, and split into
# one-per-line. Single source of truth in the workflow itself, no
# parallel registry needed.
#
# Why not \s in the regex: BSD sed (macOS) doesn't recognize \s as
# whitespace — treats it as literal `s`. POSIX [[:space:]] works on
# both BSD and GNU sed. Same hazard nuked the original draft of this
# script: \s* matched empty-prefix-of-literal-s, then the leading
# whitespace stayed in the captured group.
ACTUAL=$(grep -E '[[:space:]]*TEMPLATES="' "$WORKFLOW" \
    | head -1 \
    | sed -E 's/^[[:space:]]*TEMPLATES="([^"]*)".*$/\1/' \
    | tr ' ' '\n' \
    | grep -v '^$' \
    | sort -u)

if [ -z "$ACTUAL" ]; then
    echo "::error::could not extract TEMPLATES=\"…\" from $WORKFLOW — has the variable name or quoting changed?" >&2
    exit 2
fi

MISSING=$(comm -23 <(printf '%s\n' "$EXPECTED") <(printf '%s\n' "$ACTUAL"))
EXTRA=$(comm -13 <(printf '%s\n' "$EXPECTED") <(printf '%s\n' "$ACTUAL"))

if [ -z "$MISSING" ] && [ -z "$EXTRA" ]; then
    echo "✓ cascade list matches manifest workspace_templates ($(echo "$EXPECTED" | wc -l | tr -d ' ') entries)"
    exit 0
fi

echo "::error::cascade list drift detected between $MANIFEST and $WORKFLOW" >&2
echo "" >&2
if [ -n "$MISSING" ]; then
    echo "  Templates in manifest but MISSING from cascade (won't auto-rebuild on wheel publish):" >&2
    echo "$MISSING" | sed 's/^/    - /' >&2
    echo "" >&2
fi
if [ -n "$EXTRA" ]; then
    echo "  Templates in cascade but NOT in manifest (deprecated, wasting dispatch calls):" >&2
    echo "$EXTRA" | sed 's/^/    - /' >&2
    echo "" >&2
fi
echo "  Fix: edit the TEMPLATES=\"…\" line in $WORKFLOW so the set matches" >&2
echo "  manifest.json's workspace_templates (suffix-stripped). See PR #2556 for context." >&2
exit 1
