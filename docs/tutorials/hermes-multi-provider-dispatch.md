# Hermes Multi-Provider Dispatch: Native Anthropic, Gemini, and Multi-Turn History

Hermes is Molecule AI's inference router. Out of the box it proxies every model through an OpenAI-compatible shim. That works for plain text, but the shim does format translation on every round-trip — and it gets the Gemini message format wrong (Gemini expects `role: "model"` and a `parts: [{text}]` wrapper; the shim passes `role: "assistant"` and a flat string). It also flattens multi-turn conversations into a single user blob, losing role attribution across turns.

Phases 2a–2c wire three native dispatch paths keyed on `auth_scheme`. This tutorial shows you how to unlock them.

> **Phase 2d scope note:** Tool calling, vision content blocks, system instructions, and streaming on the native paths are scoped for Phase 2d and are **not yet shipped**. This tutorial covers what is merged today: correct native dispatch + multi-turn history continuity.

## What you'll need

- A Molecule AI account with API access
- `ANTHROPIC_API_KEY` **or** `GEMINI_API_KEY` (or both)
- `curl` + `jq`

## The dispatch table

After Phases 2a / 2b / 2c, Hermes picks an inference path based on which provider is configured:

| `auth_scheme` | Dispatch path | Provider | API |
|---|---|---|---|
| `openai` | `_do_openai_compat` | 13 providers (OpenRouter, Groq, Mistral…) | OpenAI-compat shim |
| `anthropic` | `_do_anthropic_native` | Anthropic | Native Messages API |
| `gemini` | `_do_gemini_native` | Google | Native `generateContent` |
| unknown | `_do_openai_compat` + warning | any | OpenAI-compat shim (forward-compat) |

**Rule of thumb:** set `ANTHROPIC_API_KEY` to get native Anthropic dispatch. Set `GEMINI_API_KEY` to get native Gemini dispatch. Set `NOUS_API_KEY` / `HERMES_API_KEY` / `OPENROUTER_API_KEY` to stay on the compat shim. Molecule AI reads these in priority order: `HERMES_API_KEY` → `OPENROUTER_API_KEY` → `ANTHROPIC_API_KEY` → `GEMINI_API_KEY`. The **first key found wins**, so don't set `HERMES_API_KEY` if you want native dispatch.

---

## Setup

```bash
# 0. Export your platform URL and a workspace to use as orchestrator
export MOLECULE_API=http://localhost:8080
export ORCH_ID=<your-orchestrator-workspace-id>

# 1. Store your Anthropic key as a global secret
curl -s -X PUT $MOLECULE_API/settings/secrets \
  -H "Content-Type: application/json" \
  -d '{"key":"ANTHROPIC_API_KEY","value":"sk-ant-YOUR-KEY"}' | jq .

# 2. Create a Hermes workspace — Anthropic native dispatch
ANTHROPIC_WS=$(curl -s -X POST $MOLECULE_API/workspaces \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hermes-anthropic",
    "role": "Inference worker — native Anthropic path",
    "runtime": "hermes",
    "model": "anthropic:claude-sonnet-4-5"
  }' | jq -r '.id')
echo "Anthropic workspace: $ANTHROPIC_WS"

# 3. Wait for it to be ready (~20–30s)
until curl -s $MOLECULE_API/workspaces/$ANTHROPIC_WS | jq -r '.status' | grep -q ready; do
  echo "Waiting..."; sleep 5
done

# 4. Store your Gemini key as a global secret
curl -s -X PUT $MOLECULE_API/settings/secrets \
  -H "Content-Type: application/json" \
  -d '{"key":"GEMINI_API_KEY","value":"YOUR-GEMINI-KEY"}' | jq .

# 5. Create a Hermes workspace — Gemini native dispatch
GEMINI_WS=$(curl -s -X POST $MOLECULE_API/workspaces \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hermes-gemini",
    "role": "Inference worker — native Gemini path",
    "runtime": "hermes",
    "model": "gemini:gemini-2.0-flash"
  }' | jq -r '.id')
echo "Gemini workspace: $GEMINI_WS"

# 6. Pin the Gemini workspace to Gemini-only keys (no ANTHROPIC_API_KEY override)
curl -s -X PUT $MOLECULE_API/workspaces/$GEMINI_WS/secrets \
  -H "Content-Type: application/json" \
  -d '{"key":"ANTHROPIC_API_KEY","value":""}' | jq .

# 7. Confirm dispatch — send a single-turn probe to the Anthropic workspace
curl -s -X POST $MOLECULE_API/workspaces/$ANTHROPIC_WS/a2a \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0","id":"probe-1","method":"message/send",
    "params":{"message":{"role":"user","parts":[{"kind":"text","text":"Which API are you using to generate this response?"}]}}
  }' | jq '.result.parts[0].text'

# 8. Same probe to the Gemini workspace
curl -s -X POST $MOLECULE_API/workspaces/$GEMINI_WS/a2a \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0","id":"probe-2","method":"message/send",
    "params":{"message":{"role":"user","parts":[{"kind":"text","text":"Which API are you using to generate this response?"}]}}
  }' | jq '.result.parts[0].text'

# 9. Multi-turn history — Phase 2c keeps turns as turns (not flattened)
#    Send turn 1
curl -s -X POST $MOLECULE_API/workspaces/$ANTHROPIC_WS/a2a \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0","id":"turn-1","method":"message/send",
    "params":{"message":{"role":"user","parts":[{"kind":"text","text":"My name is Alice. Remember that."}]}}
  }' | jq '.result.parts[0].text'

# 10. Send turn 2 — history is automatically threaded by Hermes Phase 2c
curl -s -X POST $MOLECULE_API/workspaces/$ANTHROPIC_WS/a2a \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0","id":"turn-2","method":"message/send",
    "params":{"message":{"role":"user","parts":[{"kind":"text","text":"What is my name?"}]}}
  }' | jq '.result.parts[0].text'
# Expected: "Alice" — not "I don't know", which the old flattened path could produce
```

## Expected output

**Step 7 (Anthropic workspace):** The agent confirms it is calling the Anthropic Messages API natively. Hermes executed `_do_anthropic_native` — no OpenAI-compat translation layer.

**Step 8 (Gemini workspace):** The agent confirms Google `generateContent`. Hermes called `_do_gemini_native`, which passes `role: "model"` (not `"assistant"`) and the `parts: [{text: ...}]` wrapper the native SDK requires. The compat-shim translation that produced incorrect message format is bypassed.

**Step 10 (multi-turn, Phase 2c):** Returns `"Alice"`. Before Phase 2c, history was flattened into a single user blob — the model could recover the gist but lost clean role attribution. Phase 2c passes turns as turns: OpenAI uses `{role, content}`, Anthropic uses the same wire shape for text-only, Gemini uses `{role: "model", parts: [{text}]}`.

## How dispatch works under the hood

`HermesA2AExecutor._do_inference(user_message, history)` reads `self.provider_cfg.auth_scheme`:

```python
if self.provider_cfg.auth_scheme == "anthropic":
    return await self._do_anthropic_native(user_message, history)
elif self.provider_cfg.auth_scheme == "gemini":
    return await self._do_gemini_native(user_message, history)
else:  # "openai" + unknown (forward-compat fallback)
    return await self._do_openai_compat(user_message, history)
```

Fail-loud semantics: if the `anthropic` package isn't installed, `_do_anthropic_native` raises a clear `RuntimeError` before any inference attempt. Same for `google-genai`. Silent fallback to the compat shim would mask format errors — Molecule AI chooses loud failure.

## Building a multi-provider team

The real win surfaces in a mixed-provider agent team. Your orchestrator can fan tasks to an Anthropic worker and a Gemini worker simultaneously, each receiving properly formatted messages through their native API paths:

```bash
# Fan out from the orchestrator — both fire in parallel
curl -s -X POST $MOLECULE_API/workspaces/$ORCH_ID/a2a \
  -H "Content-Type: application/json" \
  -d "{
    \"jsonrpc\":\"2.0\",\"id\":\"fan-1\",\"method\":\"message/send\",
    \"params\":{\"message\":{\"role\":\"user\",\"parts\":[{\"kind\":\"text\",
    \"text\":\"delegate_task_async $ANTHROPIC_WS 'Draft release notes for v2.1' AND delegate_task_async $GEMINI_WS 'Summarise the last 30 days of support tickets'\"}]}}
  }" | jq .
```

Both workers use their native inference paths. No LiteLLM proxy layer. No format translation on every request. The orchestrator gets results back through the same A2A protocol regardless of which underlying model powered each task.

## Capability comparison: Hermes native vs the compat shim

What is shipping today (Phases 2a + 2b + 2c — all merged to main):

| Capability | OpenAI-compat shim | Anthropic native | Gemini native |
|---|---|---|---|
| Plain text (single-turn) | ✅ | ✅ | ✅ |
| Multi-turn history | ⚠️ flattened into one user blob | ✅ role-attributed turns | ✅ `role: "model"` + `parts` wrapper |
| Correct Gemini message format | ❌ wrong role + missing parts wrapper | — | ✅ |
| No compat-shim translation overhead | ❌ every request translated | ✅ | ✅ |

What is on the roadmap for Phase 2d (not yet shipped):

| Capability | Anthropic native | Gemini native |
|---|---|---|
| `tool_use` / `tool_result` blocks | 📋 Phase 2d | 📋 Phase 2d |
| Vision content blocks | 📋 Phase 2d | 📋 Phase 2d |
| System instructions (`system=`) | 📋 Phase 2d | 📋 Phase 2d (`system_instruction=`) |
| Extended thinking | 📋 Phase 2d | — |
| Streaming | 📋 Phase 2d | 📋 Phase 2d |

**Why Molecule AI vs Letta / AG2 / n8n:** Those frameworks handle multi-LLM at the application layer — you write different agent classes per provider. Molecule AI handles it at the infrastructure layer. Your workspace configs change; your orchestration code doesn't. Swap a Gemini worker for an Anthropic worker by changing one secret. No code redeploy.

## Related

- PR #240: [Phase 2a — native Anthropic dispatch](https://git.moleculesai.app/molecule-ai/molecule-core/pull/240)
- PR #255: [Phase 2b — native Gemini dispatch](https://git.moleculesai.app/molecule-ai/molecule-core/pull/255)
- PR #267: [Phase 2c — multi-turn history on all paths](https://git.moleculesai.app/molecule-ai/molecule-core/pull/267)
- [Hermes adapter design](../adapters/hermes-adapter-design.md)
- [Platform API reference](../api-reference.md)
- Issue [#513](https://git.moleculesai.app/molecule-ai/molecule-core/issues/513)
