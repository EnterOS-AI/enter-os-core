# Workspace placement — org-per-EC2 architecture

Status: Accepted (implicit since 2026-05; formalized 2026-05-24)
Owners: hongming (CTO), cui (CEO)
Tracking: #1793

This RFC formalizes the architecture decision that has been implicit in the system since the post-suspension rebuild: **each EnterOS org is one isolated tenant on its own EC2 instance**, with every functional surface (workspace-server, memory plugin, Postgres, Redis, canvas) co-located on that instance. The platform's role is provisioning, billing, and the cross-tenant control plane — never the data path.

The implementation already follows this pattern in every direction we look (provisioner, memory v2 cutover, tenant entrypoint, controlplane user-data, even the OSS deploy story). Writing it down so it stays that way.

## TL;DR

```
                ┌──────────────────────────────────┐
                │  Platform (controlplane)         │
                │  Railway-hosted                  │
                │  api.moleculesai.app             │
                │                                  │
                │  - org provisioning              │
                │  - billing + Stripe integration  │
                │  - DNS + tunnel orchestration    │
                │  - auth / org-token issuance     │
                │  - fleet redeploy orchestration  │
                │                                  │
                │  NEVER holds tenant data         │
                └──────────────────────────────────┘
                         │                      │
              provision  │                      │  provision
              + billing  │                      │  + billing
                         ▼                      ▼
       ┌─────────────────────────┐    ┌─────────────────────────┐
       │  Tenant: agents-team    │    │  Tenant: <other-org>    │
       │  Own EC2 (us-east-2)    │    │  Own EC2 (us-east-2)    │
       │  agents-team.molecule.. │    │  <slug>.moleculesai.app │
       │                         │    │                         │
       │  ┌───────────────────┐  │    │  ┌───────────────────┐  │
       │  │ molecule-tenant   │  │    │  │ molecule-tenant   │  │
       │  │ (workspace-server │  │    │  │ (workspace-server │  │
       │  │  + canvas + go)   │  │    │  │  + canvas + go)   │  │
       │  └───────────────────┘  │    │  └───────────────────┘  │
       │  ┌───────────────────┐  │    │  ┌───────────────────┐  │
       │  │ memory-plugin     │  │    │  │ memory-plugin     │  │
       │  │ (loopback :9100)  │  │    │  │ (loopback :9100)  │  │
       │  └───────────────────┘  │    │  └───────────────────┘  │
       │  ┌───────────────────┐  │    │  ┌───────────────────┐  │
       │  │ postgres pgvector │  │    │  │ postgres pgvector │  │
       │  │ (172.17.0.1:5432) │  │    │  │ (172.17.0.1:5432) │  │
       │  └───────────────────┘  │    │  └───────────────────┘  │
       │  ┌───────────────────┐  │    │  ┌───────────────────┐  │
       │  │ redis             │  │    │  │ redis             │  │
       │  └───────────────────┘  │    │  └───────────────────┘  │
       │  ┌───────────────────┐  │    │  ┌───────────────────┐  │
       │  │ workspace runtime │  │    │  │ workspace runtime │  │
       │  │ containers (ws-*) │  │    │  │ containers (ws-*) │  │
       │  └───────────────────┘  │    │  └───────────────────┘  │
       └─────────────────────────┘    └─────────────────────────┘
```

Every tenant is a self-contained enter-os-core instance. The platform is a thin coordinator above them.

## What crosses the platform/tenant boundary

What the platform sends down to the tenant:

- Initial EC2 provisioning (user-data script via SSM) — see `molecule-controlplane/internal/provisioner/ec2.go`
- Per-tenant secrets (DB password, `SECRETS_ENCRYPTION_KEY`, `MOLECULE_CP_SHARED_SECRET`) injected as env at boot
- Image redeploys via `POST /cp/admin/tenants/:slug/redeploy` → SSM → `docker pull && docker stop && docker run`
- DNS records (Cloudflare) and tunnel registration (cloudflared)
- Billing-state changes (subscription status, plan upgrades)

What the tenant sends up to the platform:

- Boot-stage telemetry (`report_stage` calls during EC2 user-data execution)
- LLM usage events (for billing aggregation; documented in `controlplane/migrations/037_llm_usage_billing.up.sql`)
- Workspace lifecycle events for cross-tenant analytics — read-only, no remote control implied

What does NOT cross the boundary:

- Memory contents (HMA scopes, agent_memories before A3, memory_plugin records after)
- Workspace state, files, canvas layouts
- Workspace runtime container state
- Per-org user authentication state (tenant issues its own session tokens via `wsauth`)

If a feature design wants to put any of those on the platform side, that's a violation of this RFC and needs explicit justification.

## SSOT rationale

The single-source-of-truth boundary is **the tenant EC2**.

This decision was the implicit basis for the memory v1→v2 migration that ran 2026-05-24 (issues #1747 → #1791 → #1792). The v2 memory plugin runs as a sidecar on each tenant EC2, sharing the tenant's Postgres under a dedicated `memory_plugin` schema. There is no platform-side memory aggregation, no central index, no cross-tenant memory federation. Memory writes are loopback-only (workspace-server → memory-plugin on `127.0.0.1:9100`).

Why this is correct:

1. **Organizational isolation is the product.** A tenant's memory, workspaces, secrets, and conversation history must not be readable by another org, ever. The simplest enforcement is physical: different EC2, different DB, different network. Application-level multi-tenancy adds a class of cross-tenant data leak bugs that can't happen here.

2. **The platform must remain horizontally scalable independent of tenant data volume.** If memory aggregation lived on the platform, billing/provisioning/auth would scale with the volume of memory across all tenants. With per-tenant storage, the platform's scaling envelope depends only on the number of orgs.

3. **OSS-deployability requires it.** enter-os-core is open-source; anyone can deploy it. If functional state lived on a centralized platform, OSS deployers would either have to run their own platform (high barrier) or call ours (privacy concern + scale concern). Per-tenant SSOT means the OSS enter-os-core instance is functionally complete — it just talks to a platform for billing.

## OSS-deployment shape

A workspace inside any tenant reaches its parent tenant by injecting two env vars at container start:

- `MOLECULE_ORG_ID` — the UUID of the org this workspace belongs to
- `MOLECULE_PLATFORM_URL` — the tenant's HTTPS URL (e.g. `https://agents-team.moleculesai.app`)

These are baked into the workspace runtime's docker run by the workspace-server when it provisions a workspace. The workspace's agent runtime uses them to:

- Register itself in the tenant's `workspaces` table
- Send heartbeats (Redis TTL key on the tenant)
- Subscribe to A2A messages via the tenant's WebSocket hub
- Commit memories via the tenant's MCP bridge or HTTP `/memories` endpoints

An OSS deployer running their own enter-os-core instance gets the same shape: their workspaces inject the deployer's tenant URL and org ID. The agent runtime is **agnostic** to whether it's talking to our hosted platform or a self-hosted one.

The only thing tying a tenant to **our** platform is the billing/auth path:

- `MOLECULE_CP_URL` env on the tenant container points at `api.moleculesai.app`
- `MOLECULE_CP_SHARED_SECRET` env authenticates the tenant→platform direction
- LLM usage events POST to `cp_url/cp/llm-usage-events` for billing aggregation

An OSS deployer can leave `MOLECULE_CP_URL` unset (or point at their own platform). The workspace-server's `wiring.go` and `cp_provisioner.go` already handle the absent-CP case gracefully — the tenant is fully functional without it.

## Scaling envelope

Per-tenant resource shape (current):

| Layer | Sizing |
|---|---|
| EC2 | t3.medium (2 vCPU, 4 GiB) for default-tier orgs |
| Postgres | Single container, pgvector pre-installed, ~1-10 GiB per org expected |
| Memory plugin | Loopback only, ~50 MB resident, scales with memory record count |
| Workspace runtime containers (ws-\*) | One per workspace; sized by template tier |

The platform's scaling envelope:

| Layer | Sizing |
|---|---|
| controlplane | Single Railway service, scales horizontally |
| Postgres | One Railway-hosted Postgres for billing + org registry + auth tokens |
| DNS | Cloudflare zone with one CNAME per tenant |
| Tunnels | One Cloudflare tunnel per tenant |

Order-of-magnitude:

- 100 orgs: trivial (100 EC2s, controlplane unchanged)
- 10K orgs: needs an EC2 placement strategy (region pinning, dedicated-tier hosts), but the platform is still a single service
- 1M orgs: this design starts to strain — Cloudflare tunnel-per-tenant becomes expensive, EC2-per-tenant becomes resource-wasteful, and we'd want a denser tenant-on-shared-infra mode

The current architecture is sized for the 100–10K range. The 1M-org variant is explicitly out of scope for this RFC.

## Decision points for new feature design

When proposing a new feature, the design must answer "where does the data live?" Pick one:

1. **On the tenant.** Default choice for anything functional. Tenant DB, tenant memory plugin, tenant filesystem. The feature ships in `enter-os-core` and is deployed via the tenant image.

2. **On the platform.** ONLY for billing, cross-org analytics (anonymized), org registry, auth tokens, DNS/tunnel state. The feature ships in `molecule-controlplane`.

3. **Both, with one as SSOT.** Rare. The tenant is the SSOT; the platform may cache for cross-tenant queries but must be willing to re-read from the tenant on miss. Document the cache invalidation contract.

When in doubt, default to #1. If you find yourself wanting to put HMA memory, workspace state, or session history on the platform, stop — you're re-introducing the SSOT violation the v1→v2 memory migration was designed to remove.

## Migration path for non-conforming code

The implementation already conforms. There is no migration backlog as of 2026-05-24:

- Memory: v1→v2 migration complete (#1747 → #1791 → #1792). v2 plugin per-tenant is SSOT.
- Workspace state: always per-tenant (the `workspaces` table lives in the tenant Postgres).
- Activity logs: per-tenant `activity_logs` table.
- Files: per-tenant (Docker volumes attached to ws-\* containers).
- Secrets: per-tenant (`workspace_secrets` + `global_secrets` tables in tenant DB).
- LLM usage events: tenant emits, platform aggregates for billing — correct shape.

If a future PR proposes platform-side aggregation of something functional, link this RFC in the review.

## What this RFC does NOT cover

Out of scope for this document; tracked separately if needed:

- **Multi-region tenant placement** — current design is single-region (us-east-2). Multi-region needs its own RFC because it changes the EC2 placement contract.
- **BYO-compute / customer-managed VPC** — adjacent design; the org-per-EC2 boundary holds but the EC2 ownership shifts to the customer.
- **Workspace runtime selection** — separately documented in `docs/architecture/workspace-tiers.md`.
- **Tenant image upgrade strategy** — separately documented in `docs/architecture/tenant-image-upgrades.md`.
- **OSS billing alternatives** — how OSS deployers handle billing without our controlplane is a separate go-to-market decision.

## References

- `docs/architecture/memory.md` — HMA scopes + v2 plugin
- `docs/architecture/saas-prod-migration-2026-04-19.md` — provisioning pipeline reference
- `docs/architecture/molecule-technical-doc.md` §3 (System Architecture) — top-level picture
- `molecule-controlplane/internal/provisioner/ec2.go` — the canonical user-data + docker run for tenants
- `workspace-server/entrypoint-tenant.sh` — the canonical tenant boot script
- Memory system migration: #1747 (kill v1 fallback), #1791 (Phase A2 backfill), #1792 (Phase A3 drop table)
