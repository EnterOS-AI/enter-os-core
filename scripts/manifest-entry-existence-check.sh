#!/usr/bin/env bash
# manifest-entry-existence-check.sh — PR-time guard: verify every repo listed in
# manifest.json actually exists on Gitea before merge.
#
# Mirrors clone-manifest.sh retry behavior (3 attempts, linear backoff) and
# fails closed on any exhausted non-200 status (404, 500, 403, auth/network
# failures, etc.) so bad manifest entries cannot slip through.
#
# Usage:
#   GITEA_HOST=git.example.com GITEA_TOKEN=xxx ./manifest-entry-existence-check.sh [manifest.json]
#
# Exit:
#   0  all repos exist / were reachable
#   1  one or more entries could not be validated
#   2  bad usage / missing inputs / required env not set

set -euo pipefail

MANIFEST="${1:-manifest.json}"
GITEA_HOST="${GITEA_HOST:-}"
GITEA_TOKEN="${GITEA_TOKEN:-${MOLECULE_GITEA_TOKEN:-}}"
GITEA_API="${GITEA_API:-https://${GITEA_HOST}/api/v1/repos}"

if [ ! -f "$MANIFEST" ]; then
    echo "::error::manifest not found: $MANIFEST" >&2
    exit 2
fi

if [ -z "$GITEA_HOST" ]; then
    echo "::error::GITEA_HOST is not set" >&2
    exit 2
fi

if [ -z "$GITEA_TOKEN" ]; then
    echo "::error::GITEA_TOKEN (or MOLECULE_GITEA_TOKEN) is not set" >&2
    exit 2
fi

# Strip JSON5-style // comments before parsing (same as clone-manifest.sh)
_strip_comments() {
    sed '/^[[:space:]]*\/\//d' "$MANIFEST"
}

MANIFEST_JSON="$(_strip_comments)"

TOTAL=0
MISSING=()

_check_entry() {
    local name="$1" repo="$2"
    local last_http_code=""

    for attempt in 1 2 3; do
        local http_code
        http_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
            -H "Authorization: token ${GITEA_TOKEN}" \
            "${GITEA_API}/${repo}" 2>/dev/null || true)
        last_http_code="$http_code"

        if [ "$http_code" = "200" ]; then
            echo "  OK: $name -> $repo"
            return 0
        elif [ "$http_code" = "404" ]; then
            echo "::error::manifest entry '$name' points at $repo which does not exist on Gitea (404)"
            MISSING+=("$name:$repo (404)")
            return 0
        else
            echo "  attempt $attempt: '$name' -> $repo returned HTTP ${http_code:-(none)}, retrying"
            sleep $((attempt * 2))
        fi
    done

    # After exhausting retries, any non-200 status that wasn't already recorded
    # as 404 is a validation failure (500, 403, auth/network gateway errors, etc.).
    echo "::error::manifest entry '$name' -> $repo could not be validated after 3 attempts (last HTTP ${last_http_code:-(none)})"
    MISSING+=("$name:$repo (last HTTP ${last_http_code:-(none)})")
}

# Categories to check — must match manifest.json schema
_check_category() {
    local category="$1"
    local count
    count=$(echo "$MANIFEST_JSON" | jq -r ".${category} | length")

    local i=0
    while [ "$i" -lt "$count" ]; do
        local name repo
        name=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].name")
        repo=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].repo")
        TOTAL=$((TOTAL + 1))
        _check_entry "$name" "$repo"
        i=$((i + 1))
    done
}

_check_category "plugins"
_check_category "workspace_templates"
_check_category "org_templates"

if [ "${#MISSING[@]}" -gt 0 ]; then
    echo "::error::${#MISSING[@]} of ${TOTAL} manifest entries are broken:"
    printf '  - %s\n' "${MISSING[@]}"
    exit 1
fi

echo "::notice::All ${TOTAL} manifest entries resolve to existing Gitea repos."
exit 0
