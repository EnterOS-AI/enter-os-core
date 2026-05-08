# Workspace Backend Parity Matrix

**Status:** living document — update when you ship a feature that touches one backend.
**Owner:** workspace-server + controlplane teams.
**Last audit:** 2026-05-07 (plugin install/uninstall closed for EC2 backend via EIC SSH push to the bind-mounted `/configs/plugins/<name>/`, mirroring the Files API PR #1702 pattern).

## Why this exists

Molecule AI ships workspaces on two backends:

- **Docker** — the self-hosted / local-dev path. `provisioner.Docker` in `workspace-server/internal/provisioner/`. Each workspace is a container on the same daemon as the platform.
- **EC2 (SaaS)** — the control-plane path. `provisioner.CPProvisioner` in the same directory, which calls the control plane at `POST /cp/workspaces/provision`. Each workspace is its own EC2 instance.

Every user-visible workspace feature should work on both backends unless it is fundamentally tied to one substrate (e.g. `docker logs` command, AWS serial console). When the two diverge silently — a handler works on Docker but quietly 500s on EC2, or vice versa — users hit dead ends that look like bugs but are actually architectural gaps.

This document is the canonical matrix. If you are landing a workspace-facing feature, update the row before you merge.

## How to dispatch (the SoT pattern)

When a handler needs to start, stop, or check whether-something-can-run a workspace, it MUST go through the centralized dispatcher on `WorkspaceHandler`:

| Need | Use | Source |
|---|---|---|
| Start a workspace | `provisionWorkspaceAuto(ctx, ...)` | `workspace.go:130` |
| Stop a workspace | `StopWorkspaceAuto(ctx, wsID)` | `workspace.go:172` |
| Gate "do we have any backend wired?" | `HasProvisioner()` | `workspace.go:115` |

Each dispatcher routes to `cpProv.X()` when the SaaS backend is wired, then `provisioner.X()` when the Docker backend is wired, then a defined fallback (`provisionWorkspaceAuto` self-marks-failed; `StopWorkspaceAuto` no-ops; `HasProvisioner` returns false).

**Rule: do not call `h.cpProv.Stop`, `h.provisioner.Stop`, `h.cpProv.Start`, or `h.provisioner.Start` directly from a handler.** Source-level pins (`TestNoCallSiteCallsDirectProvisionerExceptAuto`, `TestNoCallSiteCallsBareStop`) gate this at CI; they exist because the same drift class shipped twice — TeamHandler.Expand (#2367) bypassed routing on Start, then `team.go:208` + `workspace_crud.go:432` bypassed it on Stop (#2813, #2814) for ~6 months.

Allowed exceptions (in the source-pin allowlists):
- `workspace.go` and `workspace_provision.go` — define the per-backend bodies the dispatcher routes between.
- `workspace_restart.go` — pre-dates the dispatchers and uses manual if-cpProv-else dispatch with retry semantics tuned for the restart hot path. Consolidation tracked in #2799.
- `container_files.go` — drives the Docker daemon directly for short-lived file-copy containers; no workspace-level Stop semantics involved.

For "do we have any backend?", use `HasProvisioner()`, never bare `h.provisioner == nil && h.cpProv == nil`. Source-level pin `TestNoBareBothNilCheck` enforces this — added 2026-05-05 after the hongming org-import incident showed the bare check shape was a recurring drift target.

## The matrix

| Feature | File(s) | Docker | EC2 | Verdict |
|---|---|---|---|---|
| **Lifecycle** | | | | |
| Create | `workspace.go:130` `provisionWorkspaceAuto` → `provisionWorkspace()` (Docker) / `provisionWorkspaceCP()` (CP) | dispatched | dispatched | ✅ parity (single source of truth, PR #2811) |
| Start | `provisioner.go:140-325` | container create + image pull | EC2 `RunInstance` via CP | ✅ parity |
| Stop | `workspace.go:172` `StopWorkspaceAuto` → `provisioner.Stop()` (Docker) / `cpProv.Stop()` (CP) | dispatched | dispatched | ✅ parity (single source of truth, PR #2824) |
| Restart | `workspace_restart.go:45-210` | reads runtime from live container before stop | reads runtime from DB only | ⚠️ divergent — config-change + crash window can boot old runtime on EC2 |
| Delete | `workspace_crud.go` `stopAndRemove` → `StopWorkspaceAuto` + Docker-only `RemoveVolume` | stop + volume rm | stop only (stateless — CP has no volumes) | ✅ parity (PR #2824 closed the SaaS-leak gap) |
| Org-import (bulk Create) | `org_import.go:178` gates on `h.workspace.HasProvisioner()`; routes through `provisionWorkspaceAuto` per workspace | dispatched | dispatched | ✅ parity (PR #2811 closed the SaaS-skip gate) |
| Team-collapse (bulk Stop) | `team.go:206` calls `StopWorkspaceAuto` for each child | dispatched | dispatched | ✅ parity (PR #2824 closed the SaaS-leak gap) |
| **Secrets** | | | | |
| Create / update | `secrets.go` | DB insert, injected at container start | DB insert, injected via user-data at boot | ✅ parity |
| Redaction | `workspace_provision.go:251` | applied at memory-seed time | applied at agent runtime | ⚠️ divergent — timing differs |
| **Files API** | | | | |
| List / Read / Write / Replace / Delete | `container_files.go`, `template_import.go` | `docker exec` + tar `CopyToContainer` | SSH via EIC tunnel (PR #1702) | ✅ parity as of 2026-04-22 (previously docker-only) |
| **Plugins** | | | | |
| Install / uninstall / list | `plugins_install.go` + `plugins_install_eic.go` | `deliverToContainer()` → exec+`CopyToContainer` on local container | `instance_id` set → EIC SSH push of the staged tarball into the EC2's bind-mounted `/configs/plugins/<name>/` (per `workspaceFilePathPrefix`), `chown 1000:1000`, restart | ✅ parity |
| **Terminal (WebSocket)** | | | | |
| Dispatch | `terminal.go:90-105` | `instance_id=""` → `handleLocalConnect` → `docker attach` | `instance_id` set → `handleRemoteConnect` → EIC SSH + `docker exec` | ✅ parity (different implementations, same UX) |
| **A2A proxy** | | | | |
| Forward | `a2a_proxy.go` | `127.0.0.1:<port>` | EC2 private IP inside tenant VPC | ✅ parity |
| Liveness | `a2a_proxy_helpers.go` | `provisioner.IsRunning()` | `cpProv.IsRunning()` (DB-backed) | ✅ parity |
| Channel envelope enrichment (peer_name / peer_role / agent_card_url) | `a2a_proxy.go` + workspace-runtime channel emitter (PR #2471) | inbox row carries enriched fields | inbox row carries enriched fields | ✅ parity as of 2026-05-02 |
| **MCP tools (a2a)** | | | | |
| `chat_history` — fetch prior turns with a peer | `mcp_server.go` + workspace-runtime `a2a_mcp` (PR #2474) | runtime-served, backend-agnostic | runtime-served, backend-agnostic | ✅ parity as of 2026-05-02 |
| **Activity API** | | | | |
| `before_ts` paging on `/workspaces/:id/activity` | `activity.go` (PR #2476) | DB-driven | DB-driven | ✅ parity as of 2026-05-02 |
| `peer_id` filter on `/workspaces/:id/activity` | `activity.go` (PR #2472) | DB-driven | DB-driven | ✅ parity as of 2026-05-02 |
| **Config / template injection** | | | | |
| Template copy at provision | `provisioner.go:553-648` | host walk → tar → `CopyToContainer(/configs)` | CP user-data bakes template into bootstrap script | ⚠️ divergent — sync (docker) vs async (EC2) |
| Runtime config hot-reload | `templates.go` + handlers | no hot-reload — restart required | no hot-reload — restart required | ✅ parity (both require restart; acceptable) |
| **Memory (HMA)** | | | | |
| Seed initial memories | `workspace_provision.go:226-260` | DB insert at provision time | DB insert at provision time | ✅ parity |
| **Bootstrap signals** | | | | |
| Ready detection | registry `/registry/register` | container heartbeat | tenant heartbeat + boot-event phone-home (CP `bootevents` table + `wait_platform_health=ok`) | ✅ parity as of molecule-controlplane#235 |
| Console / log output | `workspace_bootstrap.go` | `docker logs` | `ec2:GetConsoleOutput` via CP proxy | 🟡 ec2-only (docker has `docker logs` directly; no unified API) |
| `runtime_wedge` post-`execute()` smoke gate | workspace-runtime `smoke_mode.py` (PRs #2473 + #2475) | runtime-served, surfaces SDK-init wedges to wheel-smoke + container start | runtime-served, surfaces SDK-init wedges to wheel-smoke + container start | ✅ parity as of 2026-05-02 |
| **Test infrastructure** | | | | |
| Canvas-E2E `.playwright-staging-state.json` written before any CP call | `tools/e2e-staging-setup` (PR #2327, 2026-04-30) | n/a — staging-only safety net | required so workflow safety-net can find slug; pattern-sweeping by date prefix poisons concurrent runs | ✅ enforced (staging E2E) |
| **Orphan cleanup** | | | | |
| Detect + terminate stale | `healthsweep.go` + CP `DeprovisionInstance` | Docker daemon scan | CP OrgID-tag cascade (molecule-controlplane#234) | ✅ parity as of 2026-04-23 |
| **Health / budget / schedules** | | | | |
| Budget enforcement | `budget.go` | DB-driven | DB-driven | ✅ parity |
| Schedule execution | `workspace_restart.go:235-280` | `provisioner.Stop()` + re-provision | `cpProv.Stop()` + CP auto-restart | ✅ parity |
| Liveness probe | `healthsweep.go` | `provisioner.IsRunning()` | `cpProv.IsRunning()` | ✅ parity |
| **Template recipes (per-template user-data)** | | | | |
| Hermes `install.sh` (bare-host) / `start.sh` (Docker) | `molecule-ai-workspace-template-hermes/` | `start.sh` entrypoint | `install.sh` called by CP user-data hook | ⚠️ structurally divergent — two scripts maintained separately; **parity enforced by CI lint**, see `tools/check-template-parity.sh` |

## Top drift risks (ordered by production impact)

1. **Plugin install is docker-only.** Hot-install UX (POST /plugins) calls `deliverToContainer()` which requires a live Docker daemon. On EC2, there is no equivalent — plugins must be baked into user-data at boot. SaaS users who want to iterate on plugins without restarting today cannot. **Fix path:** add a CP-side plugin-manager endpoint that the tenant workspace-server proxies to, or document "restart required" on SaaS.
2. **Template config injection is sync on Docker and async on EC2.** Docker writes config files right before `ContainerStart`; EC2 embeds them in user-data and they materialize whenever cloud-init runs. A workspace that starts serving before cloud-init completes can see stale config. **Fix path:** make the canvas wait for `wait_platform_health=ok` boot-event before flipping to `online`, same mechanism the provisioning path uses.
3. **Restart divergence on runtime changes.** Docker re-reads `/configs/config.yaml` from the container before stop, so a changed `runtime:` survives a restart even if the DB isn't synced. EC2 trusts the DB only. If you change the runtime via the Config tab and the handler races the restart, Docker will land on the new runtime, EC2 will land on the old one. **Fix path:** make the Config-tab save explicitly flush to DB before kicking off a restart, not deferred.
4. **Console-output asymmetry.** Users debugging a stuck workspace on Docker see `docker logs`; on EC2 they see `GetConsoleOutput`. The two outputs look nothing alike. **Fix path:** expose a unified `GET /workspaces/:id/boot-log` that proxies to whichever backend serves the data. Already partly there via `cp_provisioner.Console`.
5. **Template script drift.** `install.sh` and `start.sh` in each template repo do the same high-level work (install hermes-agent, write .env, write config.yaml, start gateway) but must be kept byte-level consistent on the provider-key forwarding block. Easy to forget. Enforced now by `tools/check-template-parity.sh` (see below) — run it in each template repo's CI.
6. **Both backends panic when underlying client is nil.** Discovered by the contract-test scaffold landing in this PR: `Provisioner.{Stop,IsRunning}` nil-dereferences the Docker client, and `CPProvisioner.{Stop,IsRunning}` nil-dereferences `httpClient`. The real code always sets these, so this is theoretical in prod — but it means the contract runner can't execute scenarios against zero-value backends. **Fix path:** guard each method with `if p.docker == nil { return false, errNoBackend }` (and equivalent for CP), then flip the `t.Skip` in the contract tests to `t.Run`.

## Enforcement

- **`tools/check-template-parity.sh`** (this repo) — ensures `install.sh` and `start.sh` in a template repo forward identical sets of provider keys. Wire into each template repo's CI as `bash $MONOREPO/tools/check-template-parity.sh install.sh start.sh`.
- **Contract tests** (stub) — `workspace-server/internal/provisioner/backend_contract_test.go` defines the behaviors every `provisioner.Provisioner` implementation must satisfy. Fails compile when a method drifts between `Docker` and `CPProvisioner`. Scenario-level runs are `t.Skip`'d today pending drift risk #6 (see above) — compile-time assertions still catch method drift.
- **Source-level dispatcher pins** — `workspace_provision_auto_test.go` enforces the SoT pattern documented above:
  - `TestNoCallSiteCallsDirectProvisionerExceptAuto` — no handler calls `.provisionWorkspace(` or `.provisionWorkspaceCP(` directly outside the dispatcher's allowlist.
  - `TestNoCallSiteCallsBareStop` — no handler calls `.provisioner.Stop(` or `.cpProv.Stop(` directly outside the dispatcher's allowlist (strips Go comments before substring match so archaeology in code comments doesn't trip the gate).
  - `TestNoBareBothNilCheck` — no production code uses `h.provisioner == nil && h.cpProv == nil`; must use `!h.HasProvisioner()`.
  - `TestOrgImportGate_UsesHasProvisionerNotBareField` — pins the org-import provisioning gate against the bare-Docker-check shape that caused the 2026-05-05 hongming incident.

## How to update this doc

When you land a feature that touches a handler dispatch on `h.cpProv != nil`, add or update the matching row. If you can't implement both backends in the same PR, mark the row `docker-only` or `ec2-only` and file an issue tracking the gap.

### When you add a NEW dispatch site

If you find yourself writing `if h.cpProv != nil { ... } else if h.provisioner != nil { ... }` for a new operation (Pause, Hibernate, Snapshot, etc.):

1. Add a `<Op>WorkspaceAuto` method on `WorkspaceHandler` next to the existing dispatchers. Mirror the docstring shape: routing, no-backend fallback, ordering rationale.
2. Add a source-level pin in `workspace_provision_auto_test.go` — the bare-call shape your dispatcher replaces, fail when a handler reintroduces it.
3. Add a row to the matrix above with the dispatcher reference.
4. If your operation has retry semantics specific to a hot path, leave them in the original location for now and file a follow-up under #2799 — don't bake retry into the generic dispatcher unless every caller benefits.

The pattern is "one dispatcher per verb." Don't fold every operation into `provisionWorkspaceAuto` — different verbs have different no-backend fallbacks (mark-failed for Start, no-op for Stop, false for Has).
