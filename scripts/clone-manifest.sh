#!/bin/sh
# clone-manifest.sh — clone all repos listed in manifest.json into their
# target directories. Replaces hardcoded git-clone lines in Dockerfiles.
#
# Usage:
#   ./scripts/clone-manifest.sh <manifest.json> <ws-templates-dir> <org-templates-dir> <plugins-dir>
#
# Requires: git, jq (lighter than python3 — ~2MB vs ~50MB in Alpine)

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

        # Post-2026-05-06 GitHub-org-suspension: clone from Gitea instead.
        # manifest.json paths still read "Molecule-AI/..." (the historic
        # github.com slug); Gitea lowercases the org part to "molecule-ai/".
        # Lowercase the org segment on the fly so we don't need to rewrite
        # every manifest entry.
        repo_gitea="$(echo "$repo" | awk -F/ '{ printf "%s", tolower($1); for (i=2; i<=NF; i++) printf "/%s", $i; print "" }')"

        echo "  cloning $repo_gitea -> $target_dir/$name (ref=$ref)"
        if [ "$ref" = "main" ]; then
            git clone --depth=1 -q "https://git.moleculesai.app/${repo_gitea}.git" "$target_dir/$name"
        else
            git clone --depth=1 -q --branch "$ref" "https://git.moleculesai.app/${repo_gitea}.git" "$target_dir/$name"
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
