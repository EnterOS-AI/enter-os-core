# RFC: Org-level Platform Agent — a tenant-resident concierge

**Perspective:** CTO + Backend Engineer + DevOps
**Status:** Draft — pre-implementation, **CTO sign-off required before any implementation PR**
**Scope:** `enter-os-core` (workspace-server), `molecule-controlplane`, workspace runtime, `molecule-app`
**This document is the single source of truth (SSOT) for the feature.** Code, OpenAPI, the platform
MCP, and end-user docs reconcile to this RFC — not to each other.

---

## 1. Summary

Today a EnterOS tenant is a control/router box: one EC2 runs the `workspace-server`
(`molecule-tenant` container) + Postgres + Redis, and **each workspace is its own separate EC2**
running a runtime image that joins the tenant's A2A mesh. A2A has exactly two participant kinds:
**workspaces** (agents) and the **user** (the canvas, modeled implicitly as `activity_logs.source_id
IS NULL`). A user who wants to *do* anything must drive individual workspaces directly — create them,
assign agents, wire channels/schedules/secrets — i.e. they must carry a lot of platform knowledge.

This RFC introduces a **platform agent**: an always-on org-level agent that

1. runs as a **container on the tenant EC2** itself (beside `molecule-tenant`),
2. natively holds the **platform-management MCP** (the org-admin tool surface) so it can do anything
   in the org,
3. joins A2A as a **first-class third participant** (`kind='platform'`) that sits at the org root, and
4. becomes the **user's default chat target** — a concierge the user talks to like a chatbot, which
   then orchestrates the org on their behalf.

Destructive actions the concierge triggers are **human-approved** through the existing approvals
subsystem.

## 2. Motivation

- **Lower the knowledge floor.** "Spin up an SEO team and have them publish weekly" should be a
  sentence, not a sequence of workspace/agent/schedule/secret operations.
- **One front door.** A single conversational entry point that *is* the org, instead of N per-workspace
  chats the user has to coordinate.
- **Reuse, don't rebuild.** The agent runtime, A2A mesh, the 87-tool platform MCP, and the approvals
  subsystem already exist. This feature is mostly *composition* plus one honest new participant kind.

## 3. Goals / Non-Goals

**Goals**
- A per-tenant platform agent, provisioned automatically, that controls the org via the platform MCP.
- A first-class `platform` participant in A2A with correct routing and tenant isolation.
- Server-side approval gating for destructive org operations.
- Parity with normal workspaces for runtime/model/provider/billing (no special-casing).

**Non-Goals (this RFC)**
- Replacing the canvas. The canvas remains the advanced/power-user surface.
- Multi-concierge / per-team concierges. Exactly **one** platform agent per org.
- A new scoped-down token system for the MCP (tracked separately; see §10 Open Questions).

## 4. Current-state ground truth (verified, with references)

- **Topology.** Tenant EC2 runs `molecule-tenant` (workspace-server) + Postgres + Redis;
  `controlplane/internal/provisioner/ec2.go:buildTenantUserDataSM()` `docker run`s it with
  `--network host`, `PORT=8080`. Each **workspace is its own EC2** (`ec2.go:ProvisionWorkspace`).
- **No `org_id` column.** An "org" is the `parent_id IS NULL` subtree root;
  `workspace-server/internal/handlers/org_scope.go` resolves it with a recursive CTE (`orgRootID`) and
  `sameOrg()` compares two workspaces' resolved roots for tenant isolation (#1953/OFFSEC-015).
- **A2A authorization is hierarchy-based.** `workspace-server/internal/registry/access.go:CanCommunicate`
  permits self / siblings / ancestor↔descendant. Root-level rows are "siblings" but every routing path
  is additionally gated by `sameOrg()`.
- **No participant-kind discriminator.** `workspaces.role` is a free-form string; the user is implicit
  (`activity_logs.source_id IS NULL`). `migrations/001_workspaces.sql`.
- **Runtime injects MCP servers** in the claude-code executor's `mcp_servers` dict — today exactly one
  entry, `"a2a"` (`molecule-ai-workspace-template-claude-code/claude_sdk_executor.py`,
  `molecule_runtime/claude_sdk_executor.py`). The agent self-registers via `POST /registry/register`
  (`molecule_runtime/main.py`) and is identified by `WORKSPACE_ID` + `X-Molecule-Org-Id`.
- **Platform MCP** (`molecule-mcp-server`, stdio Node) authenticates purely from env
  (`MOLECULE_API_KEY` = org-admin token, `MOLECULE_API_URL`, `MOLECULE_ORG_ID`; `src/api.ts`), is a
  thin proxy over the tenant REST/A2A API (`chat_with_agent` → `POST /workspaces/:id/a2a`,
  `async_delegate` → `/delegate`), and has **zero embeddability blockers**.
- **Billing** is a per-workspace resolver — `ResolveLLMBillingModeDerived`
  (`workspace-server/internal/handlers/workspace_provision.go`, `llm_billing_mode.go`), defaulting
  closed to `platform_managed`; `byok` runs on the tenant's own provider key (see
  `docs/architecture/byok-fail-closed-billing.md`).
- **Approvals** exist: `migrations/007_approvals.sql`, `internal/handlers/approvals.go`,
  `EventApprovalRequested`, decide route `POST /workspaces/:id/approvals/:approvalId/decide`.

## 5. Design

### 5.1 The platform agent IS the org root

Because `sameOrg()` resolves each workspace to its topmost `parent_id IS NULL` root, a platform agent
added as a *second* root would resolve to a *different* root than the existing team and be **blocked**
by `sameOrg`. Therefore the platform agent **becomes the single org root**, and the org's existing
root is **re-parented under it**. Consequences:

- `orgRootID(any workspace) == platform-agent-id`; `sameOrg(platform, any in-org ws) == true`.
- The platform agent reaches every workspace (and is reachable) via the **existing**
  ancestor↔descendant rules — **no `CanCommunicate` change**, and tenant isolation is unchanged.

This is the honest realization of "a third participant above workspace and user": the concierge is
literally the org.

### 5.2 `kind` discriminator (the only new marker)

Add a single column `workspaces.kind TEXT NOT NULL DEFAULT 'workspace'`, constrained to
`('workspace','platform')`. It is the **only** marker of the platform agent — we do **not** also
encode identity in `role`/`tier` (those stay descriptive). The enum is defined once: the migration
`CHECK` and the Go constants `KindWorkspace`/`KindPlatform` (+ one `IsValidKind`) are kept in lockstep.

Invariants (handler-enforced, since there is no `org_id` for a pure-SQL unique):
- `kind='platform' ⇒ parent_id IS NULL`.
- A row may be `kind='platform'` only if it is its own org root (`orgRootID(self) == self`), giving
  "exactly one platform agent per org". Guard the check+write in a tx with `FOR UPDATE` on the root.

### 5.3 Identity & registration

- **ID** = derived `uuidv5(org-namespace, "platform-agent")` — reproducible, no stored-vs-derived
  drift, lowercase so it satisfies the runtime's `WORKSPACE_ID` validator.
- CP **pre-seeds** the `workspaces` row (`kind='platform'`, `parent_id=NULL`, `tier=0`) before the
  agent boots; the agent self-registers (`POST /registry/register`) into that row. `Register` accepts
  an optional `kind` and reconciles it, enforcing the §5.2 invariants.

### 5.4 Default-target resolver

New `GET /registry/platform-agent` (handler `internal/handlers/platform_agent.go`): resolve the
caller's `orgRootID()` and return it iff `kind='platform'`. This is the server hook the dashboard
targets by default; no change to `ProxyA2A`. **Authored in the OpenAPI SSOT first**; MCP/CLI/docs
derive from it.

### 5.5 Runtime: two MCPs, config-driven

Make the runtime's `mcp_servers` **config-driven** rather than hardcoded:
- `molecule_runtime/config.py`: add `extra_mcp_servers: list[dict]` to `WorkspaceConfig`, read
  `raw.get("mcp_servers", [])`.
- Both executors merge `extra_mcp_servers` into the `mcp_servers` dict after the always-on `"a2a"`
  entry (the template `claude_sdk_executor.py` is the live one; the runtime-package copy is the
  fallback).

The platform agent's `config.yaml` then declares:

```yaml
runtime: claude-code
model: sonnet            # default; user-switchable model AND provider via providers.yaml
a2a:
  port: 8090             # avoid the workspace default 8000 under host networking
mcp_servers:
  - name: platform
    command: node
    args: ["/opt/molecule-mcp-server/dist/index.js"]
```

The `platform` MCP reads `MOLECULE_API_KEY`/`MOLECULE_API_URL`/`MOLECULE_ORG_ID` from the container
env (passed through to the stdio child) — no per-server `env` block needed.

### 5.6 Hosting & provisioning (tenant EC2 container)

In `ec2.go:buildTenantUserDataSM()` add a `start_platform_agent` stage **after** `wait_platform_health`
(the agent registers against `localhost:8080` on boot):

```bash
docker run -d --restart=always --name molecule-platform-agent --network host \
  -v /data/platform-agent/configs:/configs \
  -e WORKSPACE_ID=<platform-uuid> -e WORKSPACE_CONFIG_PATH=/configs \
  -e PLATFORM_URL=http://localhost:8080 \
  -e MOLECULE_API_URL=http://localhost:8080 -e MOLECULE_API_KEY=$ADMIN_TOKEN -e MOLECULE_ORG_ID=<orgID> \
  -e ANTHROPIC_AUTH_TOKEN=$ADMIN_TOKEN -e MOLECULE_LLM_ANTHROPIC_BASE_URL=$MOLECULE_LLM_ANTHROPIC_BASE_URL \
  <platform-agent-image>
```

- The org `admin_token` is already on the box (Secrets Manager `molecule/tenant/{orgID}`).
- `--restart=always` provides Docker-level supervision (matches `molecule-tenant`).
- Mirror the block into the redeploy path (`buildRedeployScript`) so existing tenants backfill it.

### 5.7 Image

A **dedicated `molecule-platform-agent` image**: `FROM workspace-template-claude-code`, `COPY` the
prebuilt `molecule-mcp-server/dist` + `node_modules` into `/opt/molecule-mcp-server`, and **pin Node
20** (the slim base ships Node 18; the MCP expects ≥20). A dedicated image keeps the org-admin MCP
**out of** ordinary workspace images (security hygiene) and lets us set concierge defaults without
touching the workspace template. `molecule-ci` publishes it.

### 5.8 Approval gate (server-side trust boundary)

The MCP is a *client* of the tenant handlers, so enforcement lives in the **handlers**, not the MCP.

- `internal/approvals/policy.go` (new): one auditable map of gated actions —
  `delete_workspace`, `deprovision`, `secret_write`, `org_token_mint`.
- `requireApproval(ctx, workspaceID, action, contextHash)` reuses the existing approvals
  INSERT/broadcast/escalate. If an `approved`+unconsumed row matches → consume it → proceed. Else
  create a `pending` row, broadcast `EventApprovalRequested`, and return **HTTP 202
  `{approval_id, status:"pending"}`** instead of executing. The human decides via the existing decide
  route; the agent retries and the gate now passes.
- Add `approval_requests.consumed_at` (single-use) and optional `request_hash` (dedupe identical
  pending requests).
- **Escalation:** the platform agent's `parent_id` is NULL, so platform-originated approvals escalate
  to the **user** (canvas notify), not a parent.
- The 202 response shape is authored in the **OpenAPI SSOT**.

### 5.9 Billing & model/provider parity

The platform agent is a `workspaces` row, so it inherits the one billing resolver and the
`providers.yaml` runtime matrix unchanged:
- **Default `platform_managed`** (metered CP proxy, billed to org credits) — the env wiring in §5.6.
- **`byok`** = flip `/admin/workspaces/:id/llm-billing-mode` + supply the org's `ANTHROPIC_API_KEY`
  secret (workspace or global). Exposed as a provisioning flag so a tenant can choose at create time.
- Model **and provider** are switchable (Claude, Kimi-for-coding, …) via the same dashboard
  model-switcher any workspace uses.

### 5.10 UX (summary; detailed in app RFC / Phase 5)

The **dashboard** (`molecule-app`) becomes the primary entry: a concierge chat (default-targeting the
§5.4 resolver) plus a live org overview, with pending approvals surfaced inline. The **canvas** stays
for advanced users. First UI version is produced in Claude Design and iterated before build.

## 6. SSOT mapping (derive, don't fork)

| Concern | Single source of truth | This RFC's rule |
|---|---|---|
| "The org" | `orgRootID()`/`sameOrg()` (`org_scope.go`) | platform agent *becomes* the root; no `org_id` column |
| Platform marker | `workspaces.kind` | `kind` only; never also `role`/`tier` |
| Model/provider | `providers.yaml` runtime matrix | concierge switches via the same registry |
| LLM billing | `ResolveLLMBillingModeDerived` | inherits the one resolver; no new path |
| Config/secrets delivery | tenant Secrets Manager bundle (`seedWorkspaceConfigSecret`) | no new S3 prefix / second store |
| Management API | OpenAPI spec | new endpoints authored there first; MCP/CLI/docs derive |
| Gated actions | `internal/approvals/policy.go` | one map |
| Platform-agent id | `uuidv5(org, "platform-agent")` | derived, never stored separately |

## 7. Security & blast radius

The concierge holds the org **admin token** (full tenant-root, self-minting) and is driven by
end-user chat. Mitigations:
- **Approval gate (§5.8)** must ship *with* the agent going user-facing, not after. Until then the
  agent is operator-only.
- **Tenant isolation** is unchanged — every reach path still passes `sameOrg()`.
- **MCP not in workspace images** (dedicated image, §5.7); the admin token lives only in the
  platform-agent container env on the tenant box.
- **Token rotation:** the MCP reads env once at spawn → rotation = `docker restart
  molecule-platform-agent` (runbook item).
- Future: a scoped-down org token (no delete/billing/member) — see §10.

## 8. Migration & rollout

Phase ordering is the rollout contract:
- **Phase 0** (schema) ships and bakes before anything writes `kind`. Backward-compatible: every
  existing row defaults to `kind='workspace'`; the `CHECK` is added `NOT VALID` then validated.
- **Phase 1 re-parenting backfill** is the one real watch-item. **Before** running it, audit whether
  any org-scoped table keys off the *root workspace id* (e.g. `org_api_tokens`, `org_plugin_allowlist`)
  versus the CP org UUID. If they reference the root workspace id, re-parenting changes "the root" and
  those refs must migrate too. The backfill is per-org, idempotent, and reversible.
- New orgs get the platform agent from first boot; existing orgs backfill via `/admin/tenants
  redeploy` + a one-time re-parent migration.

## 9. Implementation phases

0. **Schema + model** (`enter-os-core`): `kind` column + `approval_requests.consumed_at`; model field +
   constants; `Register` accepts/validates `kind` with invariants.
1. **Platform-as-root + resolver** (`enter-os-core` + CP): CP pre-seeds the platform row and creates
   teams under it; per-org re-parent backfill (after the §8 audit); `GET /registry/platform-agent`.
2. **Config-driven two-MCP runtime** (runtime + claude-code template).
3. **Image + tenant provisioning** (CP + image + `molecule-ci`): dedicated image; `start_platform_agent`
   in user-data + redeploy; config via the tenant Secrets Manager bundle; billing knob.
4. **Approval gate** (`enter-os-core`): policy map + `requireApproval` at destructive handlers; OpenAPI
   202 shape.
5. **Dashboard concierge UX** (`molecule-app`): design-first, then build against the resolver.
6. **Cleanup**: exclude the platform agent from billable counts; canvas visibility; rotation runbook.

## 10. Open questions

- **Scoped-down token.** Should the concierge hold a reduced-scope token (no delete/billing/member)
  instead of full admin + an approval gate? The token-scope system does not exist yet (`orgtoken`
  TODO). Recommendation: ship admin-token + approval gate now; add scope-down as a follow-up.
- **Re-parenting vs. wrapper.** If product later wants a platform agent that is *not* the topological
  root, a `CanCommunicateWithKind` wrapper (guarded by `sameOrg`) is the alternative. Deferred —
  platform-as-root is lower-risk and needs zero access-control change.
- **Canvas visibility** of the root concierge node (hide vs. show as the org anchor).

## 11. Verification (end-to-end on a staging tenant)

1. **Schema:** Phase-0 migrations applied; existing workspaces report `kind='workspace'`; `go test
   ./...` + `-tags=integration` green.
2. **Provision:** redeploy a staging tenant; `docker ps` shows `molecule-platform-agent` healthy; its
   logs show a successful `/registry/register`.
3. **Identity:** the platform row is `kind='platform'`, `parent_id IS NULL`; the former root now has
   `parent_id = <platform id>`; `GET /registry/platform-agent` returns it.
4. **Reach:** chat the platform agent → it `list_workspaces` then `create_workspace` via the platform
   MCP and reports back via `send_message_to_user`.
5. **Isolation:** it reaches every workspace in its org and **cannot** reach another tenant's
   workspace.
6. **Approval gate:** `delete_workspace` → HTTP 202 pending + approval event; decide-approve →
   completes; a second delete with the same approval is rejected (consumed).
7. Drive a real concierge flow ("spin up a PM + engineer to build X") and watch the delegation/activity
   ledger.

---

*Derived from a read-only multi-agent source audit of `enter-os-core`, `molecule-controlplane`,
`molecule-ai-workspace-runtime`, `molecule-ai-workspace-template-claude-code`, and
`molecule-mcp-server`. No secret values recorded.*
