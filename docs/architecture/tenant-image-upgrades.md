# Tenant Image Upgrade Strategies

> **Status:** Option B (sidecar auto-updater) implemented. Options A and C
> documented for future use.

## Problem

When we push a new `platform-tenant:latest` to GHCR, existing EC2 tenant
instances keep running the old image. New orgs get the latest image at boot,
but existing tenants fall behind — missing bug fixes, security patches, and
new features.

## Option A: Rolling restart on publish (coordinated)

The publish workflow calls a CP admin endpoint after pushing the image.
The CP iterates all running tenants and restarts them one by one.

```
publish-platform-image succeeds
  → POST https://api.moleculesai.app/cp/admin/rolling-upgrade
    → CP queries org_instances WHERE status = 'running'
    → For each tenant (staggered, 30s apart):
      1. AWS SSM Run Command: docker pull + docker restart
      2. Wait for /health 200
      3. Update org_instances.updated_at
      4. If health fails after 60s, rollback (docker run old image)
    → Return summary: {upgraded: N, failed: M, skipped: K}
```

### Pros
- Immediate, coordinated upgrades across all tenants
- CP has full visibility into upgrade status
- Can implement canary (upgrade 1 tenant first, verify, then rest)
- Rollback capability per tenant

### Cons
- Requires AWS SSM agent on EC2 instances (not installed yet)
- Alternatively requires SSH access from Railway → EC2 (network/key management)
- Brief downtime per tenant during restart (~10-30s)
- Blast radius: a bad image can take down all tenants before canary catches it

### Implementation effort
- Add SSM agent to EC2 user-data script
- Add `POST /cp/admin/rolling-upgrade` handler
- Add upgrade step to publish workflow
- Add rollback logic
- ~2-3 days

### When to use
- Urgent security patches that can't wait 5 min
- Breaking changes that need coordinated rollout
- When you want canary/staged deployment

---

## Option B: Sidecar auto-updater (implemented)

A cron job on each EC2 checks GHCR for a new image digest every 5 minutes.
If the digest changed, it pulls the new image and restarts the container.

```bash
# Runs every 5 min on each EC2 (added to user-data)
*/5 * * * * /usr/local/bin/molecule-auto-update.sh
```

The update script:
1. `docker pull platform-tenant:latest`
2. Compare digest with running container's image digest
3. If different: `docker stop molecule-tenant && docker rm molecule-tenant && docker run ...`
4. Wait for `/health` 200
5. Log result to `/var/log/molecule-auto-update.log`

### Pros
- Zero CP involvement — fully autonomous per tenant
- Tenants upgrade within 5 min of any publish
- No SSH/SSM infrastructure needed
- Each tenant upgrades independently (natural canary)
- Simple to implement (2 lines in user-data + a small script)

### Cons
- Up to 5 min delay between publish and tenant upgrade
- Brief downtime during restart (~10-30s)
- No centralized visibility into upgrade status
- Can't selectively hold back specific tenants
- All tenants track `latest` — no pinned versions

### When to use
- Default for all tenants
- Works well for early-stage SaaS with frequent deploys

---

## Option C: Blue-green via Worker (zero downtime)

Each EC2 runs two container slots: `blue` (current) and `green` (new).
The Cloudflare Worker routes traffic to whichever is healthy.

```
EC2 instance:
  molecule-tenant-blue  → :8080 (current, serving traffic)
  molecule-tenant-green → :8081 (new, starting up)

Upgrade flow:
  1. Pull new image
  2. Start green on :8081
  3. Health check green: GET :8081/health
  4. If healthy: update Worker routing (KV: slug → port 8081)
  5. Stop blue
  6. Next upgrade: blue becomes the new slot

Worker routing:
  KV key: "example-org" → {"ip": "<EC2_IP>", "port": 8081}
  (port defaults to 8080 when not in KV)
```

### Pros
- Zero downtime — traffic switches atomically after health check
- Instant rollback — just switch back to the old slot
- Worker already exists — just add port to the routing lookup
- Health-verified before any traffic switches

### Cons
- Double memory usage during transition (~512MB extra per tenant)
- More complex user-data script (manage two containers)
- Worker needs port-aware routing (KV schema change)
- Need to track which slot is active per tenant

### Implementation effort
- Update user-data to manage blue/green containers
- Update Worker to read port from KV
- Add blue/green state tracking to CP (org_instances.active_slot)
- Update auto-updater script for blue-green swap
- ~3-5 days

### When to use
- When tenants have SLAs requiring zero downtime
- Production deployments with paying customers
- After Option B proves the auto-update pattern works

---

## Migration path

```
Now:     Option B (auto-updater, 5 min delay, brief downtime)
         ↓
Growth:  Option A (add SSM for urgent patches, keep B as default)
         ↓
Scale:   Option C (zero-downtime for premium/enterprise tenants)
```
