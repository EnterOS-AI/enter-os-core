#!/usr/bin/env bash
# Staging E2E diagnostic — classify peer-visibility token acquisition.
#
# This is intentionally narrower than test_peer_visibility_mcp_staging.sh:
# it provisions the same throwaway org, creates managed sibling workspaces,
# and stops immediately after auth_token acquisition. The default runtime set
# compares hermes with claude-code so a failure is easy to classify:
#   - hermes fails, claude-code passes -> Hermes/runtime-specific
#   - both fail                         -> shared admin/auth/proxy route
#
# Required env matches test_peer_visibility_mcp_staging.sh:
#   MOLECULE_ADMIN_TOKEN
# Optional:
#   MOLECULE_CP_URL, E2E_RUN_ID, PV_RUNTIMES, E2E_KEEP_ORG,
#   E2E_MINIMAX_API_KEY / E2E_ANTHROPIC_API_KEY / E2E_OPENAI_API_KEY

set -euo pipefail

export PV_RUNTIMES="${PV_RUNTIMES:-hermes claude-code}"
export PV_TOKEN_DIAGNOSTIC_ONLY=1

exec "$(dirname "${BASH_SOURCE[0]}")/test_peer_visibility_mcp_staging.sh"
