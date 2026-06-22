# RFC: Decouple workspace config + skill delivery from Secrets Manager

**Status:** Draft
**Author:** CEO Assistant (on CTO direction)
**Related:** RCA #2831 (SaaS agents lose config/skills/memory), #2832 (credentials in auto-memory), #2838 (provisioner reconciliation — partial), merged runtime fix #125/#134 (memory re-inject on auto-heal + persistence discipline), seo-template #16 (slash-command format)

## 1. Summary

Workspace **config, prompts, and skills** are currently delivered to SaaS tenants through the **AWS Secrets Manager** "config bundle" path. That couples non-sensitive, potentially-large template assets to a store sized and scoped for *secrets*. The result is the recurring class of failures in #2831:

- Skills (e.g. the 716 KiB `seo-all` package) don't fit the ≤256 KiB secrets bundle → they are silently dropped → `agent_card.skills = []`.
- The "fix" attempted in #2838 was a per-template patch (`EnableSEOSkillPackage` / `SEOSkillPackageFiles()`) that hardcodes one template's files into core — non-scalable, a layering violation, and it ships disabled.
- A workspace whose config secret is absent or stale silently falls back to a stub `/configs/config.yaml` and never self-repairs.

**Proposal:** Secrets Manager keeps **only secrets**. Config + prompts + skills move to a **generic, non-secret asset channel** — the workspace fetches its template assets (incl. `agent-skills/`) from the **template repo** onto the **persisted data volume** at provision and reconciles them on every boot. This removes the size cap, deletes the per-template patch, and makes stub/missing-config workspaces self-repair.

## 2. Problem & evidence

### 2.1 Live Secrets-Manager inventory (operator acct `<AWS_ACCOUNT_ID>`, `us-east-2`)
SM holds exactly two kinds of secret:

| Secret | Contents | Verdict |
|---|---|---|
| `molecule/tenant/<tenant-id>/bootstrap` | `db_password`, `admin_token`, `secrets_encryption_key`, `tunnel_token`, `ghcr_token`, `cp_admin_api_token`, `shared_secret`, `display_session_signing_secret` | **Real secrets — correct to be in SM** |
| `molecule/workspace/<ws-id>/config` (~97+) | `{ "config.yaml": "<yaml text>" }` — sampled `88cc3af2…` = **240 bytes, config.yaml only** (no `prompts/`, no skills) | **Non-secret config in a secrets store** |

- **Zero `*skill*` secrets exist.** Skills never reach SaaS via SM.
- **JRS SEO `28f97a7f` has no `workspace/.../config` secret at all** → root cause of its 218-byte stub config + `skills:[]`: it never got a bundle and fell back to the user-data baseline stub.
- Even the bundles that exist are tiny (config.yaml only) — prompts/skills largely don't make it through for anyone.

### 2.2 Mechanism (from code, per #2831)
- Core `collectCPConfigFiles()` collects only `config.yaml` + `prompts/` from the template; the SEO skill package lives under `source/seo-agent-template-main/agent-skills/seo-all/` (716 KiB), outside the allowlist.
- CP stages that bundle to SM `molecule/workspace/<id>/config` (cp#329 moved config off the 16 KB user-data limit into SM, cap raised to 256 KiB).
- Normal restart/auto-heal calls `RestartWorkspaceAutoOpts(templatePath="", configFiles=nil)` → **no reconciliation**: a stub `/configs` is reused forever.

### 2.3 Root cause
**Transport coupling.** Non-sensitive assets (config, prompts, skills) are forced through a secrets-sized, secrets-scoped channel. This (a) caps skills out, (b) invites per-template patches, (c) silently no-ops when a config secret is missing.

## 3. Goals / non-goals

**Goals**
- Secrets and non-secret assets ride **separate channels**.
- **Generic** template-asset (config + prompts + skills) delivery for *any* template/runtime — no per-template code in core.
- **Reconcile/self-repair** on every provision, restart, restore, auto-heal — a stub/missing config heals from the template.
- No size cap on assets; skills always delivered.
- Comprehensive docs + unit tests + e2e.

**Non-goals**
- Changing what's in `tenant/<id>/bootstrap` (it stays; it's correct).
- The runtime memory-persistence fix (already merged: #125/#134) — complementary, not in scope here.
- Changing how *workspace_secrets* (per-workspace env in the tenant DB) work.

## 4. Proposed design

### 4.1 Secrets ↔ assets boundary (the core principle)
| Channel | Carries | Store |
|---|---|---|
| **Secrets** | tenant bootstrap secrets (db/admin/encryption/tunnel/ghcr/cp-admin/shared) | AWS Secrets Manager `tenant/<id>/bootstrap` (unchanged) |
| **Assets** | `config.yaml`, `prompts/*`, `agent-skills/**` | Template repo → fetched to the persisted data volume `/configs` (+ skills dir) |

Secrets Manager is no longer a config/asset transport.

### 4.2 Asset delivery — generic template fetch
At provision and on every boot, the workspace materializes its template assets from the **template repo** (Gitea), pinned to the template's `.runtime-version`/ref:

1. Provisioner records the workspace's **template identity** (repo + ref) in the workspace record (it already knows the template at create time).
2. The boot/entrypoint performs a shallow fetch of the template repo's asset paths (`config.yaml`, `prompts/`, `agent-skills/`) into `/configs` on the data volume (the volume already persists `/configs` + `/workspace` + `~/.claude`).
3. The skill loader reads skills from the delivered `agent-skills/` dir; `config.yaml: skills:` just names which to load.
4. This is **template-agnostic** — every template ships these conventional asset paths; core has no template-specific knowledge.

> Transport options for the fetch (decide in impl): (a) `git` shallow clone of the template repo with a read-scoped token delivered via the bootstrap secret (the only secret needed); (b) a tarball from the Gitea archive API; (c) an object-store (R2/S3) artifact the provisioner publishes per template-version. (a)/(b) reuse the template repo as SSOT and need no extra publish step; preferred unless artifact immutability is required.

### 4.3 Reconcile / self-repair on boot
Replace the "reuse existing config volume, templatePath=\"\"" path: on every provision/restart/restore/auto-heal, re-materialize (or verify-and-repair) the template assets. A workspace with a missing/stub/partial `/configs` heals to the template's current assets. Keep #2838's good half: the `isCPTemplateConfigFile` allowlist + abort-on-provider-error + nil-safety.

### 4.4 Deletions (remove the patch)
Delete from core: `EnableSEOSkillPackage`, `SEOSkillPackageFiles()`, `SEOSkillConfigBlock()`, `workspace-server/internal/provisioner/seo_skill_package.go`, and all references. Core carries **zero** template-specific skill knowledge.

## 5. Migration
- **Existing stub workspaces (e.g. JRS `28f97a7f`):** self-repair on next boot once §4.3 ships — no manual per-workspace fix needed. (Until then, a one-off re-provision pulls the assets.)
- **Existing `workspace/<id>/config` SM secrets:** become vestigial. Stop writing them; delete on a sweep after the asset channel is verified live (don't delete before, to allow rollback).

## 6. Security considerations
- The only secret the asset fetch needs is a **read-scoped template-repo token**, delivered via the existing `bootstrap` secret — no broadening of secret exposure.
- Assets are public, non-sensitive by definition; no credential material rides the asset channel.
- Complements #2832: secrets a user asks to persist go to `workspace_secrets` (durable env), not into config/memory; auto-memory redaction is tracked there.

## 7. Test plan
**Unit (core, `workspace-server/internal/provisioner`)**
- Provisioning a template with skills produces an asset set including `agent-skills/**` (fails if skills are dropped).
- Reconcile path with `templatePath=""` (restart/auto-heal) still materializes/repairs `/configs` from the template (no stub regression).
- Missing/stub `/configs` → self-repaired to the template assets.
- No SEO/template-specific symbols remain (grep gate).
- Secrets boundary: provisioning writes secrets only to `bootstrap`; no config/skill bytes land in any SM secret.

**E2E (staging)**
- Provision a real SEO-template workspace → assert `agent_card.skills` is non-empty and the `/seo-*` commands are present on the data volume.
- Restart the workspace → assert `/configs` + skills survive (or are re-materialized), config is not a stub.
- Corrupt `/configs` to a stub → boot → assert self-repair restores config + skills.

## 8. Rollout
1. Land the asset channel + reconciliation behind the generic path (auto-bump templates/runtime per repo default).
2. Verify on a staging SEO workspace (e2e above).
3. Re-provision/boot existing SaaS SEO workspaces (incl. JRS) to pick up assets; confirm `skills` non-empty.
4. Sweep + delete vestigial `workspace/<id>/config` SM secrets after a soak.

## 9. Alternatives considered
- **Keep SM, raise the cap / chunk skills across secrets:** still misuses a secrets store for public assets; per-secret limits + cost; rejected.
- **Per-template flags (`EnableSEOSkillPackage`, status quo of #2838):** O(templates) patches, layering violation, ships disabled; rejected (this RFC deletes it).
- **Bake skills into the runtime image:** couples per-template content to image builds, breaks the "template owns its skills" SSOT; rejected.

## 10a. Sibling anti-pattern found in the audit: the concierge identity is hardcoded in core

The same "should be a template, not a patch" smell exists for the **org concierge / platform agent**. `workspace-server/internal/handlers/platform_agent.go` hardcodes its entire identity in Go:
- `conciergeSystemPromptTmpl` — the full "You are … the Org Concierge / org orchestrator" system prompt (string literal).
- `conciergeMCPServersBlock` — its `mcp_servers:` config block.
- `conciergeDeclaredModel = "moonshot/kimi-k2.6"`, `conciergeRuntime = "claude-code"`.
- `conciergeIdentityFiles()` overlays these as `system-prompt.md` + `config.yaml` at provision.

The concierge has an image (`Dockerfile.platform-agent`) but **no template home for its identity** — so its prompt/config/model live as core string literals, exactly like the SEO skill files did. The fix is the same abstraction: make the concierge a **platform-agent template** (prompt/config/model in template files) delivered via this RFC's generic asset channel, and delete the `conciergeSystemPromptTmpl`/`conciergeMCPServersBlock`/`conciergeIdentityFiles` literals from core. The asset channel introduced here is the enabler for removing **both** the SEO patch **and** the concierge hardcoding.

**Audit scope notes:** per-runtime branches in core (e.g. `if runtime == "hermes"` for provision-timeout/config paths) are adapter/registry concerns, not per-template patches — lower priority, candidates for data-driven cleanup but not in this RFC. No plugin-behavior was found hardcoded in core (the plugin system is used for extensions). The two clear "should be a template" patches are: (1) SEO skill package, (2) concierge identity.

## 10. What we keep
- The merged runtime memory fix (#125/#134) — orthogonal.
- #2838's reconciliation scaffold + allowlist (the generic half) — retained; only the SEO-specific injection is removed.
- `tenant/<id>/bootstrap` in SM — unchanged.
