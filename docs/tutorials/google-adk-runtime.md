# Running a Google ADK Workspace on Molecule AI

Google's Agent Development Kit (ADK) is now a first-class runtime on Molecule AI. This tutorial walks you from zero to a running ADK agent workspace — one that persists per-conversation session state and sits alongside your Claude Code and Gemini CLI workers in the same A2A network.

## What you'll need

- A Molecule AI account with at least one provisioned tenant
- A `GOOGLE_API_KEY` from [aistudio.google.com](https://aistudio.google.com) (or Vertex AI credentials — see below)
- `curl` + `jq`

## Setup

```bash
# 1. Store your Google API key as a global secret
curl -s -X PUT http://localhost:8080/settings/secrets \
  -H "Content-Type: application/json" \
  -d '{"key":"GOOGLE_API_KEY","value":"YOUR-AI-STUDIO-KEY"}' | jq .

# 2. Create a google-adk workspace
WS=$(curl -s -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -d '{
    "name": "adk-agent",
    "role": "Google ADK inference worker",
    "runtime": "google-adk",
    "model": "google:gemini-2.0-flash"
  }' | jq -r '.id')
echo "Workspace: $WS"

# 3. Wait for ready (~30s)
until curl -s http://localhost:8080/workspaces/$WS | jq -r '.status' | grep -q ready; do
  echo "Waiting..."; sleep 5
done

# 4. Send your first task
curl -s -X POST http://localhost:8080/workspaces/$WS/a2a \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"1","method":"message/send",
       "params":{"message":{"role":"user","parts":[{"kind":"text",
       "text":"Summarise the ADK architecture in 3 bullet points."}]}}}' \
  | jq '.result.parts[0].text'

# 5. Multi-turn — session state is preserved across calls
curl -s -X POST http://localhost:8080/workspaces/$WS/a2a \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"2","method":"message/send",
       "params":{"message":{"role":"user","parts":[{"kind":"text",
       "text":"Now give me a one-line TL;DR of what you just said."}]}}}' \
  | jq '.result.parts[0].text'

# 6. Vertex AI alternative — set these instead of GOOGLE_API_KEY
# curl -X PUT .../secrets -d '{"key":"GOOGLE_GENAI_USE_VERTEXAI","value":"1"}'
# curl -X PUT .../secrets -d '{"key":"GOOGLE_CLOUD_PROJECT","value":"my-project"}'
# curl -X PUT .../secrets -d '{"key":"GOOGLE_CLOUD_LOCATION","value":"us-central1"}'
```

## Expected output

After step 4, ADK streams the Gemini response through its event bus, filters for `is_final_response()` events, and returns the agent's reply as a standard A2A text part. Step 5 should reference the prior answer — the adapter ties each A2A `context_id` to an `InMemorySessionService` session, so conversation state is isolated per task context and survives across calls within the same session.

## How it works

The `google-adk` adapter wraps Google ADK's runner/session model behind the same `AgentExecutor` interface used by every other Molecule AI runtime. On each turn, `GoogleADKA2AExecutor` calls `runner.run_async()` with the incoming message wrapped in a `google.genai.types.Content` object, then drains the event stream until it collects a final-response event. The `google:` model prefix is stripped before being passed to ADK — so `google:gemini-2.0-flash` in your workspace config becomes `gemini-2.0-flash` in the ADK `LlmAgent`. Error class names are sanitized before leaving the executor; raw Google SDK stack traces never reach the A2A caller.

## Mixed-runtime teams

ADK workspaces participate in the same A2A network as Claude Code, Gemini CLI, Hermes, and LangGraph workers. An orchestrator can delegate long-context summarisation to a `google-adk` worker (Gemini 1.5 Pro's 1M token window) while routing tool-use tasks to a `claude-code` worker — with no provider-specific code in the orchestrator itself. Add an ADK peer with `POST /workspaces`, set `GOOGLE_API_KEY`, and it's available for `delegate_task` immediately.

## Related

- PR #550: [feat(adapters): add google-adk runtime adapter](https://git.moleculesai.app/molecule-ai/molecule-core/pull/550)
- [Google ADK (adk-python)](https://github.com/google/adk-python)
- [Gemini CLI runtime tutorial](./gemini-cli-runtime.md)
- [Platform API reference](../api-reference.md)
