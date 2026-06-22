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
#   Repos in manifest.json may be public or platform-private. CI and
#   operator refresh jobs should set MOLECULE_GITEA_TOKEN to the
#   SSOT-managed template read token. Anonymous clone still works for
#   public entries, but private platform templates depend on the token.
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

# Strip JSON5-style // comments from manifest.json before parsing.
# The automated Integration Tester appends a trailing comment
# (// Triggered by ... ) which is valid JSON5 but not standard JSON.
# jq's default parser rejects it. This sed removes only full-line comments
# (lines starting with optional whitespace followed by //) before jq reads the file.
_strip_comments() {
    # Remove full-line // comments (whitespace-safe); pass-through for non-comment lines
    sed 's/^[[:space:]]*\/\/.*//' "$MANIFEST"
}
MANIFEST_JSON="$(_strip_comments)"

EXPECTED=0
CLONED=0

# clone_one_with_retry — clone a single repo, retrying on transient failure.
#
# Why: the publish-workspace-server-image (and harness-replays) CI jobs
# clone the full manifest (~36 repos) serially on a memory-constrained
# Gitea Actions runner. Under host memory pressure the OOM killer
# occasionally SIGKILLs git-remote-https mid-clone:
#
#   error: git-remote-https died of signal 9
#   fatal: the remote end hung up unexpectedly
#
# (observed in publish-workspace-server-image run 4622 on 2026-05-10 — the
# job died on the 14th of 36 clones, which wedged staging→main). One
# transient SIGKILL / network blip would otherwise fail the whole tenant
# image rebuild. Retrying after a short backoff lets the pressure subside.
# The durable fix is more runner RAM/swap (tracked with Infra-SRE); this
# just stops a single flake from being release-blocking.
#
# Args: <target_dir> <name> <clone_url> <display_url> <ref>
clone_one_with_retry() {
    local tdir="$1" name="$2" url="$3" display="$4" ref="$5"
    local attempt=1 max_attempts=3 backoff

    while : ; do
        # A killed attempt can leave a partial directory behind; git clone
        # refuses a non-empty target, so wipe it before each try.
        rm -rf "$tdir/$name"

        if [ "$ref" = "main" ]; then
            if git clone --depth=1 -q "$url" "$tdir/$name"; then return 0; fi
        elif echo "$ref" | grep -qE '^[0-9a-f]{40}$'; then
            # Pinned SHA (RFC #2927 manifest ref-pinning): `--branch <sha>` fails
            # with "Remote branch <sha> not found" because git's --branch only
            # resolves named refs. Clone the full repo (no --depth so the SHA
            # is reachable in history) then check out the pinned SHA.
            if git clone -q "$url" "$tdir/$name" \
                && (cd "$tdir/$name" && git checkout -q "$ref"); then
                # Drop .git after checkout — we only need the tree (matches
                # the post-clone .git strip below in clone_category).
                rm -rf "$tdir/$name/.git"
                return 0
            fi
        else
            if git clone --depth=1 -q --branch "$ref" "$url" "$tdir/$name"; then return 0; fi
        fi

        if [ "$attempt" -ge "$max_attempts" ]; then
            echo "::error::clone failed after ${max_attempts} attempts: ${display}" >&2
            return 1
        fi
        backoff=$((attempt * 3))   # 3s, then 6s
        echo "  ⚠ clone attempt ${attempt}/${max_attempts} failed for ${display} — retrying in ${backoff}s" >&2
        sleep "$backoff"
        attempt=$((attempt + 1))
    done
}

clone_category() {
    local category="$1"
    local target_dir="$2"

    mkdir -p "$target_dir"

    local count
    count=$(echo "$MANIFEST_JSON" | jq -r ".${category} | length")
    EXPECTED=$((EXPECTED + count))

    local i=0
    while [ "$i" -lt "$count" ]; do
        local name repo ref
        name=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].name")
        repo=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].repo")
        ref=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].ref // \"main\"")

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
        clone_one_with_retry "$target_dir" "$name" "$clone_url" "$display_url" "$ref"
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
