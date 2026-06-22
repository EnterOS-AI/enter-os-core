# SaaS prod migration — 2026-04-19

Promoted staging → main on both `Molecule-AI/molecule-controlplane` and `Molecule-AI/enter-os-core`. This note captures the prod cutover deltas so ops can cross-check against the running system.

## What changed

Ten PRs landed, split across the two repos:

**Control plane (`molecule-controlplane`)**
- PR #50 — C1/C2/C3: bearer auth on `/cp/workspaces/*`, shell-escape tenant user-data, per-tenant security group
- PR #51 — H1/H2: crash-safe `SECRETS_ENCRYPTION_KEY` log, dropped `admin_token` from `/instance` SELECT
- PR #52 — SSRF guard on `platform_url`
- PR #53 — CP injects `MOLECULE_CP_SHARED_SECRET` + `MOLECULE_CP_URL` into tenant env
- PR #54 — Stripe webhook body capped at 1 MiB

**Core (`enter-os-core` / this repo)**
- PR #978 — H3/H4: LimitReader on Discord webhook + workspace config PATCH
- PR #979 — C4: `AdminAuth` fail-closed on fresh install when `ADMIN_TOKEN` is set
- PR #980 — log-scrub: dropped token prefix logging, stopped logging raw upstream response bodies
- PR #981 — tenant `CPProvisioner` attaches the CP bearer on every outbound `/cp/workspaces/*` call
- PR #982 — Canvas API fetch timeout (15s)
- PR #984 — E2E smoke test sync for #966 (public GET no longer exposes `current_task`)

## New prod env vars (Railway, project `molecule-platform`, env `production`)

Set before the CP merge landed:

| Variable | Value shape | Purpose |
|---|---|---|
| `PROVISION_SHARED_SECRET` | 32-byte hex | Gates `/cp/workspaces/*` on CP. Routes refuse to mount when unset — C1 fail-closed. |
| `EC2_VPC_ID` | `vpc-…` | Enables per-tenant SG creation (C3). Shared-SG fallback emits a startup warning. |
| `CP_BASE_URL` | `https://api.moleculesai.app` | Injected into newly-provisioned tenant containers as `MOLECULE_CP_URL`. |

The live prod `PROVISION_SHARED_SECRET` value is held only in Railway; not committed anywhere. Rotate by `railway variables --set` + redeploy.

## Existing-tenant migration (the sharp edge)

Tenants provisioned **before** this cutover are still running the previous workspace-server image. When they pull the new image on their next boot or auto-update cycle, their `CPProvisioner` will start expecting `MOLECULE_CP_SHARED_SECRET` in the container env — but the existing tenant EC2s don't have that variable in their user-data (the CP only started injecting it from PR #53 onward).

**Symptom**: a pre-cutover tenant can still serve its users' existing workspaces, but any attempt to **provision a new workspace** from inside the tenant UI will hit the CP's new bearer gate and get `401` or `404` back, surfacing as "workspace provision failed" with a generic error.

**Fix per existing tenant (pick one)**:

1. **SSH in + add the env var**
   - Copy `PROVISION_SHARED_SECRET` from Railway prod env.
   - `ssh ubuntu@<tenant-ip>` and append to the running container's env (`docker stop && docker run … -e MOLECULE_CP_SHARED_SECRET='…' -e MOLECULE_CP_URL=https://api.moleculesai.app …`). Rolling this into an auto-update hook is follow-up work.

2. **Re-provision the tenant**
   - `DELETE /cp/orgs/:slug` → re-create via normal signup flow. Tenant-level data survives only if the tenant's own Postgres volume is preserved; workspace_id values change. This is the heavy hammer — only for tenants where existing data can be recreated easily.

3. **Wait for the auto-update + user-data refresh cycle**
   - Tenant auto-updater (cron, 5-minute cadence) pulls the new container image but **does not refresh env vars** — those are frozen from the initial user-data. So option 3 alone doesn't fix this; it still needs option 1 or 2.

Script at `scripts/migrate-tenant-cp-secret.sh` (follow-up) will automate option 1 across all running tenants in the prod AWS account.

## Post-deploy verification checklist

- [ ] Railway prod deploy for `controlplane` lands on the new commit (check `https://railway.com/project/7ccc…/service/ae76…`)
- [ ] `curl https://api.moleculesai.app/health` → 200 `{service: molecule-cp, status: ok}`
- [ ] `curl -X POST https://api.moleculesai.app/cp/workspaces/provision` (no bearer) → 401 (**not** 404 — proves the env var is live and routes mounted)
- [ ] GHCR publishes new `workspace-server` image for the core main commit
- [ ] Vercel canvas prod deploy lands

## Rollback

If prod is on fire:

1. `gh pr revert 46 -R Molecule-AI/molecule-controlplane` — reverts all 6 CP PRs together.
2. `gh pr revert 983 -R Molecule-AI/enter-os-core` — reverts the core bundle.
3. Both reverts auto-deploy via Railway / GHCR / Vercel.

Existing tenants aren't affected by a rollback — they're running whichever tenant image tag they booted with. Only newly-provisioned tenants pick up the reverted control plane code.
