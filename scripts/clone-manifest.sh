#!/bin/sh
# clone-manifest.sh — clone all repos listed in manifest.json into their
# target directories. Replaces hardcoded git-clone lines in Dockerfiles.
#
# Usage:
#   ./scripts/clone-manifest.sh <manifest.json> <ws-templates-dir> <org-templates-dir> <plugins-dir>
#
# Requires: git, jq (lighter than python3 — ~2MB vs ~50MB in Alpine)
#
# Auth (optional):
#   Post-2026-05-08 (#192): every repo in manifest.json is public on
#   git.moleculesai.app. Anonymous clone works for the entire registered
#   set. The OSS-surface contract is recorded in manifest.json's _comment
#   — Layer-3 customer/private templates (e.g. reno-stars) are NOT in the
#   manifest; they are handled at provision-time via the per-tenant
#   credential resolver (internal#102 RFC).
#
#   MOLECULE_GITEA_TOKEN is therefore optional today. Kept supported for
#   two reasons: (a) historical CI configs that still inject
#   AUTO_SYNC_TOKEN remain harmless, (b) reserved for the case where a
#   private internal-only template is later registered via a ci-readonly
#   team grant — review must explicitly sign off on that, since it
#   violates the public-OSS-surface contract.
#
#   The token (when set) never enters the Docker image: this script runs
#   in the trusted CI context BEFORE `docker buildx build`, populates
#   .tenant-bundle-deps/, then `Dockerfile.tenant` COPYs from there with
#   the .git directories already stripped (see line ~67 below).

set -euo pipefail

MANIFEST="${1:?Usage: clone-manifest.sh <manifest.json> <ws-dir> <org-dir> <plugins-dir>}"
WS_DIR="${2:?Missing workspace-templates dir}"
ORG_DIR="${3:?Missing org-templates dir}"
PLUGINS_DIR="${4:?Missing plugins dir}"

EXPECTED=0
CLONED=0

clone_category() {
    local category="$1"
    local target_dir="$2"

    mkdir -p "$target_dir"

    local count
    count=$(jq -r ".${category} | length" "$MANIFEST")
    EXPECTED=$((EXPECTED + count))

    local i=0
    while [ "$i" -lt "$count" ]; do
        local name repo ref
        name=$(jq -r ".${category}[$i].name" "$MANIFEST")
        repo=$(jq -r ".${category}[$i].repo" "$MANIFEST")
        ref=$(jq -r ".${category}[$i].ref // \"main\"" "$MANIFEST")

        # Idempotent: skip if the target already looks populated. Lets the
        # README quickstart rerun setup.sh safely without having to delete
        # already-cloned repos. A directory with any entries counts as
        # populated; empty dirs reclone (may exist from a prior failed run).
        if [ -d "$target_dir/$name" ] && [ -n "$(ls -A "$target_dir/$name" 2>/dev/null || true)" ]; then
            echo "  skipping $target_dir/$name (already populated)"
            CLONED=$((CLONED + 1))
            i=$((i + 1))
            continue
        fi

        # Build the clone URL. When MOLECULE_GITEA_TOKEN is set (CI path)
        # embed it as basic-auth so private repos succeed. The username
        # part ("oauth2") is conventional and ignored by Gitea — only the
        # token-as-password is verified.
        #
        # manifest.json was migrated to lowercase org slugs on
        # 2026-05-07 (post-suspension reconciliation), so we use $repo
        # verbatim — no on-the-fly tolower transform needed.
        if [ -n "${MOLECULE_GITEA_TOKEN:-}" ]; then
            clone_url="https://oauth2:${MOLECULE_GITEA_TOKEN}@git.moleculesai.app/${repo}.git"
            display_url="https://oauth2:***@git.moleculesai.app/${repo}.git"
        else
            clone_url="https://git.moleculesai.app/${repo}.git"
            display_url="$clone_url"
        fi

        echo "  cloning $display_url -> $target_dir/$name (ref=$ref)"
        if [ "$ref" = "main" ]; then
            git clone --depth=1 -q "$clone_url" "$target_dir/$name"
        else
            git clone --depth=1 -q --branch "$ref" "$clone_url" "$target_dir/$name"
        fi
        CLONED=$((CLONED + 1))
        i=$((i + 1))
    done

    # Strip .git dirs to save space
    find "$target_dir" -name '.git' -type d -exec rm -rf {} + 2>/dev/null || true
}

echo "==> Cloning workspace templates..."
clone_category "workspace_templates" "$WS_DIR"

echo "==> Cloning org templates..."
clone_category "org_templates" "$ORG_DIR"

echo "==> Cloning plugins..."
clone_category "plugins" "$PLUGINS_DIR"

# Verify all repos were cloned
if [ "$CLONED" -ne "$EXPECTED" ]; then
    echo "::error::Expected $EXPECTED repos but only cloned $CLONED — some clones failed"
    exit 1
fi

echo "==> Done. $CLONED/$EXPECTED repos cloned successfully."
