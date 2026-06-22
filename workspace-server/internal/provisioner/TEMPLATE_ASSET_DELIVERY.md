# Template asset delivery (config + prompts + skills)

> Status: live as of RFC #2843 (PRs #2845 core channel + #2857 fetcher/wiring).
> Scope: how a workspace receives its **non-secret** template assets
> (`config.yaml`, `prompts/**`, `agent-skills/**`) — decoupled from AWS
> Secrets Manager.

## Why this exists

Before RFC #2843, config/prompts/skills were bundled into the AWS Secrets
Manager `molecule/workspace/<id>/config` secret. That coupled non-sensitive,
sometimes-large assets (e.g. the ~716 KiB `seo-all` skill package) to a store
sized/scoped for **secrets** (≤256 KiB), so skills were silently dropped
(`agent_card.skills == []`) and a missing/stale config secret left a workspace
on a stub `/configs` forever. See RFC #2843 for the full root-cause.

The fix: **Secrets Manager carries only secrets.** Config + prompts + skills
ride a separate, generic, non-secret **template-asset channel** fetched from
the template repo onto the persisted data volume, and reconciled on every boot.

## The two channels

| Channel | Carries | Store | Size bound |
|---|---|---|---|
| **Secrets** | tenant bootstrap secrets (db/admin/encryption/tunnel/ghcr/cp-admin/shared) | SM `molecule/tenant/<id>/bootstrap` | 256 KiB (SM) |
| **Assets** | `config.yaml`, `prompts/**`, `agent-skills/**` | template repo (Gitea) → data volume `/configs` | 16 MiB (transport DoS guard only) |

The asset channel is **never** a secrets transport; the secrets channel is
**never** an asset transport.

## How delivery works

1. **Identity.** At boot, `initTemplateRepoByName()` (package init in
   `workspace_provision.go`) populates `templateRepoByName` from the runtime
   `manifest.json` `workspace_templates` block — `runtime -> {repo, ref}`.
   `templateIdentityForRuntime(runtime)` returns `"<owner>/<repo>@<ref>"`, or
   `("", false)` for runtimes with no template (`external`/`kimi`/`mock`).
   The map is **reset and re-populated every call**, so dropping a template
   from the manifest drops its identity (reconcile-on-boot semantic).

2. **Fetch.** `giteaTemplateAssetFetcher.Load(ctx, identity)` issues
   `GET {baseURL}/api/v1/repos/<owner>/<repo>/archive/<ref>.tar.gz` with
   `Authorization: token <PAT>`, gunzips, stream-extracts, strips the archive's
   top-level dir, and returns only **allowlisted** paths
   (`config.yaml`, `prompts/**`, `agent-skills/**`). Traversal (`../`) entries
   are rejected; a 16 MiB per-file safety bound prevents a hostile tarball from
   exhausting memory (the real cap is consumer-side).

3. **Materialize.** `collectCPConfigFiles` (`cp_provisioner.go`) calls the
   fetcher only when `cfg.TemplateAssetFetcher != nil && cfg.TemplateIdentity
   != ""`, re-gates each path through `IsCPTemplateAssetPath`, and routes assets
   into the **`TemplateAssets`** field (16 MiB bound) — **separate** from the
   SM-bound `ConfigFiles` field (256 KiB). Agent-owned paths
   (`MEMORY.md`, `USER.md`, `CLAUDE.md`, `.claude/sessions/**`, `/workspace`)
   are **rejected** by the allowlist and can never be clobbered by a fetch.

4. **Reconcile every boot.** `buildProvisionerConfig` is the single shared
   config builder used by **both** first-provision and the restart/auto-heal
   path (`workspace.go` → `provisionWorkspaceAuto` → `workspace_provision_shared.go`
   → `buildProvisionerConfig`). So a stub/missing/partial `/configs` self-repairs
   to the template's current assets on every restart, not just first provision.

## Fail-closed / fail-open contract

- Any transport/extract/parse error from `Load` returns a non-nil error and the
  provision **aborts** (no silent regression to stub `/configs`).
- If `MOLECULE_TEMPLATE_REPO_TOKEN` is **unset**, the fetcher is left **nil**
  (logged at startup) and `collectCPConfigFiles` skips it — pre-RFC behavior
  preserved for self-host / unconfigured deployments (the asset channel is
  opt-in via the token).

## Configuration (env)

| Var | Required | Default | Notes |
|---|---|---|---|
| `MOLECULE_TEMPLATE_REPO_TOKEN` | to enable the channel | (unset → disabled) | **read-only**, per-identity Gitea PAT scoped to the template repos. NOT a founder PAT. |
| `MOLECULE_GITEA_BASE_URL` | no | `https://git.moleculesai.app` | override for staging / a Gitea mirror; the `/api/v1/...` suffix is appended by the fetcher. |

## Runbook: provision the read-only token

1. Mint (or reuse from the per-persona Infisical SSOT) a **read-only** Gitea PAT
   with repo-read scope on the template repos:
   `molecule-ai/molecule-ai-workspace-template-{claude-code,hermes,openclaw,codex,google-adk,seo-agent}`.
   Do **not** use a founder PAT or a workspace-admin token.
2. Store it in Infisical SSOT and project it to the workspace-server env as
   `MOLECULE_TEMPLATE_REPO_TOKEN`. Roll **staging first**, validate, then prod.
3. Validate (RFC §7): provision a staging SEO workspace → assert
   `agent_card.skills` is non-empty and the `/seo-*` commands are present on the
   data volume; restart the workspace and assert they survive.

## Runbook: add a new template

1. Add the template repo + `agent-skills/**` to the template, and a
   `workspace_templates` entry (`name`, `repo`, `ref`) in `manifest.json`.
2. Ensure the read-only PAT's scope includes the new repo.
3. No core code change is required — delivery is generic over the manifest.

## Related

- RFC: `docs/design/rfc-decouple-config-skill-delivery.md`
- Code: `internal/provisioner/gitea_template_assets.go`,
  `internal/handlers/runtime_registry.go`,
  `internal/provisioner/cp_provisioner.go` (`collectCPConfigFiles`,
  `IsCPTemplateAssetPath`).
- Sibling cleanup tracked by RFC §10a: de-hardcode the concierge identity into a
  platform-agent template delivered via this same channel.
