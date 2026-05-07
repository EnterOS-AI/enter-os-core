# Runtime native-MCP push parity — status

**Goal:** every workspace runtime delivers Molecule A2A inbox messages
with the same UX as claude-code's MCP `notifications/claude/channel`
push: session continuity + queued or interrupted handling of new
messages mid-thread, no fresh subprocess per message.

Tracked across four runtime streams. Updated 2026-05-02.

---

## claude-code

**Status:** ✅ Done. Native MCP `notifications/claude/channel` push
shipped via `workspace/a2a_mcp_server.py`. Requires the host to launch
with `--dangerously-load-development-channels server:molecule`.

No further work.

---

## OpenClaw

**Status:** Scaffolded; awaiting validation + companion adapter rewrite.

**Path:** Channel-plugin SDK (`openclaw/plugin-sdk`), auto-discovered
from `~/.openclaw/plugins/<name>/` or workspace `.openclaw/`. Plugin
registers an HTTP webhook on `openclaw gateway`; Molecule workspace
adapter POSTs A2A messages to it; gateway dispatches through the same
`dispatchReplyWithBufferedBlockDispatcher` kernel call native channels
(Telegram, Lark, Slack, Discord) use.

**Artifacts landed:**
- `molecule-ai-workspace-template-openclaw/packages/openclaw-channel-plugin/`
  - `package.json`, `openclaw.plugin.json` (manifest), `index.ts`
    (channel + webhook handler), `README.md`, `tsconfig.json`
- Pre-release `v0.1.0-pre`. Mirrors `rabbit-lark-bot` reference
  plugin shape.

**Remaining (task #84, #87):**
1. Validate against a running OpenClaw gateway. Open questions in the
   plugin README: `resolveAgentRoute` peer-id shape,
   `dispatchReplyWithBufferedBlockDispatcher` async semantics,
   `outbound.sendText` no-op safety.
2. Rewrite Python adapter (`adapter.py`) to stop shelling out
   `openclaw agent --message ...` and instead POST to the plugin's
   webhook + run `/agent-reply` callback HTTP server. **Post-demo
   work** (touches a working integration).

---

## hermes

**Status:** Workspace template patch PR #32 MERGED 2026-05-02; image
rebuild succeeded; plugin baked into the workspace runtime. Plugin
package published. Real-subprocess full-chain E2E (`scripts/e2e_full_chain.py`)
green — proves wire shape end-to-end against a real `hermes gateway run`
subprocess + stub OpenAI-compat LLM. Caught + fixed a real `KeyError`
in upstream `hermes_cli/tools_config.py` (PLATFORMS dict lookup
crashed on plugin platforms) — fix on the patched fork branch
(`molecule-ai/hermes-agent` `feat/platform-adapter-plugins`, commit
`18e4849e`, hosted on Gitea at
`https://git.moleculesai.app/molecule-ai/hermes-agent` — moved from the
suspended `github.com/HongmingWang-Rabbit/hermes-agent`, see
`molecule-ai/internal#72`). Upstream PR #18775 OPEN; CONFLICTING with main.
Not on critical path for our platform — patched fork is what the
workspace image installs.

Real A2A peer traffic on staging gated only on running the harness
(`molecule-core/scripts/test-all-runtimes-a2a-e2e.sh`) — script ready,
needs provider keys.

**Path:** Hermes's MODERN plugin system is `hermes_cli/plugins.py`
(not the older `plugins/memory/`). It already does full discovery
across user dir + project dir + pip entry_points (group:
`hermes_agent.plugins`) for tools / hooks / CLI commands / slash
commands / context engines / skills. **Platform adapters are the only
plugin type still hardcoded** (`gateway/run.py:_create_adapter`).

The PR adds three pieces upstream:
1. `PluginContext.register_platform_adapter(name, adapter_class, requirements_check=None)`
2. `GatewayConfig.plugin_platforms` populated by `from_dict` for
   plugin-claimed names
3. `GatewayRunner._create_plugin_adapter(name, config)` boot-path
   fallback

Plus a `PluginPlatformIdentifier` helper class so plugin adapters can
satisfy `BasePlatformAdapter.__init__(config, platform: Platform)`
without extending the closed Platform enum.

Total: ~100 LOC upstream change. External plugin then ships as
`hermes-platform-molecule-a2a` via `pip install` + entry_points — no
fork needed in production.

**Artifacts landed:**
- **Upstream PR**: [NousResearch/hermes-agent#18775](https://github.com/NousResearch/hermes-agent/pull/18775)
  — 5 commits on `feat/platform-adapter-plugins`: registration
  surface, config + boot wiring, `PluginPlatformIdentifier` helper,
  `resolve_platform_id` for plugin-platform-safe deserialization, and
  `self.adapters[adapter.platform]` keying fix (caught by real-subprocess
  test before merge — see below).
- **Plugin package**: [Molecule-AI/hermes-platform-molecule-a2a](https://github.com/Molecule-AI/hermes-platform-molecule-a2a)
  v0.1.0 — public, MIT-licensed. 11 unit tests + 8 in-process E2E
  + 4 real-subprocess E2E checkpoints all green.
- **Workspace template patch**: [Molecule-AI/molecule-ai-workspace-template-hermes#32](https://github.com/Molecule-AI/molecule-ai-workspace-template-hermes/pull/32)
  — Dockerfile installs the patched fork + plugin into the hermes
  installer's venv; start.sh seeds `platforms.molecule-a2a` config
  stanza. Pre-demo deliberately install-only; adapter.py rewrite to
  USE the plugin path is a separate post-demo PR.
- Real adapter package at `~/hermes-platform-molecule-a2a/`:
  - `pyproject.toml` with `hermes_agent.plugins` entry point
  - `hermes_platform_molecule_a2a/adapter.py` —
    `MoleculeA2APlatformAdapter(BasePlatformAdapter)` with HTTP
    listener (aiohttp), inbound `MessageEvent(internal=True)` dispatch,
    outbound `send()` POST to per-chat callback URL, optional shared
    secret enforcement
  - `tests/test_adapter.py` — **11/11 unit tests pass** covering plugin
    entry-point shape, lifecycle, inbound auth, outbound routing
  - `scripts/e2e_validate.py` — production-path validation (entry
    points → registry → GatewayConfig → boot → HTTP roundtrip), all
    7 checkpoints pass
- `docs/integrations/hermes-platform-plugins-upstream-pr.md` — PR
  draft including problem, prior art, proposal, code shape, backward
  compat, test plan, and open questions.
- `.hermes-validation/test_register_platform_adapter.py` — local
  9-check validation of the patched fork via the user-dir discovery
  path (complementary to the entry-points path tested by the package).

**Why no short-term polling shim:** earlier framing was wrong. Molecule
runtime already polls the inbox via `wait_for_message` per turn; each
polled message fires a fresh `execute()` on the adapter, which
proxies to hermes's stateless `/v1/chat/completions`. Adding adapter-
side polling would be duplicate work. The genuine short-term gap is
**session continuity** (hermes daemon doesn't see a single
conversation across turns because chat/completions is stateless), not
push latency. That gap is solved by the upstream PR; no
intermediate shim earns its complexity.

**Remaining:**
1. **Upstream PR review/merge** (NousResearch/hermes-agent#18775). On
   maintainers — typical OSS review lag.
2. **Workspace template merge + image republish** (PR #32). Once
   merged, `publish-runtime.yml` regenerates the hermes workspace image
   with the plugin baked in. Safe to merge as-is — install-only, no
   behavior change for current workspaces.
3. **Runtime adapter rewrite** (task #87 equivalent for hermes).
   `molecule-ai-workspace-template-hermes/adapter.py` currently proxies
   A2A → `/v1/chat/completions`. Switching to POST `/a2a/inbound` is
   what unlocks single-session continuity. **Post-demo timing**
   (touches a working live integration).
4. **Real A2A peer traffic E2E** (task #86): boot a real workspace
   from the republished image, send peer A2A message from another
   workspace, observe single-session reply. Gated on items 2 + 3.

---

## Codex (OpenAI Codex CLI)

**Status:** Template SHIPPED. Repo live at
[`Molecule-AI/molecule-ai-workspace-template-codex`](https://github.com/Molecule-AI/molecule-ai-workspace-template-codex)
(14 files, 1411 LOC, 12/12 tests). molecule-core registration in
[PR #2512](https://github.com/Molecule-AI/molecule-core/pull/2512).
E2E with real A2A traffic remains.

**Path:** Persistent `codex app-server` stdio JSON-RPC client
(NDJSON-framed, v2 protocol). One app-server child per workspace
session; one `thread/start` per session; each A2A message becomes a
`turn/start` RPC; agent responses arrive as
`agent_message_delta` notifications. Per-thread serialization for
mid-turn arrivals (matches OpenClaw's per-chat sequentializer).
Optional `turn/interrupt` for "latest message wins" workspaces.

**Artifacts landed:**
- `docs/integrations/codex-app-server-adapter-design.md` — full design
  including RPC sequence, executor skeleton, eight open questions.
- `molecule-ai-workspace-template-codex/` — full template repo
  scaffolded:
  - `app_server.py` (286 LOC) — async JSON-RPC over NDJSON stdio
  - `executor.py` (~270 LOC) — thread bootstrap, turn dispatch,
    notification accumulation, mid-turn serialization
  - `adapter.py` — thin `BaseAdapter` shell + preflight
  - `Dockerfile`, `start.sh`, `config.yaml`, `requirements.txt`,
    `README.md`
  - `tests/` — **12/12 tests pass** (7 vs NDJSON mock child, 5 vs
    fake AppServerProcess covering executor logic)

**Validated against live `codex-cli 0.72.0`:** NDJSON framing,
`initialize` handshake, AND `thread/start` all work end-to-end.
**Schema-runtime drift caught:** real binary returns `thread.id`,
not `thread.threadId` as the JSON schema claims. Executor now
accepts both shapes; without the smoke test this would have been
a production bug.

**Remaining (task #85, #86):**
1. Register `codex` in molecule-core's `manifest.json` +
   `workspace-server/internal/handlers/runtime_registry.go`.
   **Defer to post-demo** — touches working live registry.
2. E2E verification with a real Molecule workspace + peer A2A
   traffic, per `feedback_close_on_user_visible_not_merge`.

---

## Cross-cutting (task #86)

End-to-end verification per `feedback_close_on_user_visible_not_merge`.
For each runtime, the closure criterion is not "code merged" but
"observed: real workspace boots → A2A message from peer agent →
delivered to running session → reply returned through A2A response
queue → peer agent receives". No runtime stream closes until that
chain is observed.

---

## What's blocking what

| Stream | Blocked on |
|---|---|
| claude-code | (done) |
| OpenClaw plugin | live gateway validation, then post-demo adapter rewrite |
| OpenClaw adapter rewrite | post-demo timing |
| hermes upstream PR | user confirmation to submit + Discord pre-validation |
| hermes consumer plugin | upstream PR merging |
| codex implementation | resolve 8 open questions, then post-demo eng time |
| E2E verification | each runtime stream completing |

Three of four runtime streams are at decision points needing user
input. Pre-demo (T-4d to 2026-05-06), the safe move is to land the
remaining design + scaffolding work and defer all behavioral changes to
post-demo.
