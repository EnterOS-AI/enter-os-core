# Running a Google ADK Workspace on EnterOS

> **Status (2026-05-29):** the `google-adk` runtime is **landing**, not yet on
> `main`. It's implemented in the template repo
> [`molecule-ai-workspace-template-google-adk`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-google-adk)
> (PR **#1**) with platform registration in enter-os-core PR **#2003** and the
> validator allowlist in molecule-ci PR **#26**. Design + approval: RFC
> [`internal#730`](https://git.moleculesai.app/molecule-ai/internal/issues/730).
> Remove this banner once those PRs merge.
>
> **Doc-accuracy note:** a prior version of this page claimed ADK was "already
> first-class" and cited "PR #550" — that PR is unrelated (a MemoryTab test
> suite). No `google-adk` adapter existed at that time. This rewrite reflects
> the real implementation.

Google's Agent Development Kit (ADK) runs as a EnterOS workspace runtime:
ADK is the **agent engine** (`LlmAgent` + `Runner`), and the workspace
participates in EnterOS's A2A org like any other runtime.

## How it actually works

- **ADK = engine only.** The adapter builds an ADK `LlmAgent` from the
  workspace config (model + system prompt + tools) and drives its `Runner`.
  It installs `google-adk[mcp]==2.1.0` and **never** the `[a2a]` extra — ADK's
  a2a layer pins `a2a-sdk<0.4`, which is incompatible with the platform's
  `a2a-sdk>=1.0`. (Verified: `google-adk[mcp]==2.1.0` + `a2a-sdk 1.0.3` coexist.)
- **A2A** is provided by the platform's a2a-1.x server; a EnterOS-authored
  executor bridges ADK's `Runner` event stream onto it, one ADK session per
  A2A `context_id`.
- **Tools** reach the agent via ADK's native `McpToolset` pointed at the
  workspace's `a2a_mcp_server` — the same MCP surface the CLI runtimes use
  (`delegate_task`, `commit_memory`, `list_peers`, …). No LangChain.

## Auth — Vertex AI via ADC (keyless), or an AI Studio key

The runtime supports both google-genai auth paths:

- **Vertex AI + Application Default Credentials (recommended; required if your
  org disallows API keys).** Set `model: vertex:gemini-2.5-pro` and provide
  `GOOGLE_CLOUD_PROJECT`; the adapter sets `GOOGLE_GENAI_USE_VERTEXAI=1` and
  google-genai authenticates via ADC — no API key. (Locally:
  `gcloud auth application-default login`.)
- **AI Studio API key** (where your org permits API keys): set
  `model: google_genai:gemini-2.5-pro` and `GOOGLE_API_KEY`.

## Create a workspace

```bash
# Vertex AI + ADC (keyless)
curl -s -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -d '{
    "name": "adk-agent",
    "role": "Google ADK inference worker",
    "runtime": "google-adk",
    "model": "vertex:gemini-2.5-pro",
    "runtime_config": {"required_env": ["GOOGLE_CLOUD_PROJECT"]}
  }'
```

Send it a task via the A2A proxy (`POST /workspaces/:id/a2a`, JSON-RPC
`message/send`) and it replies through the ADK `Runner`. Verified end-to-end:
a Gemini 2.5 round-trip on Vertex via ADC returns through the built image.

## Related
- Template + adapter: [`molecule-ai-workspace-template-google-adk`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-google-adk) (PR #1)
- Platform registration: enter-os-core PR #2003 · validator: molecule-ci PR #26
- Design/approval: RFC [`internal#730`](https://git.moleculesai.app/molecule-ai/internal/issues/730)
- [Google ADK (adk-python)](https://github.com/google/adk-python)
