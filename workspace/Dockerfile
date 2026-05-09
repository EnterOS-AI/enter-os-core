FROM python:3.11-slim@sha256:e78299e55776ca065dcb769f80161f48465ad352014240eb5fe4712e22505e9b

WORKDIR /app

# Install Node.js, git, gh CLI in a single layer to minimize image size
RUN apt-get update && \
    apt-get install -y --no-install-recommends curl git ca-certificates && \
    # Node.js 22
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y --no-install-recommends nodejs && \
    # GitHub CLI
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list && \
    apt-get update && apt-get install -y --no-install-recommends gh && \
    # Cleanup apt caches and temp files
    apt-get purge -y --auto-remove -o APT::AutoRemove::RecommendsImportant=false && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Create non-root user (claude --dangerously-skip-permissions refuses root)
RUN useradd -m -s /bin/bash agent

# Install base Python dependencies (A2A SDK + HTTP only)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy runtime code (adapters/ has been removed — adapters now live in standalone
# template repos and install molecule-ai-workspace-runtime from PyPI)
COPY *.py ./
COPY entrypoint.sh ./
COPY skill_loader/ ./skill_loader/
COPY builtin_tools/ ./builtin_tools/
COPY plugins_registry/ ./plugins_registry/
COPY policies/ ./policies/

# Create CLI aliases
RUN ln -s /app/a2a_cli.py /usr/local/bin/a2a && chmod +x /app/a2a_cli.py /app/a2a_mcp_server.py && \
    ln -s /app/molecule_ai_status.py /usr/local/bin/molecule-monorepo-status && chmod +x /app/molecule_ai_status.py

# gh wrapper — auto-prefixes PR / issue titles with the agent role + appends
# a body footer. Every agent in the template shares one GitHub PAT so plain
# `gh pr list` can't distinguish workspaces; the wrapper reads GIT_AUTHOR_NAME
# (set by the platform provisioner, "Molecule AI <Role>") and rewrites the
# title/body accordingly. Fails open when the env is missing. Anything that
# isn't `gh pr create` or `gh issue create` passes through untouched.
# /usr/local/bin is earlier in PATH than /usr/bin/gh so this shadows the
# real binary without renaming it.
COPY scripts/gh-wrapper.sh /usr/local/bin/gh
RUN chmod +x /usr/local/bin/gh

# Copy the git credential helper so entrypoint.sh can register it at boot.
# molecule-git-token-helper.sh fetches a fresh GitHub App installation token
# from the platform on every git push/fetch, preventing stale-token failures
# after the ~60 min GitHub App token TTL (issue #613 / #547).
COPY scripts/molecule-git-token-helper.sh ./scripts/
RUN chmod +x ./scripts/molecule-git-token-helper.sh

# Copy the background token refresh daemon. Runs as a background process
# started by entrypoint.sh — refreshes gh CLI auth and the credential
# helper cache every 45 min so tokens never expire mid-operation.
COPY scripts/molecule-gh-token-refresh.sh ./scripts/
RUN chmod +x ./scripts/molecule-gh-token-refresh.sh

# Dirs and permissions
RUN mkdir -p /workspace /plugins /home/agent/.claude /home/agent/.config /home/agent/.local \
    /home/agent/.molecule-token-cache && \
    chown -R agent:agent /app /home/agent /workspace

# Install gosu for clean root → agent user handoff in entrypoint.
# The entrypoint starts as root to fix volume ownership, then exec's
# as the agent user so Claude Code's --dangerously-skip-permissions works.
RUN apt-get update && apt-get install -y --no-install-recommends gosu && \
    rm -rf /var/lib/apt/lists/*

VOLUME /configs
VOLUME /workspace

EXPOSE 8000
RUN chmod +x /app/entrypoint.sh
# Start as root — entrypoint fixes volume permissions then drops to agent
CMD ["./entrypoint.sh"]
