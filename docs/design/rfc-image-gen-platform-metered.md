# RFC: Plugin proxy socket — a generic metered egress primitive (two-tier registry); image generation = first consumer

- **Status:** Draft — for CTO review (do not build the generic socket until the two-tier shape is signed off)
- **Date:** 2026-06-20 (rev 4 — re-scoped from a bespoke image handler to a generic socket after design review)
- **Supersedes:** the bespoke `ProxyImages` approach in **CP #880** (that PR is gutted down to the migration + the socket's first capability entry — see §10).

## 1. Why this changed shape

The first cut added a bespoke `ProxyImages` handler to the LLM proxy. That's an **addon**: do it again for video, TTS, embeddings, rerank, and the proxy becomes a junk drawer of per-capability handlers — which defeats the point of the plugin system. If we touch core, it should be a **fundamental, generic primitive built once**, not another special case.

The codebase is already moving this way: `providers.yaml` (internal#718 / vertex-provider-ssot-endpoint) pulled routing facts (upstream URL, auth mode, wire prefix) **out of hardcoded Go into a registry**, and its own comment says *"Phase 2 migrates the remaining static providers."* This RFC is that endgame: a **generic, registry-driven metered egress socket** that any plugin uses, where adding a capability is **data, not code**.

## 2. The primitive: one metered egress socket

Core exposes ONE generic path. A plugin calls it; everything sensitive resolves server-side; the plugin only ever receives what is safe to hand out.

```
plugin ──> POST /internal/llm/proxy   {capability/model, request}
              │  CORE (generic — built once):
              │   1. AUTH: workspace↔org handshake            ← already exists
              │   2. RESOLVE capability from the REGISTRY     ← providers.yaml SSOT
              │   3. INJECT vendor credential server-side     ← key-env | wif (registry auth_mode)
              │   4. FORWARD to the registry-declared upstream
              │   5. METER usage via registry-declared paths → debit credits (existing)
              │   6. RETURN per registry response_mode, stripped of anything unsafe
              └──> {safe output}   (never a key; never raw upstream internals)
```

### 2.1 Trust model (why the box never holds a vendor key)
The plugin runs on the tenant's workspace box, where the tenant has **root**. The box holds an **org-scoped credential** (the org/admin token + workspace id, read from workspace env) and uses it to handshake with CP (`resolveLLMProxyPrincipal` + `workspaceBelongsToOrg`, already in core). The box does **NOT** hold the vendor key.

Blast radius of a compromised box is therefore **asymmetric and bounded**:
- attacker can spend **that one org's** credits (≤ balance + overage cap) — the tenant's own loss;
- the platform's **master vendor keys stay in CP**, and **every other org is untouched** — no unmetered global abuse, no key exfiltration.

That asymmetry is the whole reason keys + billing live in CP and only the handshake lives on the box. (Hardening seam, not v1: make the box token *workspace*-scoped rather than org-admin to shrink the radius further.)

## 3. Capabilities are data (the registry)

A capability is a registry entry (extends `providers.yaml`, the existing SSOT). It declares everything the generic socket needs — no per-capability Go:

```yaml
- name: gemini-image
  capability: image
  tier: platform_metered            # A (our keys/credits) | byok (their key)
  upstream: vertex                  # reuses existing auth_mode: wif_adc (keyless WIF mint)
  endpoint: ":generateContent"      # native image surface (vs the openapi text surface)
  billing_model: gemini-2.5-flash-image   # → llm_price_catalog row
  usage:                            # declarative extraction — no parse func
    input_path:  usageMetadata.promptTokenCount
    output_path: usageMetadata.candidatesTokenCount
  response_mode: blob               # see §5; json | blob
```

Adding image, video, TTS, embeddings → **a registry entry + a price row. Zero new handler.**

## 4. Two-tier registry (the marketplace split)

A capability entry binds **{ upstream · which credential to inject · the price }**. Whether an entry can be **third-party-dynamic** depends entirely on *whose credential*:

### Tier A — platform-metered (our keys, our credits): **platform-curated, NOT freely dynamic**
A free-for-all here is catastrophic: a plugin could declare *"forward to evil.com, inject the platform OpenAI key"* (key exfiltration), *"price = 0"* (billing bypass), or point egress anywhere (SSRF). So entries that spend **our** money with **our** keys — image gen included — are **platform-controlled**: a registry/DB row added through a **vetted onboarding / reviewed change**, never self-served. (Curated ≠ hardcoded-in-Go: it's a trusted config row, but the trust decision is ours.)

### Tier B — BYOK (the plugin brings its own key): **dynamically self-registrable**
Nothing of ours is at stake — the third party's credential, their cost. So a third-party plugin **can register its own capability dynamically.** CP still proxies it (egress control + observability) but injects the **plugin's** key and **does not debit platform credits**.

The **marketplace scales through Tier B** (dynamic, self-served, ~10K plugins/day — see the marketplace RFC `project_marketplace_private_template_delivery`); **Tier A** stays a small curated set of platform-subsidized capabilities. One socket serves both; the only differences are *whose credential is injected* and *whether platform credits are debited*. (Tier B is a designed-in seam in v1, not necessarily shipped day one.)

## 5. The only genuinely-new core primitives (built once, reused forever)

1. **Declarative usage extraction** — a registry-declared JSON path per token bucket (`input_path`, `output_path`, `cached_path`, …). Retires the `parseOpenAIUsage` / `parseAnthropicUsage` / `parseOpenAIResponsesUsage` sprawl; a new vendor's metering becomes config, not Go.
2. **A small fixed set of response modes** — how the socket returns the upstream result safely:
   - `json` — passthrough the (sanitized) JSON (text/chat/responses/embeddings).
   - `blob` — the upstream returns binary (image/audio). Two sub-modes:
     - `blob_url` — CP stores the bytes (R2) and returns a time-boxed **presigned URL** (uniform across vendors; the agent just gets a link).
     - `blob_passthrough` — CP returns the bytes to the plugin; the plugin writes them into the workspace. Keeps core thinnest; output is a workspace file, not a hosted URL.
   - **Open decision (D1):** default response_mode for images — `blob_url` (uniform URL, +R2 in core) vs `blob_passthrough` (thinnest core, file path out). Recommend `blob_url` for a clean agent UX, behind a per-capability flag so a capability can choose.

Everything else (auth, credential injection by `auth_mode`, forwarding, the credits debit, the fail-closed price gate) **already exists** — the socket wires the existing pieces generically.

## 6. Image generation = first Tier-A consumer (the concrete instance)

Image gen proves the primitive. As registry entries (Tier A, our keys/credits):

- **`google/gemini-2.5-flash-image`** ("Nano Banana") — `upstream: vertex`, reuses the **existing keyless `molecule-vertex` WIF mint** (`internal/vertexauth.Token`) the proxy already uses for Gemini text. Native `:generateContent`, `responseModalities:["IMAGE"]`, location `global`. Verified live 2026-06-20: HTTP 200, `inlineData image/png`, `usageMetadata.candidatesTokenCount=1290` (the 1290 tok/image basis). **Zero new credentials.**
- **`openai/gpt-image-2`** — `upstream: openai`, platform OpenAI key (Infisical). Via the OpenAI image surface (Images API, or the Responses API image tool the proxy already proxies — chosen at build time).
- **Pricing (migration):** image SKUs in `llm_price_catalog` at **vendor list × 1.5** (markup baked into the row). Both vendors meter token-based (Gemini 1290 tok/image; gpt-image-2 token-based), so the existing per-token columns fit with no schema change. Unpriced image model → **422 pre-serve** (the anti-$0-leak gate). Anti-free-serve: if a vendor omits usage, synthesize the known output-token count so the debit always fires.
- **Output:** per §5 response_mode (D1).

## 7. Billing (unchanged path)
Reuses `recordProxiedLLMUsage` → `ChargeLLMUsage` → `DebitWithOverage`: meter (declarative) → price-catalog lookup → debit org credits → overage up to cap → 402 when exhausted. Image gen is **uncapped**; credits are the only limit. Tier B (BYOK) records non-billable usage (observability) and debits nothing.

## 8. What lives where (footprint)
| Piece | Where | Size |
|---|---|---|
| Generic socket (auth→resolve→inject→forward→meter→respond) | **CP** core | built once |
| Declarative usage extraction + response modes (json/blob) | **CP** core | built once |
| Each capability (image, video, …) | **registry entry + price row** | data |
| The plugin (`molecule-ai-plugin-image-gen` etc.) | plugin repo | thin: call socket → hand result to agent |

## 9. The plugin (thin, unchanged in spirit)
`molecule-ai-plugin-image-gen` — `plugin.yaml` + `settings-fragment.json` + a small MCP adaptor exposing `generate_image` / `edit_image` / `list_image_models`. Each tool reads workspace env (org/workspace id + handshake token), POSTs the socket, and returns the result (URL or file path per D1). No keys, no billing, no storage, no vendor-specific logic.

## 10. Relationship to CP #880
CP #880 (bespoke `ProxyImages` + R2 wiring + per-vendor parsers + tests) is **superseded**. Keep from it: **migration 055** (image price rows) and the verified vendor request/response shapes (they become the `gemini-image` / `gpt-image-2` registry entries + the `blob` response_mode). Drop: the bespoke handler, the hardcoded per-vendor parse funcs, the standalone storage wiring (folds into `response_mode: blob_url`). Net: #880 shrinks to the migration; the rest re-lands as the generic socket.

## 11. Rollout
1. **Generic socket alongside the existing text handlers** (do NOT converge text in the same change — don't destabilize the live text path). New capabilities route through the socket; chat/completions/messages/responses keep working as-is.
2. Declarative usage extraction + response modes (`json`, `blob`).
3. Tier-A image capability entries (`gemini-image`, `gpt-image-2`) + price rows. Inert until `MOLECULE_IMAGE_GEN_BUCKET` (+ R2 creds) set, if `blob_url`.
4. Thin `molecule-ai-plugin-image-gen`.
5. Staging e2e: image gen debits credits + returns output; out-of-credits → 402; unpriced → 422; (edit works on gemini).
6. **Follow-ups (designed-in, not v1):** Tier-B BYOK dynamic registration; converge the text handlers onto the socket (internal#718 Phase 2); workspace-scoped box token.

## 12. Open decisions
- **D1 (§5):** default image `response_mode` — `blob_url` (recommended) vs `blob_passthrough`.
- **D2 (§11.1):** confirm "socket alongside, converge text later" (recommended) vs converge text now.
- **D3 (§6):** OpenAI image via Images API vs the already-proxied Responses image tool — pick at build (favor the one needing least new egress surface).
- **D4 (deploy):** confirm `molecule-vertex` has `gemini-2.5-flash-image` enabled (same API surface as the gemini-2.5-pro/flash it already serves) — proven by staging e2e; the WIF mint is AWS-identity-bound, not locally exercisable.

## 13. Alternatives considered
- **Bespoke per-capability handlers** (`ProxyImages`, future `ProxyVideo`, …) — rejected: addon sprawl, defeats the plugin system. (This RFC's whole motivation.)
- **Plugin calls the vendor directly** — rejected: vendor keys on a root-accessible tenant box = the keyless-Vertex billing leak the codebase already closed; self-reported usage is forgeable.
- **Separate SA key on `molecules-ai-proxy`** (rev 3) — rejected/retired: the proxy already does keyless `molecule-vertex` WIF; the SA key + org-policy exception were a redundant detour (Infisical secret deleted; owner GCP-console cleanup pending).
- **Single platform god-token for all capabilities** — rejected: no per-seller isolation/entitlement; conflicts with the marketplace RFC. Hence the two-tier split.
