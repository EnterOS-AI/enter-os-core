#!/usr/bin/env bash
# jq-install.sh
#
# Library used by .gitea/workflows/review-check-tests.yml (and any
# other workflow that needs jq on a Gitea Actions ubuntu-latest
# runner) to install jq fail-closed: if BOTH the apt-get install
# AND the GitHub-binary fallback fail, the function returns non-zero
# and emits a `::error::` line — so the workflow step fails loud
# and the job fails loud, NOT a silent continue that defers the
# failure to the test step.
#
# Replaces the prior inline `continue-on-error: true` mask in
# review-check-tests.yml (mc#1982) that was root-fixed by core#2460
# (commit 8caff364). That commit removed the `continue-on-error: true`
# on the install step and added `exit 1` on both-fail, but the
# logic stayed embedded in the YAML `run:` block and was therefore
# untestable. This library extracts the logic so the fail-closed
# contract can be unit-tested (see tests/test_jq_install.sh).
#
# IDEMPOTENT: re-sourcing this file is a clean no-op. The deploy
# pipeline may `source` it from multiple job steps; tests source
# it twice on purpose to prove the guard.
if [[ -n "${__JQ_INSTALL_SH_SOURCED:-}" ]]; then
  return 0
fi
__JQ_INSTALL_SH_SOURCED=1

# Tunables (env, with defaults — exposed for tests + future workflows):
#   JQ_INSTALL_VERSION   the jq version to download in the fallback
#                         (default: 1.7.1, matching #2460)
#   JQ_INSTALL_BIN_PATH  where to drop the downloaded binary
#                         (default: /usr/local/bin/jq)
#   JQ_INSTALL_APT_GET   override the apt-get binary (for tests; default: apt-get)
#   JQ_INSTALL_CURL      override the curl binary (for tests; default: curl)
#   JQ_INSTALL_TIMEOUT   curl timeout seconds (default: 120)
#   JQ_INSTALL_DEBUG     set to 1 to print intermediate diagnostics

# install_jq
#
# Try apt-get first; on success, emit a ::notice:: and return 0.
# On apt-get failure, fall back to a GitHub release download via
# curl; on success, emit a ::notice:: and return 0. On BOTH
# failures, emit a ::error:: (NOT ::warning:: — the failure is
# page-on-call, not a heads-up) and return 1.
#
# The function never silently continues past a failure. This is
# the fail-closed contract that #2460 / mc#1982 root-fix
# establishes. A regression that swallows either install failure
# (e.g. by re-adding `continue-on-error: true` upstream, or by
# downgrading the ::error:: to ::warning::) would let the
# review-check.sh regression suite silently "pass" with no jq
# available — the SEV-1 failure mode.
install_jq() {
  local apt_bin="${JQ_INSTALL_APT_GET:-apt-get}"
  local curl_bin="${JQ_INSTALL_CURL:-curl}"
  local jq_version="${JQ_INSTALL_VERSION:-1.7.1}"
  local jq_bin_path="${JQ_INSTALL_BIN_PATH:-/usr/local/bin/jq}"
  local curl_timeout="${JQ_INSTALL_TIMEOUT:-120}"
  local debug="${JQ_INSTALL_DEBUG:-0}"

  if [[ "$debug" == "1" ]]; then
    echo "::debug::install_jq: trying apt-get first" >&2
  fi

  if "$apt_bin" update -qq && "$apt_bin" install -y -qq jq; then
    echo "::notice::jq installed via apt-get: $(jq --version)"
    return 0
  fi

  if [[ "$debug" == "1" ]]; then
    echo "::debug::install_jq: apt-get failed, falling back to GitHub binary" >&2
  fi

  if timeout "$curl_timeout" "$curl_bin" -sSL \
    "https://github.com/jqlang/jq/releases/download/jq-${jq_version}/jq-linux-amd64" \
    -o "$jq_bin_path" && chmod +x "$jq_bin_path"; then
    echo "::notice::jq binary downloaded: $("$jq_bin_path" --version)"
    return 0
  fi

  # BOTH paths failed — fail loud, NOT a warning.
  # (The pre-#2460 emit was `::warning::` + silent continue; that
  # masked install failures and deferred the failure to the test
  # step, making diagnostics harder. See mc#1982 root-fix.)
  echo "::error::jq install failed — apt-get and GitHub download both failed. review-check.sh regression tests cannot run without jq."
  return 1
}
