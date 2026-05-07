# Staging SaaS E2E — runbook

Four workflows + a shared bash harness that together cover the SaaS stack end to end against live staging. Every workflow provisions a fresh org per run and tears it down; leaks are CI failures.

## Coverage

| Workflow | Cadence | Wall time | Scope |
|---|---|---|---|
| `e2e-staging-saas.yml` | push + nightly 07:00 UTC | ~20 min | Full API: org → tenant → 2 workspaces → A2A → HMA → delegation → leak check |
| `canary-staging.yml` | every 30 min | ~8 min | Minimum smoke + self-managed alert issue |
| `e2e-staging-canvas.yml` | push + weekly Sunday 08:00 | ~25 min | All 13 canvas workspace-panel tabs via Playwright |
| `e2e-staging-sanity.yml` | weekly Monday 06:00 | ~10 min | Intentional-failure: teardown safety-net self-check |

`tests/e2e/test_staging_full_saas.sh` is the shared harness all workflows invoke (with `E2E_MODE={full|canary}` and `E2E_INTENTIONAL_FAILURE={0|1}` toggles).

### Full-SaaS checklist (sections)

| # | What |
|---|---|
| 0 | CP preflight |
| 1 | `POST /cp/admin/orgs` — org create without WorkOS session |
| 2 | Wait for tenant status = running |
| 3 | `GET /cp/admin/orgs/:slug/admin-token` — fetch per-tenant bearer |
| 4 | Tenant TLS readiness on `/health` |
| 5 | Provision parent workspace |
| 6 | Provision child workspace (full mode) |
| 7 | Wait both online |
| 8 | A2A round-trip on parent — expect agent response |
| 9 | HMA memory write + read, peers smoke, activity log (full mode) |
| 10 | Delegation mechanics: parent → child via proxy + activity assertion (full mode) |
| 11 | EXIT trap — teardown + leak detection |

### Canvas tabs

Opens all 13 workspace-panel tabs against the freshly-provisioned org:

```
chat, activity, details, skills, terminal, config, schedule,
channels, files, memory, traces, events, audit
```

Per tab: visible, panel renders, no "Failed to load" toast, screenshot captured. Known SaaS-mode gaps (Files empty, Terminal disconnect, Peers 401) are whitelisted — see issue #1369.

### Sanity self-check

Runs the harness with `E2E_INTENTIONAL_FAILURE=1`, which poisons the tenant admin token after the org is provisioned. The workspace-provision step then fails and the script exits non-zero; the EXIT trap + teardown + leak assertion must still run clean. If they don't, the sanity workflow files a `priority-high` issue with label `e2e-safety-net`.

## Required secret (exactly one)

Set in **Settings → Secrets and variables → Actions → Repository secrets**:

### `MOLECULE_STAGING_ADMIN_TOKEN`

The `CP_ADMIN_API_TOKEN` env currently set on the Railway staging molecule-platform → controlplane service.

```
railway variables --environment staging --service controlplane --kv | grep CP_ADMIN_API_TOKEN
```

This **one** secret drives everything:

- `POST /cp/admin/orgs` — provision org (no WorkOS session needed)
- `GET /cp/admin/orgs/:slug/admin-token` — fetch per-tenant bearer
- `DELETE /cp/admin/tenants/:slug` — teardown
- `GET /cp/admin/orgs` — leak detection post-teardown

The per-tenant admin token (short-lived, per-org) drives every tenant-side call (`POST /workspaces`, `/memories`, `/a2a`, etc.).

**No WorkOS session cookie needed** — admin endpoints bypass session auth via `AdminGate` (bearer + rate-limit only). CI provision + teardown collapse to one credential.

## Running locally

```
export MOLECULE_ADMIN_TOKEN="…"
# Optional: keep the org for post-mortem inspection
export E2E_KEEP_ORG=1
bash tests/e2e/test_staging_full_saas.sh
```

`E2E_KEEP_ORG=1` skips teardown so you can poke at the provisioned tenant yourself. **Never set this in CI** — staging will fill with orphans.

## Cost

- Full run: ~20 min, ~$0.007
- Canary (48/day): ~$0.06/day
- Canvas (few/week): ~$0.01/day
- Sanity (weekly): ~$0.002/week
- **Total staging burn: < $0.15/day** at expected CI load

Hard per-workflow timeouts (15–40 min) cap runaway cost. Three teardown layers:

1. Bash `trap cleanup_org EXIT INT TERM` in the harness
2. Playwright `globalTeardown` for the canvas workflow
3. `if: always()` step in every workflow that greps today's `e2e-*` orgs and force-deletes them

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Happy path |
| 1 | Generic failure (agent didn't respond, provisioning hung, etc.) |
| 2 | Missing required env |
| 3 | Provisioning timed out |
| 4 | Teardown left orphan resources (**leak detected — sanity workflow catches this**) |

## Known gaps (tracked elsewhere)

- [#1369](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1369): SaaS canvas Files / Terminal / Peers tabs — architecturally broken; whitelisted in the spec
- LLM-driven delegation (autonomous `delegate_task` tool use) — probabilistic, not in v1; proxy mechanics covered
