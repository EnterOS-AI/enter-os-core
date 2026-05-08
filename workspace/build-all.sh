#!/usr/bin/env bash
# build-all.sh — Rebuild base image and optionally adapter images.
#
# NOTE: Adapters have been extracted to standalone template repos:
#   https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-<runtime>
#
# This script now only builds the base image from workspace/Dockerfile.
# Each adapter repo has its own Dockerfile that installs molecule-ai-workspace-runtime
# from PyPI and the adapter-specific deps.
#
# Usage:
#   bash workspace/build-all.sh          # Build base image only
#
# Standalone adapter repos still reference the legacy base image for local dev
# (e.g. FROM workspace-template:base). To build those locally, clone the adapter
# repo and run `docker build -t workspace-template:<runtime> .` from its root.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[build]${NC} $1" >&2; }
err() { echo -e "${RED}[error]${NC} $1" >&2; }

# Build base image
log "Building workspace-template:base ..."
if ! docker build -t workspace-template:base -f Dockerfile . ; then
  err "Base image build failed"
  exit 1
fi
log "Base image built"
log "Done. Adapters are in standalone template repos — see docs/workspace-runtime-package.md"
