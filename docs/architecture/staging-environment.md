# Staging Environment Design

> **Status:** Planned — gates all future infra changes (Tunnel migration,
> security fixes, etc.)
>
> **Problem:** We merge directly to main and auto-deploy to production.
> Today's session broke CI twice and caused hours of Cloudflare edge cache
> issues because there was no staging to test infra changes first.
>
> **Goal:** Full staging environment that mirrors production. Every change
> ships to staging first, gets verified, then promotes to production.

---

## Architecture

```
                    staging                         production
                    ───────                         ──────────
Git branch:         main (auto-deploy)              main (manual promote)
                    or staging branch               

CP (Railway):       staging service                 production service
                    staging.api.moleculesai.app     api.moleculesai.app

Tenant EC2s:        staging EC2 instances            production EC2 instances
                    *.staging.moleculesai.app        *.moleculesai.app

App (Vercel):       staging.app.moleculesai.app     app.moleculesai.app
                    (Vercel preview)                 (Vercel production)

DB (Neon):          staging branch                   main branch
                    (or separate project)            

Docker images:      platform-tenant:staging          platform-tenant:latest
                    (GHCR)                           (GHCR)

Cloudflare:         *.staging.moleculesai.app        *.moleculesai.app
                    (separate tunnel/worker)         (tunnel per tenant)
```

## Deploy flow

```
Developer pushes to PR branch
  → CI runs (tests, build, lint)
  → PR merged to main
  → Auto-deploy to STAGING
  → Staging smoke tests (automated)
  → Manual verification if needed
  → Promote to PRODUCTION (manual trigger or approval)
```

## Components

### 1. Railway: two environments

Railway supports multiple environments per project. Create a `staging`
environment alongside `production`:

```bash
railway environment create staging
railway variables --environment staging --set "DATABASE_URL=<staging-neon>"
railway variables --environment staging --set "MOLECULE_ENV=staging"
# ... all other vars with staging-specific values
```

**Deploy trigger:**
- `staging`: auto-deploy on push to main
- `production`: manual promote via `railway up --environment production`
  or GitHub Actions workflow_dispatch

**Domains:**
- staging: `staging-api.moleculesai.app` (Railway custom domain)
- production: `api.moleculesai.app` (unchanged)

### 2. Neon: branch per environment

Neon supports database branches (like git branches):

```bash
# Create staging branch from main
neon branch create --project-id <id> --name staging --parent main
```

- Staging DB has same schema, separate data
- Can reset staging by re-branching from main
- Production data never touched by staging tests

### 3. Vercel: preview deployments

Vercel already supports this natively:
- Push to main → deploys to `app.moleculesai.app` (production)
- Push to `staging` branch → deploys to preview URL

**Or** use Vercel environments:
- `staging.app.moleculesai.app` → staging deployment
- `app.moleculesai.app` → production deployment

### 4. GHCR: tagged images

```
platform-tenant:staging    — built on every push to main
platform-tenant:latest     — promoted from staging after verification
platform-tenant:sha-xxxxx  — immutable, pinned to specific commit
```

**Publish workflow change:**
```yaml
# Current: pushes :latest on every main merge
# New: pushes :staging on every main merge
#       pushes :latest only on manual promote
```

### 5. Cloudflare: staging subdomain

Option A (simple): `*.staging.moleculesai.app` with its own tunnel/worker
Option B (full): separate Cloudflare zone for staging (overkill)

Recommend Option A:
- Add `staging.moleculesai.app` DNS records
- Staging tenants get `slug.staging.moleculesai.app` subdomains
- Production tenants get `slug.moleculesai.app` (unchanged)

### 6. EC2: staging tag

Staging EC2 instances tagged with `Environment=staging`:
- Separate from production instances in AWS console
- Can use different AMI, instance type, security group
- Easy to identify and clean up

## Environment variables

| Variable | Staging | Production |
|----------|---------|------------|
| `MOLECULE_ENV` | `staging` | `production` |
| `DATABASE_URL` | Neon staging branch | Neon main branch |
| `TENANT_IMAGE` | `platform-tenant:staging` | `platform-tenant:latest` |
| `APP_DOMAIN` | `staging.moleculesai.app` | `moleculesai.app` |
| `CORS_ORIGINS` | `https://staging.app.moleculesai.app` | `https://app.moleculesai.app` |
| `ADMIN_TOKEN` | per-tenant (same mechanism) | per-tenant |

## Promotion workflow

### Automated (CI/CD)

```yaml
# .github/workflows/promote-to-production.yml
name: Promote to Production
on:
  workflow_dispatch:
    inputs:
      confirm:
        description: 'Type "promote" to confirm'
        required: true

jobs:
  promote:
    if: github.event.inputs.confirm == 'promote'
    steps:
      # 1. Run staging smoke tests one more time
      - run: bash tests/e2e/test_saas_tenant.sh
        env:
          TENANT_SLUG: smoke-test
          BASE_URL: https://staging.api.moleculesai.app

      # 2. Tag Docker image
      - run: |
          docker pull ghcr.io/molecule-ai/platform-tenant:staging
          docker tag ghcr.io/molecule-ai/platform-tenant:staging \
                     ghcr.io/molecule-ai/platform-tenant:latest
          docker push ghcr.io/molecule-ai/platform-tenant:latest

      # 3. Deploy CP to production
      - run: railway up --environment production

      # 4. Production tenants auto-update within 5 min (Option B cron)
```

### Manual (for now)

Until the automated workflow is built:
1. Verify on staging (`staging.api.moleculesai.app`)
2. `docker tag platform-tenant:staging platform-tenant:latest && docker push`
3. `railway up --environment production`
4. Monitor production health

## What this prevents

- CI breakage from untested path filters (today's dorny/paths-filter issue)
- Cloudflare edge cache poisoning (test DNS changes on staging subdomain)
- Workspace boot script regressions (test on staging EC2 first)
- DB migration failures (test on Neon staging branch)
- Auth/security regressions (staging has same auth stack)

## Implementation order

1. **Railway staging environment** — create + configure vars (~30 min)
2. **Neon staging branch** — create from main (~5 min)
3. **Staging DNS** — `staging.api.moleculesai.app` CNAME to Railway (~5 min)
4. **Publish workflow** — push `:staging` tag instead of `:latest` (~15 min)
5. **Promotion workflow** — manual trigger to promote staging → production (~30 min)
6. **Vercel staging** — configure preview deployment URL (~15 min)
7. **Staging smoke test** — automated test after staging deploy (~30 min)

**Total:** ~2.5 hours for full staging pipeline.

## Cost

- Railway staging: ~$5/mo (same as production, but can be smaller)
- Neon staging branch: free (included in plan)
- EC2 staging instances: only when testing (terminate after)
- Vercel: free (preview deployments included)
- Cloudflare: free (same zone, additional records)
