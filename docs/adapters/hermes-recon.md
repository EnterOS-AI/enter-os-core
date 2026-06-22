# Hermes Agent — Adapter Reconnaissance

Reconnaissance of [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent) (v0.8.0, 68,713 ⭐, MIT) for potential EnterOS adapter integration.

> **Status:** Design-only recon — no implementation.

---

## a) CLI Invocation

**Install** (curl-to-bash, targets Linux/macOS/WSL2/Termux):

```bash
curl -fsSL https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh | bash
```

The `hermes` binary in the repo root is a Python script (`#!/usr/bin/env python3`) that imports and calls `hermes_cli.main.main()`. After install it lands on `$PATH`.

**Minimal interactive session:**

```bash
hermes                      # launches TUI, auto-detects provider from env
hermes chat                 # explicit; same as bare `hermes`
hermes setup                # one-time wizard: sets model, provider, API keys
```

**Key runtime flags:**

```bash
hermes chat \
  --model anthropic/claude-opus-4.6 \
  --provider openrouter \
  --toolsets terminal,file,web \
  --max-turns 60 \
  --query "build me a FastAPI app" \
  --resume                  # continue most recent session
  --worktree                # git-worktree isolation per session
  --profile myprofile       # load alternate HERMES_HOME profile
```

**One-shot (non-interactive):**

```bash
hermes chat --query "summarise this repo" --quiet
```

**Gateway (messaging platforms) start:**

```bash
hermes gateway start        # daemonises; reads gateway config from config.yaml
hermes gateway status
hermes gateway stop
```

**OpenClaw migration:**

```bash
hermes claw migrate --dry-run   # preview; drop --dry-run to execute
```

---

## b) Config Format

**Format:** YAML  
**Primary path:** `~/.hermes/config.yaml` (default), overrideable via `HERMES_HOME` env var.  
**Reference file in repo:** `cli-config.yaml.example`

**Minimal working config** (provider = OpenRouter, Docker terminal backend):

```yaml
# ~/.hermes/config.yaml

model:
  default: "anthropic/claude-opus-4.6"
  provider: "openrouter"          # required; "auto" if you want env-var detection
  base_url: "https://openrouter.ai/api/v1"

terminal:
  backend: "local"                # required; options: local | ssh | docker | singularity | modal | daytona
  cwd: "."
  timeout: 180
  lifetime_seconds: 300

memory:
  memory_enabled: true
  user_profile_enabled: true
  memory_char_limit: 2200
  user_char_limit: 1375
  nudge_interval: 10

agent:
  max_turns: 60
  reasoning_effort: "medium"      # xhigh | high | medium | low | minimal | none
```

**Required fields:** `model.default`, `model.provider`, `terminal.backend`.  
Everything else has a hardcoded default.

**Credentials** go in `~/.hermes/.env` (separate from config.yaml):

```bash
OPENROUTER_API_KEY=sk-or-...
ANTHROPIC_API_KEY=sk-ant-...
HERMES_HOME=~/.hermes           # optional override
```

**Skills config** (in `config.yaml`):

```yaml
skills:
  creation_nudge_interval: 15   # remind agent to persist a skill every N tool iterations
  external_dirs:
    - ~/.agents/shared-skills   # read-only external skill dirs
```

**Compression config** (in `config.yaml`):

```yaml
compression:
  enabled: true
  threshold: 0.50
  summary_model: "google/gemini-3-flash-preview"
```

---

## c) Runtime Dependencies

**Python version:** 3.13 (Dockerfile base: `ghcr.io/astral-sh/uv:0.11.6-python3.13-trixie`)  
**Package manager:** [uv](https://github.com/astral-sh/uv) (not pip directly; `uv pip install .`)  
**Package version:** `hermes-agent==0.8.0`

**Top core pip dependencies** (from `pyproject.toml`):

| Package | Version constraint | Purpose |
|---|---|---|
| `openai` | `>=2.21.0,<3` | Primary LLM client (all providers via OpenAI-compat API) |
| `anthropic` | `>=0.39.0,<1` | Direct Anthropic API adapter |
| `python-dotenv` | `>=1.2.1,<2` | `.env` loading |
| `fire` | `>=0.7.1,<1` | CLI argument dispatch |
| `httpx[socks]` | `>=0.28.1,<1` | Async HTTP (gateway, webhooks) |
| `rich` | `>=14.3.3,<15` | TUI rendering |
| `pyyaml` | `>=6.0.2,<7` | Config file parsing |
| `pydantic` | `>=2.12.5,<3` | Data validation |
| `prompt_toolkit` | `>=3.0.52,<4` | Interactive TUI / multiline input |
| `tenacity` | `>=9.1.4,<10` | Retry logic |

**Key optional extras:**

```bash
pip install "hermes-agent[modal]"     # modal>=1.0.0 — serverless backend
pip install "hermes-agent[daytona]"   # daytona>=0.148.0 — cloud sandbox backend
pip install "hermes-agent[mcp]"       # mcp>=1.2.0 — MCP server/client
pip install "hermes-agent[honcho]"    # honcho-ai — cross-session user modeling
pip install "hermes-agent[messaging]" # telegram, discord.py, aiohttp, slack
pip install "hermes-agent[voice]"     # faster-whisper, sounddevice, numpy
pip install "hermes-agent[rl]"        # atroposlib, fastapi, uvicorn, wandb
```

**System binaries** (from Dockerfile `apt-get install`):

```
nodejs  npm  ripgrep  ffmpeg  gcc  python3-dev  libffi-dev  procps  build-essential
```

`ripgrep` is used by the `file` toolset for fast codebase search. `ffmpeg` is used for voice transcription pre-processing.

---

## d) Session State

**All persistent state lives under `HERMES_HOME`** (default: `~/.hermes/`, overrideable via env var).

**Primary state store: SQLite**

```
~/.hermes/state.db          ← DEFAULT_DB_PATH = get_hermes_home() / "state.db"
```

- Schema version: **6** (`SCHEMA_VERSION = 6` in `hermes_state.py`)
- WAL mode (`PRAGMA journal_mode=WAL`) — supports concurrent gateway + CLI writers
- Three core tables: `schema_version`, `sessions`, `messages`
- **FTS5 virtual table** `messages_fts` with auto-sync triggers on INSERT/UPDATE/DELETE — backs the `session_search` toolset (full-text search across all past conversation content)
- Compression-triggered session splitting tracked via `parent_session_id` chain in `sessions` table
- Session source tagged as `'cli'`, `'telegram'`, `'discord'`, etc. for per-platform filtering

**Full directory layout:**

```
~/.hermes/
├── config.yaml          ← get_config_path()
├── .env                 ← get_env_path()
├── state.db             ← SQLite WAL, FTS5
├── skills/              ← get_skills_dir() — user-created skill SKILL.md files
├── logs/                ← get_logs_dir() — trajectory JSONs
│   └── session_YYYYMMDD_HHMMSS_<uuid>.json
├── MEMORY.md            ← agent's curated notes (injected into system prompt)
├── USER.md              ← user profile (injected into system prompt)
└── skins/               ← optional custom theme YAMLs
```

**State is persistent by default.** Session history, memories (`MEMORY.md`/`USER.md`), and skills survive restarts. The `session_reset` config controls when gateway sessions are cleared (default: `mode: both`, idle after 1440 min or at 4 AM daily). Before any reset, Hermes is given one flush turn to write important context to `MEMORY.md`.

Container backend state is controlled separately by `container_persistent: true/false` in the `terminal:` block.

---

## e) Execution Backends

**Six backends configured via a single `terminal.backend` key in `config.yaml`:**

| Backend | Where commands run | Key extra config |
|---|---|---|
| `local` | Host machine, current dir | — |
| `ssh` | Remote server | `ssh_host`, `ssh_user`, `ssh_key` |
| `docker` | Inside a Docker container | `docker_image`, `docker_mount_cwd_to_workspace` |
| `singularity` | Singularity/Apptainer container (HPC) | `singularity_image` |
| `modal` | Modal cloud sandbox (serverless) | `modal_image`, `pip install hermes-agent[modal]` |
| `daytona` | Daytona cloud sandbox | `daytona_image`, `container_disk`, `pip install hermes-agent[daytona]` |

**Architecture clarification:** Hermes's Python process **always runs locally** (or wherever you launched it). The `backend` setting controls only where the **`terminal` tool** executes shell commands. For `docker`, Hermes calls the Docker API to spawn/reuse a container and routes `terminal` tool calls into it via exec — Hermes itself is **not** containerised by this setting.

**Docker backend minimal config:**

```yaml
terminal:
  backend: "docker"
  cwd: "/workspace"                              # path inside the container
  timeout: 180
  lifetime_seconds: 300
  docker_image: "nikolaik/python-nodejs:python3.11-nodejs20"
  docker_mount_cwd_to_workspace: false           # default: false (security off). Set true to bind-mount launch dir into /workspace
  docker_forward_env:
    - "GITHUB_TOKEN"
    - "NPM_TOKEN"
  container_cpu: 1
  container_memory: 5120                         # MB
  container_disk: 51200                          # MB
  container_persistent: true                     # false = ephemeral container, wiped after session
```

**The Dockerfile** (for running *all of Hermes* inside Docker, distinct from the backend setting) uses:

```dockerfile
FROM debian:13.4
ENV HERMES_HOME=/opt/data
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/hermes/.playwright
VOLUME /opt/data
ENTRYPOINT ["/opt/hermes/docker/entrypoint.sh"]
# Runs as non-root user hermes (UID 10000), home /opt/data
```

**Serverless hibernation** (Modal + Daytona): `container_persistent: false` produces fully ephemeral sandboxes that are destroyed after `lifetime_seconds`; `true` persists the container filesystem between sessions (warm-resume, no re-install overhead).

---

## f) Value Proposition

Integrating Hermes adds one capability that none of the six existing adapters (LangGraph, Claude Code, CrewAI, AutoGen, OpenClaw, DeepAgents) deliver end-to-end: **a closed learning loop that compounds across sessions at the skill, memory, and user-model layers simultaneously.** Concretely: after a complex task, Hermes autonomously creates a `SKILL.md` file in `~/.hermes/skills/` (prompted every `creation_nudge_interval=15` tool iterations), and those skills are re-injected as context in future sessions — agents get better at tasks they've done before without any human curation step. The `session_search` toolset adds FTS5 + Gemini Flash summarization over `state.db`, so the agent can recall specific conversations from months ago with semantic-quality results. Layered on top is **Honcho dialectic user modeling** (`plastic-labs/honcho`) — a cross-session profile that tracks user communication style, preferences, and expectations, shared across any Honcho-integrated tool (not just Hermes). Finally, the **Modal and Daytona serverless backends with `container_persistent`** give EnterOS a path to hibernating, pay-per-use sandboxes that no existing adapter exposes — directly relevant to EnterOS's multi-workspace billing model. The `hermes claw migrate` command (backed by `optional-skills/migration/openclaw-migration/scripts/openclaw_to_hermes.py`) is also relevant: EnterOS could offer equivalent migration tooling to attract OpenClaw's existing ~247k-user base, and the **`agentskills.io` skill-manifest spec** (referenced in `optional-skills/`) should be reviewed before EnterOS finalises its own plugin manifest schema to ensure interoperability with what is rapidly becoming the de-facto file-based skill standard.
