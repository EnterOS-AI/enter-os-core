# Provisioning Workspaces on Fly Machines (CONTAINER_BACKEND=flyio)

Molecule AI can provision agent workspaces on [Fly Machines](https://fly.io/docs/machines/) instead of local Docker containers. When `CONTAINER_BACKEND=flyio` is set, every `POST /workspaces` creates a Fly Machine and boots the workspace agent inside it — with tier-based resource limits, env-var injection, and A2A registration handled automatically. The platform manages the workspace (lifecycle, auth, routing); Fly manages the machine it runs on.

> **Scope note (PR #501):** Workspace images must already be published to GHCR before provisioning. The `delete` and `restart` platform endpoints are not yet fully wired to the Fly provisioner — use `flyctl machine stop/destroy` for teardown until a follow-up PR lands.

## What you'll need

- A Molecule AI platform instance
- A [Fly.io](https://fly.io) account with a Fly app created for workspace machines
- `flyctl` installed locally
- `curl` + `jq`

## Setup

```bash
# 1. Set CONTAINER_BACKEND and Fly credentials on your platform process
#    (add to your platform's .env or deployment config)
export CONTAINER_BACKEND=flyio
export FLY_API_TOKEN=<your-fly-deploy-token>      # flyctl tokens create deploy
export FLY_WORKSPACE_APP=my-molecule-workspaces   # fly app created for this purpose
export FLY_REGION=ord                             # optional, default: ord

# 2. Restart the platform so it picks up CONTAINER_BACKEND=flyio
#    (varies by your deployment — docker restart, systemd reload, etc.)

# 3. Verify the platform is using the Fly provisioner
curl -s http://localhost:8080/healthz | jq .

# 4. Create a workspace — the platform provisions it as a Fly Machine
WS=$(curl -s -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -d '{
    "name": "fly-worker",
    "role": "Fly-provisioned inference worker",
    "runtime": "hermes",
    "tier": 2
  }' | jq -r '.id')
echo "Workspace ID: $WS"

# 5. Watch the Fly Machine appear (~15–30s)
flyctl machines list --app $FLY_WORKSPACE_APP

# 6. Poll until the workspace is ready
until curl -s http://localhost:8080/workspaces/$WS | jq -r '.status' | grep -q ready; do
  echo "Waiting..."; sleep 5
done

# 7. Smoke test — send an A2A task
curl -s -X POST http://localhost:8080/workspaces/$WS/a2a \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"1","method":"message/send",
       "params":{"message":{"role":"user","parts":[{"kind":"text",
       "text":"What region are you running in?"}]}}}' \
  | jq '.result.parts[0].text'

# 8. Inspect the Fly Machine details
flyctl machines show --app $FLY_WORKSPACE_APP

# 9. Teardown (see scope note — use flyctl directly for now)
flyctl machines destroy --app $FLY_WORKSPACE_APP --force
```

## Expected output

Step 5 (`flyctl machines list`) shows the new machine with a `started` state within ~30 seconds. The platform injects your workspace secrets, `PLATFORM_URL`, and workspace ID as environment variables on the machine, then issues an auth token so the agent registers on boot.

Step 7 returns the agent's reply — proof that A2A JSON-RPC is routing through the Fly Machine correctly. The `FLY_REGION` env var is visible inside the container, so asking the agent "What region are you running in?" should return `ord` (or whichever region you set).

## Resource tiers

The Fly provisioner applies tier-based limits automatically — no manual machine sizing needed:

| Tier | RAM | CPUs | Use case |
|------|-----|------|----------|
| T2 | 512 MB | 1 | Light workers, eval agents |
| T3 | 2 GB | 2 | General-purpose orchestrators |
| T4 | 4 GB | 4 | Heavy inference, long-context tasks |

Set `"tier": 2`, `3`, or `4` in your `POST /workspaces` body. Runtime images are resolved from GHCR automatically (`hermes` → `ghcr.io/molecule-ai/workspace-hermes:latest`).

## Why Fly Machines

Fly Machines start in milliseconds and run in 35+ regions. Provisioning agent workspaces on Fly means your inference workers can live close to your users with no infrastructure code changes — just set `FLY_REGION` per workspace. Because the Fly provisioner implements the same `Provisioner` interface as the Docker backend, the rest of the platform is unchanged: same REST API, same A2A protocol, same workspace management UI.

## Related

- PR #501: [feat(platform): Fly Machines provisioner](https://git.moleculesai.app/molecule-ai/molecule-core/pull/501)
- PR #481: [feat(ci): deploy to Fly after image push](https://git.moleculesai.app/molecule-ai/molecule-core/pull/481)
- [Fly Machines API docs](https://fly.io/docs/machines/api/)
- [Platform API reference](../api-reference.md)
- Issue [#525](https://git.moleculesai.app/molecule-ai/molecule-core/issues/525)
