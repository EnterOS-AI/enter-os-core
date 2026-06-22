#!/usr/bin/env bash
# Build all workspace runtime images
# Base image first, then each adapter extends it
set -e

cd "$(dirname "$0")/../workspace-template"

echo "=== Building base image ==="
docker build -t workspace-template:base -t workspace-template:latest .

for adapter in claude_code codex hermes openclaw; do
  DOCKERFILE="adapters/${adapter}/Dockerfile"
  if [ ! -f "$DOCKERFILE" ]; then
    echo "Skipping $adapter (no Dockerfile)"
    continue
  fi

  # Image tag uses dashes (claude_code -> claude-code)
  TAG=$(echo "$adapter" | tr '_' '-')
  echo ""
  echo "=== Building workspace-template:${TAG} ==="
  docker build -t "workspace-template:${TAG}" -f "$DOCKERFILE" .
done

echo ""
echo "=== All images built ==="
docker images workspace-template --format "{{.Tag}}\t{{.Size}}"
