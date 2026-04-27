# Multi-Tenant Agent Platform: SaaS Federation with Molecule AI

This tutorial walks through setting up a multi-tenant AI agent platform using Molecule AI's SaaS federation layer. You'll provision workspaces for multiple customers from a single control plane, with per-tenant database isolation, credential separation, and agent fleet visualization.

**What this covers:**

- How the control plane provisions tenant workspaces in your AWS account
- How to onboard a new tenant with isolated Neon database + EC2 security group
- How to register and inspect a tenant's agent fleet via the platform API
- How billing and quota controls work at the tenant layer

**Assumptions:** You have a Molecule AI control plane deployed, an AWS account with VPC + subnets available, and a Neon account for branch-per-tenant databases.

---

## What is SaaS federation?

Molecule AI's SaaS federation layer sits between your control plane and the tenant workspaces your customers use.

```
You (the platform operator)
  │
  ├── Control Plane (api.moleculesai.app)
  │     └─ Provisions: Neon DB branches, EC2 workspaces, security groups
  │
  └── Tenant: acme.rocket.chat
        ├── Workspace: acme-production-1 (EC2, T3)
        ├── Workspace: acme-production-2 (EC2, T4)
        └── Neon branch: acme_db → acme's Postgres
```

Each tenant is a separate organization in Molecule AI. The control plane holds credentials and provisions infrastructure — but each tenant's workspace data lives in their own isolated branch.

---

## Step 1: Onboard a new tenant

Onboarding creates a new org in your platform, provisions a Neon database branch, and sets up an EC2 security group for the tenant's workspaces.

### Via the control plane API

```bash
# Create a new tenant org
curl -X POST https://api.moleculesai.app/cp/orgs \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Acme Corp",
    "slug": "acme",
    "plan": "pro",
    "vpc_id": "vpc-0a1b2c3d4e5f6g7h8",
    "subnet_ids": ["subnet-abc123", "subnet-def456"]
  }'
```

Response:

```json
{
  "id": "org_7f2a9c",
  "name": "Acme Corp",
  "slug": "acme",
  "plan": "pro",
  "neon_branch_id": "br-shadowy-7f2a9c",
  "security_group_id": "sg-0a1b2c3d",
  "status": "provisioning"
}
```

### What gets provisioned

| Resource | How | Who manages |
|---|---|---|
| Neon branch `br-shadowy-7f2a9c` | Auto-created by control plane via Neon API | Tenant gets connection string |
| EC2 security group `sg-0a1b2c3d` | Created with inbound :443 from platform only | Control plane manages rules |
| Org record in platform DB | Created on first API call | Control plane |

The provisioning step runs asynchronously — poll `/cp/orgs/:slug` until `status: active`.

```bash
# Poll until active
until curl -s https://api.moleculesai.app/cp/orgs/acme \
    -H "Authorization: Bearer $PROVISION_SHARED_SECRET" \
    | jq -r '.status' | grep -q active; do
  echo "Still provisioning..."; sleep 10
done
echo "Tenant ready"
```

---

## Step 2: Provision workspaces for the tenant

Once the tenant org is active, workspaces can be created via the tenant's own API — no operator involvement needed.

Each workspace is provisioned as an EC2 instance in the tenant's VPC subnet, behind the tenant's security group. The security group allows inbound :443 from the platform API only.

```bash
# As the tenant (they use their own org-scoped API key)
curl -X POST https://acme.moleculesai.app/workspaces \
  -H "Authorization: Bearer $TENANT_ORG_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production-agent-1",
    "role": "Production inference worker",
    "runtime": "hermes",
    "tier": 3,
    "model": "claude-sonnet-4"
  }'
```

The control plane handles the EC2 provisioning in the background:

1. Calls `aws ec2 run-instances` in the tenant's VPC subnet
2. Waits for the instance to boot and register via A2A
3. Returns the workspace ID and connection details

The tenant sees a workspace appear in their canvas UI within ~60 seconds.

---

## Step 3: Inspect the tenant's agent fleet

From the operator side, you can inspect any tenant's workspaces via the control plane:

```bash
# List all workspaces for a tenant
curl https://api.moleculesai.app/cp/orgs/acme/workspaces \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET" \
  | jq '.'
```

Response:

```json
{
  "org": "acme",
  "workspaces": [
    {
      "id": "ws_9b3k1m",
      "name": "production-agent-1",
      "runtime": "hermes",
      "tier": 3,
      "instance_id": "i-0a1b2c3d4e5f6g7h8",
      "status": "running",
      "last_seen": "2026-04-22T09:30:00Z"
    },
    {
      "id": "ws_2n8p4q",
      "name": "staging-worker",
      "runtime": "hermes",
      "tier": 2,
      "instance_id": "i-1a2b3c4d5e6f7g8h9",
      "status": "stopped",
      "last_seen": "2026-04-21T16:00:00Z"
    }
  ]
}
```

### Fleet-level metrics

```bash
# Aggregate runtime stats for a tenant
curl https://api.moleculesai.app/cp/orgs/acme/metrics \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET" \
  | jq '{total_workspaces, active_agents, avg_response_time_ms, total_tasks_dispatched}'
```

---

## Step 4: Set quota and billing controls

Quotas are enforced at the org level. Set a workspace count limit to prevent runaway provisioning:

```bash
# Set workspace limit for tenant
curl -X PATCH https://api.moleculesai.app/cp/orgs/acme \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "max_workspaces": 10,
    "max_tier": 3,
    "billing_plan": "pro"
  }'
```

When a tenant hits their workspace limit, `POST /workspaces` returns **`409 Conflict`** (not `402 Payment Required` — quota gates are resource-state conflicts, not payment failures, per RFC 9110). The response body's `error` field carries an upgrade hint.

---

## Step 5: Revoke access for a tenant

If a tenant stops paying or needs to be suspended:

```bash
# Suspend tenant (revokes their org API key and freezes workspace creation)
curl -X POST https://api.moleculesai.app/cp/orgs/acme/suspend \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET"
```

This action:
- Revokes all org-scoped API keys for the tenant
- Stops new workspace provisioning
- Keeps existing workspace data intact (you can resume or hard-delete later)

To hard-delete a tenant and all their workspaces:

```bash
curl -X DELETE https://api.moleculesai.app/cp/orgs/acme \
  -H "Authorization: Bearer $PROVISION_SHARED_SECRET"
  -H "Content-Type: application/json" \
  -d '{"confirm": true, "delete_workspaces": true}'
```

This terminates all EC2 instances, drops the Neon branch, and removes the org record. **This is irreversible.**

---

## Security model summary

| Layer | Isolation mechanism | Who manages |
|---|---|---|
| Database | Neon branch-per-tenant | Tenant's branch, operator has no direct access |
| Compute | EC2 in tenant's VPC | Control plane provisions, operator manages SG rules |
| Credentials | No Fly/API tokens on tenant | All cloud credentials held by control plane |
| API access | Org-scoped API keys | Tenant manages their own keys; operator has CP-level override |
| Network | Security group: port 443 from platform only | Control plane manages; tenant can't modify |

---

## What's next

- **Tenant registration UI**: expose a signup flow so customers can self-serve (roadmap: Phase 34)
- **Scoped roles**: give different team members read-only vs admin access within a tenant org (roadmap: Phase 34)
- **Usage-based billing**: Meter workspace runtime and forward events to Stripe for custom billing tiers

For runbook-level details on the provisioning flow, see the architecture docs at `docs/architecture/saas-prod-migration-2026-04-19.md`.

For the API reference, see `docs/api-reference.md` — the `/cp/orgs/*` endpoints are documented there.

---

*SaaS federation is available for all Molecule AI platform operators. Contact the Molecule AI team to enable federation on your control plane.*