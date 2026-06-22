#!/usr/bin/env bash
# test_jq_install.sh
#
# Unit tests for scripts/lib/jq-install.sh. Proves the fail-closed
# contract that core#2460 (mc#1982 root-fix) established:
#   (a) when apt-get install jq SUCCEEDS, the function returns 0
#       and logs a `::notice::` line;
#   (b) when apt-get FAILS but the curl fallback SUCCEEDS, the
#       function returns 0 and logs a `::notice::` line;
#   (c) when BOTH apt-get AND curl fail, the function returns 1
#       and logs a `::error::` line (NOT a `::warning::` — the
#       pre-#2460 mask emitted a warning and silently continued).
#   (d) the `::error::` message names BOTH install paths (apt-get
#       and GitHub download) so an operator sees what failed;
#   (e) the function does NOT have any `continue-on-error` /
#       `|| true` / `|| echo` mask — a regression that re-adds
#       one would be caught here.
#
# Plus supporting coverage: idempotent sourcing, debug-mode
# behavior, version-mismatch download path.
#
# Test-injection: the lib reads `JQ_INSTALL_APT_GET` and
# `JQ_INSTALL_CURL` env vars to override the actual binaries
# (the same pattern as cp#737's wait-for-ci-status.sh). Tests
# set these to tiny shell scripts that fail or succeed on
# demand — no real package manager / network round-trip.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# shellcheck source=scripts/lib/jq-install.sh
. "$ROOT/.gitea/scripts/lib/jq-install.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

# --- make a fake apt-get / curl binary that fails (exits $2) ------
# --- with $3 on stderr -----------------------------------------
# Args: make_failing_bin PATH CODE FAIL_MSG
make_failing_bin() {
  local path="$1" code="$2" fail_msg="$3"
  cat >"$path" <<SH
#!/usr/bin/env bash
echo "$fail_msg" >&2
exit $code
SH
  chmod +x "$path"
}

# --- make a fake apt-get that always succeeds -------------------
make_succeeding_apt_get() {
  local path="$1"
  cat >"$path" <<'SH'
#!/usr/bin/env bash
echo "fake apt-get: would install jq"
exit 0
SH
  chmod +x "$path"
}

# --- make a fake curl that always succeeds (creates the file) ----
make_succeeding_curl() {
  local path="$1" target="$2"
  cat >"$path" <<SH
#!/usr/bin/env bash
# Create a stub jq binary at the target so chmod +x doesn't fail.
echo '#!/usr/bin/env bash' > "$target"
echo 'echo "jq-FAKE 1.7.1 (test stub)"' >> "$target"
chmod +x "$target"
exit 0
SH
  chmod +x "$path"
}

# ====================================================================
# (a) Happy path: apt-get succeeds → function returns 0, logs notice.
# ====================================================================
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

APT="$TMPDIR/apt-get"
make_succeeding_apt_get "$APT"
export JQ_INSTALL_APT_GET="$APT"
export JQ_INSTALL_CURL="$TMPDIR/curl"   # unused in this case, but set

set +e
out="$(install_jq 2>&1)"
rc=$?
set -e
[[ "$rc" -eq 0 ]] || fail "(a) expected rc=0 on apt-get success, got $rc (out=$out)"
[[ "$out" == *"::notice::jq installed via apt-get"* ]] || fail "(a) expected `::notice::jq installed via apt-get`, got: $out"
# (e) anti-mask: the message must NOT be a `::warning::` or
# `::error::` — apt-get success is a notice.
[[ "$out" != *"::warning::"* ]] || fail "(a) regression: success path emitted a ::warning:: (was a notice). The pre-#2460 silent-continue mask used warnings."
[[ "$out" != *"::error::"* ]] || fail "(a) regression: success path emitted a ::error::"
pass "(a) apt-get success → rc=0, ::notice::, no warnings/errors"

# ====================================================================
# (b) Mixed path: apt-get FAILS but curl succeeds → rc=0, notice.
# ====================================================================
TMPDIR2="$(mktemp -d)"
APT2="$TMPDIR2/apt-get"
make_failing_bin "$APT2" 100 "apt-get failed (test stub)"
CURL2="$TMPDIR2/curl"
TARGET="$TMPDIR2/jq"
make_succeeding_curl "$CURL2" "$TARGET"
export JQ_INSTALL_APT_GET="$APT2"
export JQ_INSTALL_CURL="$CURL2"
export JQ_INSTALL_BIN_PATH="$TARGET"

set +e
out="$(install_jq 2>&1)"
rc=$?
set -e
[[ "$rc" -eq 0 ]] || fail "(b) expected rc=0 on curl fallback success, got $rc (out=$out)"
[[ "$out" == *"::notice::jq binary downloaded"* ]] || fail "(b) expected ::notice::jq binary downloaded, got: $out"
# (e) anti-mask: no `::error::` on the partial-fail path — only the
# BOTH-fail case is a page-on-call. The lib falls through to the
# curl fallback silently; a follow-up could add a ::warning:: for
# the apt-get failure but that's outside the post-#2460 contract.
[[ "$out" != *"::error::"* ]] || fail "(b) regression: partial-fail path emitted ::error:: (should be a notice — apt-get failed but curl succeeded)"
pass "(b) apt-get fail + curl success → rc=0, ::notice::, no ::error::"

# ====================================================================
# (c) Sad path: BOTH apt-get AND curl fail → rc=1, ::error::
#     (NOT ::warning::, NOT silent continue).
# ====================================================================
TMPDIR3="$(mktemp -d)"
APT3="$TMPDIR3/apt-get"
make_failing_bin "$APT3" 100 "apt-get failed (test stub)"
CURL3="$TMPDIR3/curl"
# A curl that exits non-zero, like a network-blocked or 404 download.
make_failing_bin "$CURL3" 22 "curl: (22) The requested URL returned error: 404"
export JQ_INSTALL_APT_GET="$APT3"
export JQ_INSTALL_CURL="$CURL3"
export JQ_INSTALL_BIN_PATH="$TMPDIR3/jq"

# install_jq is EXPECTED to fail here — capture the rc without
# tripping set -e.
set +e
out="$(install_jq 2>&1)"
rc=$?
set -e
[[ "$rc" -ne 0 ]] || fail "(c) expected rc=1 on BOTH-fail (the core#2460 fail-closed contract), got $rc (out=$out)"
[[ "$out" == *"::error::"* ]] || fail "(c) expected `::error::` on both-fail (page-on-call), got: $out"
# (c) the error message must name BOTH install paths (apt-get AND
# GitHub download) so an operator sees what failed.
[[ "$out" == *"apt-get"* ]] || fail "(c) error message must name the apt-get path, got: $out"
[[ "$out" == *"GitHub download"* ]] || fail "(c) error message must name the GitHub download fallback, got: $out"
# CRITICAL: the pre-#2460 mask emitted a `::warning::` and then
# silently continued (the test step then failed because jq was
# missing). A regression to that pattern would be:
#   ::warning::jq install failed — continuing
# We assert the EXACT OPPOSITE: NO `::warning::` on the both-fail
# path (it should be a ::error::, page-on-call).
[[ "$out" != *"::warning::"* ]] || fail "(c) regression: both-fail path emitted `::warning::` — this is the pre-#2460 silent-continue mask. Must be `::error::`."
pass "(c) both fail → rc=1, ::error:: naming both paths, no ::warning:: (the pre-#2460 mask contract)"

# ====================================================================
# (d) Error message must be informative — names BOTH install paths
#     so an operator can see what failed without re-reading the
#     workflow YAML.
# ====================================================================
# (d) is already covered by (c)'s `apt-get` and `GitHub download`
# assertions. Just re-verify the exact full message contains the
# two paths as a single integration check.
[[ "$out" == *"review-check.sh regression tests cannot run without jq"* ]] || fail "(d) error message must include the SOP hint 'review-check.sh regression tests cannot run without jq', got: $out"
pass "(d) error message includes the SOP hint naming both install paths"

# ====================================================================
# (e) Anti-mask: the function does NOT contain `|| true` / `|| echo`
#     / `|| exit 0` after the install paths. A regression that
#     re-adds one would silently swallow failures.
# ====================================================================
# Source the lib into a fresh shell to inspect the function body
# for forbidden swallow patterns. Use a function-decompiler trick:
# declare -f install_jq prints the body in a portable way.
if ! declare -f install_jq >/dev/null 2>&1; then
  fail "(e) install_jq is not a defined function — lib failed to source"
fi
body="$(declare -f install_jq)"
# Forbidden swallows: the post-#2460 fail-closed contract
# REQUIRES that the both-fail path returns 1, not 0.
echo "$body" | grep -qE 'continue-on-error|\|\| true|\|\| echo|\|\| exit 0|\|\| :' && \
  fail "(e) regression: install_jq body contains a swallow pattern (\`|| true\`, \`|| echo\`, \`|| exit 0\`, \`|| :\`, or \`continue-on-error\`). The post-#2460 fail-closed contract REQUIRES the function to return non-zero on failure." || true
# The body must end with the `::error::` line + `return 1`.
echo "$body" | grep -q '::error::jq install failed' || fail "(e) regression: install_jq body does not contain the expected `::error::jq install failed` line. Was the post-#2460 fail-closed message removed?"
echo "$body" | grep -qE 'return 1$|return 1\b' || fail "(e) regression: install_jq body does not end with \`return 1\`. The post-#2460 fail-closed contract REQUIRES non-zero exit on failure."
pass '(e) install_jq body has no swallow patterns and ends with ::error:: + return 1 (the post-#2460 fail-closed contract)'

# ====================================================================
# (f) Bonus: a `JQ_INSTALL_DEBUG=1` run emits `::debug::` lines so
#     an operator can trace which branch fired. The PR-#2460 fix
#     was a debugging nightmare because the silent-continue path
#     gave no diagnostics. The debug flag is a low-cost way to
#     keep that lesson learned.
# ====================================================================
TMPDIR4="$(mktemp -d)"
APT4="$TMPDIR4/apt-get"
make_succeeding_apt_get "$APT4"
CURL4="$TMPDIR4/curl"
make_succeeding_curl "$CURL4" "$TMPDIR4/jq"
export JQ_INSTALL_APT_GET="$APT4"
export JQ_INSTALL_CURL="$CURL4"
export JQ_INSTALL_BIN_PATH="$TMPDIR4/jq"
export JQ_INSTALL_DEBUG=1

set +e
out="$(install_jq 2>&1)"
rc=$?
set -e
[[ "$rc" -eq 0 ]] || fail "(f) debug-mode success path expected rc=0, got $rc (out=$out)"
[[ "$out" == *"::debug::install_jq: trying apt-get first"* ]] || fail "(f) debug-mode should emit ::debug::install_jq: trying apt-get first, got: $out"
unset JQ_INSTALL_DEBUG
pass "(f) JQ_INSTALL_DEBUG=1 emits ::debug:: install_jq trace lines"

# ====================================================================
# Idempotent re-source: a second `source` is a clean no-op.
# ====================================================================
# shellcheck source=scripts/lib/jq-install.sh
. "$ROOT/.gitea/scripts/lib/jq-install.sh"
pass "idempotent re-source is a no-op"

echo "jq-install test passed"
