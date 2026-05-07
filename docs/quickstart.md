# Quickstart Guide

This path is aligned to the current repository and current UI. It gets you from clone to a live workspace on the canvas without assuming any extra platform wrapper.

## Prerequisites

- Docker + Docker Compose v2
- Node.js 20+
- Go 1.25+
- `jq` (for the template-registry clone in `setup.sh`)
- One model/API key for the runtime you want to use
  - `ANTHROPIC_API_KEY`
  - `OPENAI_API_KEY`
  - `GOOGLE_API_KEY`
  - or another provider routed through LiteLLM

## The one-command path

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-monorepo.git
cd molecule-monorepo
./scripts/dev-start.sh
```

That single script:

1. Generates an `ADMIN_TOKEN` into `.env` (first run only — preserved on re-runs)
2. Brings up Postgres, Redis, Langfuse, ClickHouse, and Temporal via `infra/scripts/setup.sh`
3. Populates the workspace template + plugin registry from `manifest.json`
4. Builds and starts the platform on `http://localhost:8080`
5. Installs canvas deps (first run) and starts the canvas on `http://localhost:3000`
6. Prints next-step instructions and tails both processes — `Ctrl-C` tears everything down

Total wall-clock: ~30 seconds for a re-run, ~2 minutes for a first run (npm install + docker pulls).

Once the canvas is up: open it, add your model API key in **Config → Secrets & API Keys → Global**, then click a template card or **+ Create blank workspace**.

## Manual setup (advanced)

If you'd rather run each component yourself — useful when you're iterating on the platform binary or the canvas in isolation — follow the steps below. Each section is what `dev-start.sh` does internally; running them by hand gives you per-component logs and lets you keep one piece running while you restart another.

### Step 1: Clone the repository

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-monorepo.git
cd molecule-monorepo
```

### Step 2: Start the shared infrastructure

Recommended:

```bash
./infra/scripts/setup.sh
```

That brings up Postgres, Redis, Langfuse, ClickHouse, and Temporal.

If you only want the raw compose flow:

```bash
docker compose -f docker-compose.infra.yml up -d
```

### Step 3: Start the platform

```bash
cd workspace-server
go run ./cmd/server
```

The control plane listens on `http://localhost:8080`.

### Step 4: Start the canvas

In a new terminal:

```bash
cd canvas
npm install
npm run dev
```

Open `http://localhost:3000`.

## Step 5: Deploy your first workspace

On a fresh canvas, the center empty state shows template cards plus a blank-workspace option.

You can either:

1. Click a template to provision a ready-made workspace.
2. Click `+ Create blank workspace`.

At the same time, the bottom-left onboarding wizard appears and guides the first-run flow.

## Step 6: Add an API key

1. Select the workspace.
2. Open the `Config` tab.
3. Expand `Secrets & API Keys`.
4. Add the API key in either:
   - `This Workspace`
   - `Global (All Workspaces)`

Global keys are inherited by all workspaces. Workspace keys override globals with the same name.

## Step 7: Send the first message

1. Open the `Chat` tab.
2. Send a prompt such as:

```text
What can you help me with in this workspace?
```

Responses are delivered through the platform A2A proxy and pushed back to the canvas through WebSocket events, with polling kept only as recovery fallback.


---

## Path 2: Remote Agent (run anywhere)

A remote agent runs on your own machine or a cloud VM — no Docker on the platform side. The agent registers with the platform via API, pulls its secrets at boot, and sends heartbeats to stay live on the canvas.

**Use this path if you:**
- want to run an agent on your laptop for local development
- need an agent on a machine with specific hardware (GPU, on-prem)
- have a data-residency requirement that keeps agent compute off the platform's infra

### Step 0: Prerequisites

- Python 3.10+ and `pip install molecule-agent-sdk`
- Outbound HTTPS access from the agent machine to `https://<your-org>.moleculesai.app`
- A platform admin token (from the canvas, under `Config → Secrets & API Keys → Global`)

### Step 1: Create the workspace

```bash
PLATFORM="https://acme.moleculesai.app"
ADMIN_TOKEN="your-admin-token"

curl -X POST "$PLATFORM/workspaces" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-remote-agent",
    "runtime": "external",
    "external": true,
    "url": "https://my-agent.example.com/a2a",
    "parent_id": null
  }'
```

Save the returned `workspace_id`.

### Step 2: Register the agent

```bash
WORKSPACE_ID="ws-xyz"

curl -X POST "$PLATFORM/registry/register" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"workspace_id\": \"$WORKSPACE_ID\",
    \"name\": \"my-remote-agent\",
    \"description\": \"Runs on a cloud VM in us-east-1\",
    \"skills\": [\"research\"],
    \"url\": \"https://my-agent.example.com/a2a\"
  }"
```

The response includes your bearer token — save it now. It is shown only once.

### Step 3: Pull secrets at boot

```bash
AGENT_TOKEN="the-token-from-step-2"

curl "$PLATFORM/workspaces/$WORKSPACE_ID/secrets" \
  -H "Authorization: Bearer $AGENT_TOKEN"
```

Store the returned secrets in your environment before starting the agent.

### Step 4: Run the agent

```bash
molecule-agent run \
  --workspace-id "$WORKSPACE_ID" \
  --platform-url "$PLATFORM" \
  --agent-token "$AGENT_TOKEN"
```

The agent connects to the platform, appears on the canvas within ~10 seconds, and starts processing tasks.

### Step 5: Configure the agent

Edit `config.yaml` in the agent's working directory:

```yaml
name: my-remote-agent
role: researcher
runtime: python
platform_url: https://acme.moleculesai.app
a2a:
  port: 8000
```

### Step 6: Inspect and iterate

The agent appears on the canvas as a workspace card with a **REMOTE** badge. Open the chat tab, send a task, and watch it work. To iterate, stop and restart the agent — it re-registers with the same `workspace_id` and token.

### Behind NAT (no public IP)

If the agent machine has no public IP, use a tunnel:

```bash
# Terminal 1: start a tunnel
ngrok http 8000 --url https://my-agent.ngrok.io

# Update the registered URL
curl -X POST "$PLATFORM/registry/update-card" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"workspace_id": "'"$WORKSPACE_ID"'", "url": "https://my-agent.ngrok.io/a2a"}'
```

No inbound firewall rules needed — the agent initiates the outbound WebSocket connection.

### Next steps

- [Register a Remote Agent](../tutorials/register-remote-agent.md) — full tutorial with CI/CD examples
- [External Agent Registration Guide](../guides/external-agent-registration.md) — detailed reference
- [Remote Workspaces FAQ](../guides/remote-workspaces-faq.md) — common questions

## What To Try Next

- **Expand to a team:** right-click a workspace and choose `Expand to Team`.
- **Switch runtime:** use `Config -> Runtime` to move between LangGraph, DeepAgents, Claude Code, CrewAI, AutoGen, and OpenClaw.
- **Inspect operations:** check `Activity`, `Traces`, `Events`, and `Terminal`.
- **Use global keys:** configure one provider once in `Secrets & API Keys -> Global`.
- **Import a template:** use the template palette or `POST /templates/import`.
- **Import/export bundles:** duplicate or move workspace trees as `.bundle.json`.

## Troubleshooting

| Problem | What to check |
|---|---|
| Workspace stays offline | Platform is running, Docker is available, and the runtime has valid credentials |
| Template palette is empty | `workspace-configs-templates/` exists and the platform can read it |
| Chat says agent unavailable | Workspace status is not yet `online` or `degraded` |
| No responses from model | Add the correct API key in `Secrets & API Keys` and restart the workspace |
| Want direct DB access | Postgres and Redis are internal-only; use `docker compose exec postgres psql` or `docker compose exec redis redis-cli` |

## Architecture At A Glance

```text
Browser  -->  Canvas (Next.js :3000)
                |
                v
           Platform (Go :8080)
             |       |       |
             |       |       +--> WebSocket events / A2A proxy / templates / bundles
             |       +----------> Redis
             +------------------> Postgres
                                |
                                v
                       Provisioned workspaces
                     (LangGraph / Claude Code / CrewAI / AutoGen / etc.)
```

For the full system model, see [Architecture](./architecture/architecture.md).
