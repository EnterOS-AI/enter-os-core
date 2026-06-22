# EnterOS Platform OpenAPI specs

This directory holds the machine-readable API contracts for the EnterOS
platform.

| File | Spec | Scope | Status |
|------|------|-------|--------|
| `management.yaml` | OpenAPI **3.1** | The **management surface** across both services (orgs, billing, admin, provisioning, workspaces, secrets, templates, org-tokens, bundles). | **SSOT** — hand-authored. |
| `swagger.yaml` / `swagger.json` | OpenAPI 2.0 | swaggo-generated stub, `/schedules` only (the per-workspace **runtime** surface). | Legacy stub; superseded for management by `management.yaml`. |

`management.yaml` is the **single source of truth** the management tooling
derives from — the management MCP server, the management CLI (`molecule-cli`),
and the human-facing API docs (RFC #1706, the gap closed by
`PLATFORM-MANAGEMENT-API.md` §5c). Do not hand-edit those clients' route maps;
change them here and regenerate/derive.

## The two-service split

One structural fact drives the whole spec: there are **two services with two
auth stacks**, and the management surface spans both.

```
                         ┌─────────────────────────────────────────┐
   browser / CLI / MCP   │  Control plane (CP)                      │
        │                │  molecule-controlplane @ api.moleculesai │
        │  session       │  /api/v1/* (stable) [+ /cp/* sunset]      │
        ├───────────────▶│  orgs · members · billing · provisioning │
        │  admin bearer  │  · fleet/admin ops · pins                 │
        │  provision sec │                                          │
        └────────────────┴──────────────┬───────────────────────────┘
                                         │ edge reverse-proxy
                                         │ (subdomain / X-Molecule-Org-Slug)
                                         ▼
                         ┌─────────────────────────────────────────┐
   Org API Key / ws tok  │  Tenant workspace-server                 │
        │                │  enter-os-core/workspace-server          │
        └───────────────▶│  ONE EC2 per org @ <slug>.moleculesai.app│
                         │  workspaces · secrets · templates ·      │
                         │  org-tokens · bundles                    │
                         └─────────────────────────────────────────┘
```

- **Control plane (CP)** — `api.moleculesai.app`, routes modelled under
  `/api/v1/*` (the `/cp/*` mirror is identical but sunset-headed per RFC #61 and
  is not duplicated in the spec). Owns **orgs, members, billing, provisioning,
  fleet/admin ops**.
- **Tenant workspace-server** — one EC2 per org at `<slug>.moleculesai.app`.
  Owns **workspaces, agents, secrets, templates, org-tokens, bundles**. Requests
  may also be sent to the CP host with an `X-Molecule-Org-Slug` header; the CP
  edge reverse-proxies them to the tenant host (the `Authorization`,
  `X-Molecule-Org-*`, and cookie headers pass through unchanged and the tenant's
  own middleware validates them).

The key consequence, called out in `PLATFORM-MANAGEMENT-API.md`: **the Org API
Key is a TENANT credential, not a CP one.** It is full tenant-admin over its own
org's workspace-server surface and reaches **nothing** on the CP (org
create/delete, billing, members, provisioning all 401/403 it). That is why
member/billing tools belong in a separate CP-admin MCP, not the org-key-authed
management MCP.

## Security scheme → surface map (the tier matrix)

`management.yaml` defines these `securitySchemes`; each operation declares the
one(s) it accepts. Mirror of `PLATFORM-MANAGEMENT-API.md` §1:

| Scheme | What it is | Where it applies |
|--------|-----------|------------------|
| `workosSession` | WorkOS AuthKit session cookie `mcp_session` (+ org membership/ownership checks) | CP `/api/v1/orgs/*`, `/api/v1/billing/*`. Also accepted on the tenant surface via the CP-session path. |
| `cpAdminBearer` | CP `CP_ADMIN_API_TOKEN` operator bearer (AdminGate, constant-time) | CP `/api/v1/admin/*` — admin-create-org, tenant teardown, workspace env, ListOrgWorkspaces, redeploy, pins. |
| `provisionSecret` | CP `PROVISION_SHARED_SECRET` bearer | CP `/api/v1/workspaces/provision`, `…/status`. Routes unmounted when the secret is unset. |
| `tenantAdminToken` | Per-tenant admin_token (+ `X-Molecule-Org-Id`) | CP `DELETE /api/v1/workspaces/:id` (deprovision) — **in addition to** `provisionSecret` (issue #118). |
| `orgApiKey` | Tenant Org API Key — `Authorization: Bearer <key>` + routing header; full tenant-admin, self-minting | **All** tenant routes: `/workspaces[/:id]`, `/workspaces/:id/secrets`, budget, billing-mode, `/settings/secrets`, `/org/import`, `/org/templates`, `/org/tokens`, `/templates`, `/bundles`. |
| `workspaceToken` | Per-workspace bearer, bound to one workspace id (+ routing header) | Read/lifecycle/secrets on a single `/workspaces/:id/*`. **Rejected** on admin list/create/delete when ADMIN_TOKEN is set — use `orgApiKey`. |
| `orgRoutingHeaderId` / `orgRoutingHeaderSlug` | `X-Molecule-Org-Id` / `X-Molecule-Org-Slug` | Required on every tenant-host request so the edge / TenantGuard route + authorize against the correct org. Send one of them alongside the bearer. |

### Guards worth knowing (modelled per-operation)

- **Dry-run:** `POST /api/v1/admin/orgs?dry_run=true` — validate + echo, no org
  created. (The only dry-run on the whole management API.)
- **Confirm token:** `DELETE /api/v1/admin/tenants/:slug` and
  `…/scrub-artifacts` — body `confirm` MUST equal the URL slug, else `400`
  before any teardown.
- **Force flag:** `POST /api/v1/admin/workspaces/:id/env` — keys matching the
  secret-keyword guard (`TOKEN`/`SECRET`/`KEY`/`PASSWORD`) require `force=true`.
- **Runtime-pin gate:** `POST /api/v1/workspaces/provision` returns `422
  RUNTIME_PIN_MISSING` when no runtime image pin exists.
- **Auto-restart side-effects:** writing a workspace or global secret
  auto-restarts the affected workspace(s).

## Security note (carried from the synthesis spec)

The Org API Key is **full tenant-admin and self-minting** — a management MCP
holding one holds tenant root. There is no scope-down today (TODO in
`orgtoken`). Per-role / per-workspace scoping should ship alongside the
management MCP.

## Validate

```bash
cd workspace-server/docs/openapi
npx @redocly/cli lint management.yaml   # must be clean (0 errors, 0 warnings)
```

## Scope notes / best-effort flags

- The per-workspace **runtime** surface (schedules, agent, registry, a2a,
  memory, approvals, channels, terminal, files) is intentionally **out of
  scope** here — that's the runtime contract, not management.
- A handful of bodies are **best-effort** from the handlers (org-import inline
  template, bundle import, list responses with open shapes) and are marked with
  `additionalProperties: true` in the schema. Tighten as the handler structs
  stabilise.
- `/cp/*` deprecated mirrors are omitted (identical shapes; RFC #61
  Deprecation/Sunset). Build against `/api/v1/*`.
