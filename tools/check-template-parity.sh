#!/usr/bin/env bash
# check-template-parity.sh — enforce parity between a workspace template's
# install.sh (bare-host / EC2 path) and start.sh (Docker path). Both scripts
# must forward the same set of provider API keys to the agent's .env so that
# a workspace built on one backend behaves identically to a workspace built
# on the other.
#
# Drift this catches:
#   - Someone adds HERMES_API_KEY to start.sh but forgets install.sh.
#     EC2 workspaces using Nous fail silently; Docker works.
#   - Someone adds a HERMES_CUSTOM_BASE_URL branch to install.sh only.
#     Docker can't use a custom OpenAI-compat endpoint; EC2 can.
#
# Invocation (from template-hermes repo's CI):
#
#     bash /path/to/molecule-monorepo/tools/check-template-parity.sh \
#          install.sh start.sh
#
# Or inline via curl:
#
#     bash <(curl -fsSL https://git.moleculesai.app/molecule-ai/molecule-core/raw/branch/main/tools/check-template-parity.sh) \
#          install.sh start.sh
#
# Exit codes:
#   0 — parity ok (or both files declare the same set of ${VAR:+VAR=...} exports)
#   1 — drift detected (emits a diff to stderr)
#   2 — usage / missing files
#
# What "parity" means here: the SET of environment-variable forwarders
# (lines of the form `${VAR:+VAR=${VAR}}`) in each file must be equal.
# The ordering, surrounding comments, and non-forwarder lines are free to
# differ — that's where the two paths legitimately diverge (bare-host vs
# Docker-entrypoint structure).

set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 install.sh start.sh" >&2
  exit 2
fi

INSTALL_SH="$1"
START_SH="$2"

for f in "$INSTALL_SH" "$START_SH"; do
  if [ ! -f "$f" ]; then
    echo "missing file: $f" >&2
    exit 2
  fi
done

# Extract the set of ${VAR:+VAR=...} forwarder lines, stripped of
# surrounding whitespace. sort -u gives us the set to compare.
extract_forwarders() {
  grep -oE '\$\{[A-Z_]+:\+[A-Z_]+=\$\{[A-Z_]+\}\}' "$1" 2>/dev/null | sort -u
}

TMP_INSTALL=$(mktemp)
TMP_START=$(mktemp)
trap 'rm -f "$TMP_INSTALL" "$TMP_START"' EXIT

extract_forwarders "$INSTALL_SH" > "$TMP_INSTALL"
extract_forwarders "$START_SH"   > "$TMP_START"

if diff -q "$TMP_INSTALL" "$TMP_START" > /dev/null; then
  COUNT=$(wc -l < "$TMP_INSTALL" | tr -d ' ')
  echo "template-parity: ok ($COUNT provider forwarders in both files)"
  exit 0
fi

echo "template-parity: DRIFT detected between $INSTALL_SH and $START_SH" >&2
echo >&2
echo "--- forwarders only in $INSTALL_SH ---" >&2
comm -23 "$TMP_INSTALL" "$TMP_START" | sed 's/^/  /' >&2
echo "--- forwarders only in $START_SH ---" >&2
comm -13 "$TMP_INSTALL" "$TMP_START" | sed 's/^/  /' >&2
echo >&2
echo "Fix: copy the missing forwarder lines so both files carry the same set." >&2
echo "Rationale: workspace-backend parity — see docs/architecture/backends.md" >&2
exit 1
