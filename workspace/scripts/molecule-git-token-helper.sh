#!/bin/bash
# molecule-git-token-helper.sh — git credential helper for GitHub App tokens
#
# Fetches a fresh GitHub App installation token from the Molecule AI
# platform endpoint and caches it locally (~50 min), so workspace
# containers never use an expired GH_TOKEN after the ~60 min GitHub App
# token TTL.  The cache avoids hitting the platform API on every git
# operation (push/fetch/clone).
#
# # Setup (called once at container boot by entrypoint.sh)
#
#   git config --global \
#     "credential.https://github.com.helper" \
#     "!/app/scripts/molecule-git-token-helper.sh"
#
# # How git calls this helper
#
# git passes the action as the first positional arg.  The protocol is:
#   get   → output credentials on stdout (we handle this)
#   store → persist credentials (no-op — we never cache via git)
#   erase → revoke credentials (no-op — platform manages lifecycle)
#
# On `get`, git reads key=value pairs terminated by an empty line.
# We must emit at minimum:
#   username=x-access-token
#   password=<token>
#   (blank line)
#
# # Auth
#
# The platform endpoint requires a valid workspace bearer token.  The
# token is stored at ${CONFIGS_DIR}/.auth_token (written by platform_auth.py
# on first /registry/register).  Workspace env var PLATFORM_URL defaults
# to http://platform:8080.
#
# # Caching
#
# Tokens are cached at ${CACHE_DIR}/gh_installation_token with a
# companion ${CACHE_DIR}/gh_installation_token_expiry file containing
# the epoch-seconds expiry.  Cache TTL is ~50 min (TOKEN_CACHE_TTL_SEC).
# If the cache is fresh, we return immediately without calling the API.
#
# # Fallback chain
#
# 1. Return cached token if not expired.
# 2. Fetch fresh token from platform API.
# 3. If platform is unreachable, fall back to GITHUB_TOKEN / GH_TOKEN
#    env var (set at container start, valid for up to 60 min).
# 4. If env is unset, fall back to ${CONFIGS_DIR:-/configs}/.github-token
#    static token file (operator-placed PAT as incident workaround).
#    Empty file rejected; whitespace stripped before use.
#    Written by operator into the agent-writable /configs dir so
#    no root and no platform restart needed to activate.
#    Both _fetch_token (git path) and _refresh_gh (gh CLI path) use
#    this fallback — otherwise git would work but gh auth status would
#    still be unauthenticated post-incident.
# 5. If all fail, exit 1 so git falls through to the next credential
#    helper in the chain (if any).
#
# # gh CLI integration
#
# Use the _refresh_gh action to atomically refresh both the cache and
# gh CLI auth:
#
#   bash /app/scripts/molecule-git-token-helper.sh _refresh_gh
#
# This is called by molecule-gh-token-refresh.sh (the background daemon)
# every 45 min.
#
set -euo pipefail

PLATFORM_URL="${PLATFORM_URL:-http://host.docker.internal:8080}"
CONFIGS_DIR="${CONFIGS_DIR:-/configs}"
TOKEN_FILE="${CONFIGS_DIR}/.auth_token"

# Cache location — writable by agent user
CACHE_DIR="${HOME:=/home/agent}/.molecule-token-cache"
CACHE_TOKEN_FILE="${CACHE_DIR}/gh_installation_token"
CACHE_EXPIRY_FILE="${CACHE_DIR}/gh_installation_token_expiry"

# Cache lifetime: 50 min = 3000 sec.  Installation tokens last ~60 min;
# 50 min gives a 10-min safety margin for clock skew + in-flight ops.
TOKEN_CACHE_TTL_SEC=3000

# #1068: use workspace-scoped path (WorkspaceAuth) instead of admin path
# (AdminAuth rejects workspace bearer tokens since PR #729).
WORKSPACE_ID="${WORKSPACE_ID:-}"
if [ -n "$WORKSPACE_ID" ]; then
    ENDPOINT="${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/github-installation-token"
else
    ENDPOINT="${PLATFORM_URL}/admin/github-installation-token"
fi

# _now_epoch — portable epoch-seconds (works on both GNU and BusyBox date).
_now_epoch() {
    date +%s
}

# _read_cache — output cached token if still valid; return 1 if stale/missing.
_read_cache() {
    if [ ! -f "${CACHE_TOKEN_FILE}" ] || [ ! -f "${CACHE_EXPIRY_FILE}" ]; then
        return 1
    fi
    expiry=$(cat "${CACHE_EXPIRY_FILE}" 2>/dev/null | tr -d '[:space:]')
    if [ -z "${expiry}" ]; then
        return 1
    fi
    now=$(_now_epoch)
    if [ "${now}" -ge "${expiry}" ]; then
        return 1
    fi
    token=$(cat "${CACHE_TOKEN_FILE}" 2>/dev/null | tr -d '[:space:]')
    if [ -z "${token}" ]; then
        return 1
    fi
    echo "${token}"
    return 0
}

# _write_cache — atomically persist token + expiry.
#
# Hardened per #1552:
#  - umask 077 around the writes so .tmp files are 600 from creation,
#    closing the TOCTOU window where a concurrent reader could read
#    the token while it was still mode 644 (between the create-with-
#    default-umask and the later chmod 600).
#  - Don't swallow chmod errors with `|| true`. A chmod failure leaves
#    tokens potentially world-readable; surface it as a WARN line so
#    ops can grep `[molecule-git-token-helper] WARN` and see real
#    permission failures instead of silent 644 files.
_write_cache() {
    local token="$1"
    mkdir -p "${CACHE_DIR}"
    if ! chmod 700 "${CACHE_DIR}" 2>/dev/null; then
        echo "[molecule-git-token-helper] WARN: failed to chmod 700 ${CACHE_DIR} — cache dir may be world-readable" >&2
    fi
    now=$(_now_epoch)
    expiry=$((now + TOKEN_CACHE_TTL_SEC))

    # Restrictive umask so the .tmp files are 600 from creation. Restored
    # before return so callers' umask isn't perturbed.
    local prev_umask
    prev_umask=$(umask)
    umask 077

    # Write atomically via tmp + mv to avoid partial reads.
    printf '%s' "${token}" > "${CACHE_TOKEN_FILE}.tmp"
    printf '%s' "${expiry}" > "${CACHE_EXPIRY_FILE}.tmp"
    mv -f "${CACHE_TOKEN_FILE}.tmp" "${CACHE_TOKEN_FILE}"
    mv -f "${CACHE_EXPIRY_FILE}.tmp" "${CACHE_EXPIRY_FILE}"

    umask "${prev_umask}"

    # Belt-and-suspenders chmod — umask 077 should make the files 600
    # already, but a chmod that fails on the post-rename file is itself
    # a real signal worth surfacing.
    if ! chmod 600 "${CACHE_TOKEN_FILE}" "${CACHE_EXPIRY_FILE}" 2>/dev/null; then
        echo "[molecule-git-token-helper] WARN: chmod 600 failed on cache files — token may be world-readable" >&2
    fi
}

# _fetch_token_from_api — hit the platform endpoint.
# Outputs the raw token string on success; returns non-zero on failure.
_fetch_token_from_api() {
    if [ ! -f "${TOKEN_FILE}" ]; then
        echo "[molecule-git-token-helper] .auth_token not found at ${TOKEN_FILE}" >&2
        return 1
    fi

    bearer=$(cat "${TOKEN_FILE}" | tr -d '[:space:]')
    if [ -z "${bearer}" ]; then
        echo "[molecule-git-token-helper] .auth_token is empty" >&2
        return 1
    fi

    # NOTE: capture stderr to a tmp file (NOT $response) so the response
    # body — which contains the token on success — never lands in error
    # log lines via $response interpolation.
    local _err_file
    _err_file=$(mktemp)
    response=$(curl -sf \
        -H "Authorization: Bearer ${bearer}" \
        -H "Accept: application/json" \
        --max-time 10 \
        "${ENDPOINT}" 2>"${_err_file}") || {
        local _curl_rc=$?
        local _err_msg
        _err_msg=$(cat "${_err_file}")
        rm -f "${_err_file}"
        echo "[molecule-git-token-helper] platform request failed (curl rc=${_curl_rc}): ${_err_msg}" >&2
        return 1
    }
    rm -f "${_err_file}"

    # Parse {"token":"ghs_...","expires_at":"..."} with sed (no jq dependency).
    token=$(echo "${response}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    if [ -z "${token}" ]; then
        # SECURITY: the response body MAY contain a token under a different
        # JSON key name. Never include $response in this error message —
        # log only the size as a coarse debugging signal.
        echo "[molecule-git-token-helper] empty token in platform response (body=${#response} bytes)" >&2
        return 1
    fi

    echo "${token}"
}

# _fetch_token — return a fresh token using cache > API > env > static fallback chain.
# Outputs the raw token string on success; exits non-zero if all sources fail.
_fetch_token() {
    # 1. Try cache first.
    cached=$(_read_cache) && {
        echo "${cached}"
        return 0
    }

    # 2. Fetch from platform API.
    api_token=$(_fetch_token_from_api 2>/dev/null) && {
        _write_cache "${api_token}"
        echo "${api_token}"
        return 0
    }

    # 3. Fall back to env var (set at container start, may be stale but
    #    better than nothing for the first ~60 min of container life).
    env_token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
    if [ -n "${env_token}" ]; then
        echo "[molecule-git-token-helper] API unreachable, falling back to env GITHUB_TOKEN" >&2
        echo "${env_token}"
        return 0
    fi

    # 4. Static token fallback — operator-placed PAT in the agent-writable
    #    configs dir. Written without root; no platform restart needed.
    #    Both this helper and _refresh_gh use the same fallback so git
    #    and gh both recover from a platform outage.
    static_token_file="${CONFIGS_DIR:-/configs}/.github-token"
    if [ -f "${static_token_file}" ]; then
        static_token=$(tr -d '[:space:]' < "${static_token_file}")
        if [ -n "${static_token}" ]; then
            echo "[molecule-git-token-helper] API + env unreachable, falling back to static .github-token" >&2
            echo "${static_token}"
            return 0
        fi
    fi

    echo "[molecule-git-token-helper] all token sources exhausted" >&2
    return 1
}

ACTION="${1:-get}"

case "${ACTION}" in
    get)
        token=$(_fetch_token) || exit 1
        # Emit git credential protocol response.
        printf 'username=x-access-token\n'
        printf 'password=%s\n' "${token}"
        printf '\n'
        ;;
    store|erase)
        # No-op — the platform manages token lifecycle.
        ;;
    _fetch_token)
        # Return raw token (cache > API > env > static fallback).
        _fetch_token
        ;;
    _refresh_gh)
        # Refresh cache AND update gh CLI auth in one shot.
        # Called by molecule-gh-token-refresh.sh background daemon.
        # Force-bypass cache to get a definitely fresh token.
        #
        # Chain: API > static fallback. Env is deliberately excluded here —
        # _refresh_gh is a background daemon that re-runs every 30 min;
        # if we used the env fallback on every cycle the gh CLI would stay
        # stuck on a stale env token instead of recovering when the API
        # comes back. Static fallback is intentionally operator-activated
        # only (file presence gates it).
        api_token=$(_fetch_token_from_api) || {
            # API down — try static token fallback.
            static_token_file="${CONFIGS_DIR:-/configs}/.github-token"
            if [ -f "${static_token_file}" ]; then
                static_token=$(tr -d '[:space:]' < "${static_token_file}")
                if [ -n "${static_token}" ]; then
                    echo "[molecule-git-token-helper] _refresh_gh: API unreachable, using static .github-token" >&2
                    _write_cache "${static_token}"
                    echo "${static_token}" | gh auth login --hostname github.com --with-token 2>/dev/null || {
                        echo "[molecule-git-token-helper] _refresh_gh: gh auth login with static token failed (non-fatal)" >&2
                    }
                    echo "[molecule-git-token-helper] _refresh_gh: static token used successfully" >&2
                    return 0
                fi
            fi
            echo "[molecule-git-token-helper] _refresh_gh: API fetch failed and no static fallback" >&2
            exit 1
        }
        _write_cache "${api_token}"
        # Update gh CLI auth — gh auth login reads token from stdin.
        echo "${api_token}" | gh auth login --hostname github.com --with-token 2>/dev/null || {
            echo "[molecule-git-token-helper] _refresh_gh: gh auth login failed (non-fatal)" >&2
        }
        # Also update GH_TOKEN file for scripts that source it.
        # Same #1552 hardening as _write_cache — umask 077 around the
        # write so the .tmp file is 600 from creation, and surface a
        # WARN on chmod failure instead of swallowing it.
        gh_token_file="${HOME}/.gh_token"
        # `local` is illegal here (top-level case branch, not a
        # function); shadow with a uniquely-named global instead.
        _gh_prev_umask=$(umask)
        umask 077
        printf '%s' "${api_token}" > "${gh_token_file}.tmp"
        mv -f "${gh_token_file}.tmp" "${gh_token_file}"
        umask "${_gh_prev_umask}"
        unset _gh_prev_umask
        if ! chmod 600 "${gh_token_file}" 2>/dev/null; then
            echo "[molecule-git-token-helper] WARN: chmod 600 failed on ${gh_token_file} — token may be world-readable" >&2
        fi
        echo "[molecule-git-token-helper] _refresh_gh: token refreshed successfully" >&2
        ;;
    _invalidate_cache)
        # Force next call to hit the API (useful after a 401).
        rm -f "${CACHE_TOKEN_FILE}" "${CACHE_EXPIRY_FILE}" 2>/dev/null
        ;;
    *)
        echo "[molecule-git-token-helper] unknown action: ${ACTION}" >&2
        exit 1
        ;;
esac
