# Tiny stub runtime image for the local Docker-provisioner lifecycle e2e.
#
# It impersonates a real workspace runtime's platform contract (register +
# heartbeat + A2A message/send) with ZERO LLM/SDK weight so the lifecycle e2e
# (provision -> online -> restart-survive -> proxy-reach) runs in seconds and
# without the 2.5GB real claude-code image.
#
# Resolution trick (see tests/e2e/test_local_provision_lifecycle_e2e.sh):
# the local provisioner resolves runtime=claude-code via RegistryModeLocal,
# which is a `docker image inspect` cache-check on
#   molecule-local/workspace-template-claude-code:<gitea-HEAD-sha12>
# BEFORE it clones+builds. Pre-tagging THIS image to that exact cache tag makes
# the provisioner cache-hit the stub instead of building the real template.
#
# linux/amd64: the provisioner forces --platform=linux/amd64 for every workspace
# container (defaultImagePlatform, #1875) for parity with the amd64-only prod
# images. Build the stub amd64 too so the platforms match and Docker doesn't
# refuse the create with a manifest mismatch.
FROM --platform=linux/amd64 python:3.12-alpine

# /configs is the named-volume mount point the provisioner attaches
# (ws-<id>-configs:/configs). The real entrypoint chowns it; the stub just
# needs the dir to exist so a missing-mount never trips it up.
RUN mkdir -p /configs /workspace

WORKDIR /app
COPY server.py /app/server.py

EXPOSE 8000

# No gosu/agent-uid drop here — the stub does no privileged work and the e2e
# only cares about the platform contract, not the agent-uid security posture.
ENTRYPOINT ["python3", "/app/server.py"]
