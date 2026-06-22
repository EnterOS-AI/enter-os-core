#!/usr/bin/env bash
# onboard-template.sh — gitops helper to wire local-e2e into a new template.
#
# Drops .gitea/workflows/session-continuity-e2e.yml into the target template
# repo (a thin shim that clones molecule-core's local-e2e harness, then runs
# run-canary.sh against the locally-built template image). Opens a PR.
#
# Usage:
#   ./local-e2e/scripts/onboard-template.sh molecule-ai-workspace-template-claude-code
#
# Per task #342 sequencing: do NOT run this for every template at once.
# Bake the gate on hermes for ≥5 business days first; expand only after
# the canary is empirically stable.
#
# Cross-refs:
#   feedback_no_single_source_of_truth — the workflow content is identical
#     across templates; this helper guarantees it.
#   feedback_image_promote_is_not_user_live — we wire the gate at the
#     CI layer; flipping it to REQUIRED in branch_protection is a
#     separate step (see README.md).

set -euo pipefail

REPO="${1:?usage: onboard-template.sh <template-repo-name>}"
HARNESS_ROOT="$( cd "$( dirname "${BASH_SOURCE[0]}" )/.." && pwd )"

# Sanity: ensure the template-side workflow file exists in this repo.
TEMPLATE_WORKFLOW="$HARNESS_ROOT/templates/session-continuity-e2e.yml"
[ -f "$TEMPLATE_WORKFLOW" ] || {
    echo "ERROR: $TEMPLATE_WORKFLOW not found in this harness checkout"
    exit 1
}

WORK_DIR=$(mktemp -d -t e2e-onboard-XXXXXX)
trap 'rm -rf "$WORK_DIR"' EXIT

cd "$WORK_DIR"

# Use mol_clone — preserves the persona credential model.
# shellcheck disable=SC1090
source "$HOME/.molecule-ai/ops.sh"
mol_clone "$REPO"
cd "$REPO"

git checkout -b "task342/session-continuity-e2e-gate"

mkdir -p .gitea/workflows
cp "$TEMPLATE_WORKFLOW" .gitea/workflows/session-continuity-e2e.yml

git add .gitea/workflows/session-continuity-e2e.yml
git commit -m "ci: add local-e2e session-continuity canary gate (task #342)

Wires this template into the cross-template session-continuity harness
in molecule-ai/molecule-core/local-e2e/. The gate boots THIS repo's
locally-built image, drives 4 canonical canaries (2-turn name continuity,
file-only message, file+prompt, cross-session memory recall), and fails
PRs that regress any of them.

Per CTO directive: required-context flip in branch_protection is a
SEPARATE step after 5 business days of bake."

# Push branch; do not auto-open PR — leave that to the operator so the
# review-relay routing follows the same rules as a normal change.
git push -u origin "task342/session-continuity-e2e-gate"

echo
echo "DONE. Branch pushed to $REPO. Open PR manually:"
echo "  https://git.moleculesai.app/molecule-ai/$REPO/compare/main...task342/session-continuity-e2e-gate"
