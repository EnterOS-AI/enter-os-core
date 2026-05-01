# Demo-day runbook

Pre-, during-, and post-demo operational procedures for the molecule
production stack. Updated 2026-05-01 ahead of the funding-demo on
~2026-05-06.

The whole stack:

```
Vercel canvas (app.moleculesai.app)
  → Railway controlplane (api.moleculesai.app)
    → CloudFront/Cloudflare per-tenant edge (<slug>.moleculesai.app)
      → EC2 tenant instance running platform container
        → Docker workspaces pulled from
          ghcr.io/molecule-ai/workspace-template-<runtime>:latest
```

Every layer has its own deploy/rollback story. This runbook indexes
them in the order an operator would touch them during an incident.

## Pre-demo (T-48h to T-1h)

### 1. Freeze the runtime + template image cascade

A merge to `molecule-core/staging` that touches `workspace/**` triggers
`publish-runtime.yml` → PyPI bump → repository_dispatch → 8 template
repos rebuild and re-tag `:latest`. A merge to any template repo's
`main` triggers the same final re-tag directly. Either path means a
new workspace provision during the demo pulls whatever `:latest`
resolved to seconds earlier.

Capture current good digests + disable both cascade vectors:

```bash
# Dry-run first — verifies digests can be fetched and tooling is set up
scripts/demo-freeze.sh

# Apply
scripts/demo-freeze.sh --execute
```

The script writes two receipts to `scripts/demo-freeze-snapshots/`:

- `digests-<TS>.txt` — current `:latest` digest per template (rollback target if needed)
- `disabled-workflows-<TS>.txt` — workflow paths to re-enable post-demo

Verify the freeze landed:

```bash
gh workflow list -R Molecule-AI/molecule-core | grep publish-runtime
# expect: status = disabled_manually
```

If a critical fix MUST ship during the freeze window:

1. `gh workflow enable publish-runtime.yml -R Molecule-AI/molecule-core`
2. Merge the fix
3. Watch the cascade through to GHCR:latest manually
4. Smoke-verify against a staging tenant (`scripts/api-smoke.sh` or
   manual canvas walkthrough)
5. `gh workflow disable publish-runtime.yml -R Molecule-AI/molecule-core` to re-freeze

Don't auto-promote during the freeze — the value of the freeze is that
nothing happens automatically.

### 2. Confirm production CP is on the expected SHA

```bash
gh run list -R Molecule-AI/molecule-controlplane --branch main --limit 5
# Last `ci` run should be SUCCESS with the SHA you intend to demo on
```

Railway auto-deploys from main. Spot-check `api.moleculesai.app`:

```bash
curl -fsS -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  https://api.moleculesai.app/cp/admin/orgs?limit=1
# Expect: 200 + a JSON {"orgs": [...]}
```

### 3. Confirm production canvas (Vercel) is on main

Vercel auto-deploys `main`. Verify in the Vercel dashboard the most
recent prod deploy ran from the expected commit SHA.

### 4. Pre-warm the demo tenant

Cold-start times on workspace-template images:

| Runtime | Cold-start (first boot) |
|---|---|
| claude-code | ~30-60s |
| openclaw | ~1-2 min |
| langgraph | ~1 min |
| hermes | **~7 min** (large image) |

If the demo will use `hermes`, provision the demo workspace at least
10 min before. The cold-start clock starts when the workspace is
created, not when it's used.

## During demo — emergency rollback levers

### Lever A: Platform-image rollback (canvas/CP layer regression)

If the canvas or platform container shipped a regression, retag
`:latest` to a prior staging SHA without rebuilding:

```bash
# Find a known-good SHA from staging history
gh run list -R Molecule-AI/molecule-core --workflow=publish-canvas-image.yml --limit 5

# Roll both platform + tenant images
GITHUB_TOKEN=$(gh auth token) scripts/rollback-latest.sh <good-sha>
```

`rollback-latest.sh` retags both `ghcr.io/molecule-ai/platform:latest`
and `ghcr.io/molecule-ai/platform-tenant:latest`. Existing tenants
auto-pull `:latest` every 5 min — rollback propagates without manual
restart.

### Lever B: Workspace-template image rollback

If a specific runtime template (claude-code, hermes, etc.) shipped a
broken `:latest`:

```bash
# Get the demo's snapshotted-good digest from the freeze receipt
grep claude-code scripts/demo-freeze-snapshots/digests-<TS>.txt

# Retag :latest back to the snapshotted digest using crane
crane auth login ghcr.io -u "$(gh api user --jq .login)" \
  --password-stdin <<< "$(gh auth token)"
crane tag \
  ghcr.io/molecule-ai/workspace-template-claude-code@sha256:<digest> \
  latest
```

The next workspace provision pulls the rolled-back image. Existing
workspaces are unaffected (their image is already loaded into Docker).

### Lever C: Wedged demo tenant — redeploy

If the demo tenant's EC2 instance is wedged (boot succeeded but app
not responding, or a stuck workspace), the controlplane has an admin
redeploy endpoint:

```bash
# AWS-side: forces a fresh EC2 launch with current image. ~3 min.
curl -fsS -X POST \
  -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  https://api.moleculesai.app/cp/admin/orgs/<slug>/redeploy
```

WARNING per memory: this triggers real EC2 + SSM actions on production.
Double-check `<slug>` against the demo tenant's slug before pressing
return. The `/redeploy` endpoint is idempotent on the EC2 side but
WILL drop active SSH sessions.

### Lever D: Specific bad workspace — delete

If a single workspace inside the demo tenant is misbehaving (e.g.
hermes wedged on cold-start, claude-code returning the generic
"Agent error (Exception)" message), kill it:

```bash
# Get the demo tenant's per-tenant ADMIN_TOKEN
TENANT_ADMIN=$(curl -fsS -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  https://api.moleculesai.app/cp/admin/orgs/<slug>/admin-token \
  | jq -r .admin_token)

ORG_ID=$(curl -fsS -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  https://api.moleculesai.app/cp/admin/orgs?limit=20 \
  | jq -r '.orgs[] | select(.slug=="<slug>") | .id')

# Delete the bad workspace
curl -fsS -X DELETE \
  -H "Origin: https://<slug>.moleculesai.app" \
  -H "Authorization: Bearer $TENANT_ADMIN" \
  -H "X-Molecule-Org-Id: $ORG_ID" \
  https://<slug>.moleculesai.app/workspaces/<workspace-id>
```

Then re-provision a fresh workspace from the canvas. Faster than
debugging the wedged one.

### Lever E: Railway production rollback (CP regression)

If the last Railway deploy of CP introduced a regression that lever A
can't fix (e.g. a logic bug, not a container issue):

1. Open Railway dashboard → molecule-platform → controlplane → Deployments
2. Find the previous-known-good deployment
3. Click **Rollback to this deployment**

Manual step — no CLI equivalent built. Takes ~30s to redeploy from
the prior image. Note: rollback restores the prior code AND prior env
var snapshot; don't expect any env var changes made since to persist.

### Lever F: Vercel production rollback (canvas regression)

If the canvas ships a regression:

1. Open Vercel dashboard → molecule-app → Deployments
2. Find the previous prod deployment
3. **Promote to Production**

Same pattern as Railway — fast revert, no rebuild.

## Tenant-level read-only diagnostics (not actions)

Useful during a "is this working?" moment without touching anything:

```bash
# Tenant infra state
curl -fsS -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  "https://api.moleculesai.app/cp/admin/orgs?limit=20" \
  | jq '.orgs[] | select(.slug=="<slug>")'

# Tenant boot events (debug a stuck provision)
curl -fsS -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
  "https://api.moleculesai.app/cp/admin/tenants/<slug>/boot-events?limit=50" \
  | jq

# Workspace activity (debug an unresponsive agent)
curl -fsS \
  -H "Origin: https://<slug>.moleculesai.app" \
  -H "Authorization: Bearer $TENANT_ADMIN" \
  -H "X-Molecule-Org-Id: $ORG_ID" \
  "https://<slug>.moleculesai.app/workspaces/<workspace-id>/activity?limit=20" \
  | jq
```

## Post-demo (T+30m to T+24h)

### 1. Thaw the cascades

```bash
# Find the freeze receipt
ls scripts/demo-freeze-snapshots/

# Thaw — pass the timestamp suffix
scripts/demo-thaw.sh 20260506-180000
```

The next merge to `molecule-core/staging` (workspace/**) or any
template repo's `main` will resume the auto-rebuild cascade.

### 2. Audit what was held back

If any merges queued during the freeze:

```bash
gh pr list -R Molecule-AI/molecule-core --base staging --state merged \
  --search "merged:>=$(date -u -v-7d +%Y-%m-%d)"
```

Verify each merge's CI is green and dispatch the runtime cascade once
to ensure all templates rebuild against the post-freeze HEAD.

### 3. File a post-mortem if anything fired

If any rollback lever was used during the demo, file a brief doc:

- Which lever (A through F)
- Which SHA was rolled back FROM and TO
- Did the rollback fully resolve the issue or was a follow-up needed
- Whether the underlying regression should have been caught by CI

## Common issues + first-line fix

| Symptom | First lever to try |
|---|---|
| Workspace boots but agent always errors | Lever D (delete + reprovision) |
| Whole tenant unreachable | Lever C (redeploy) |
| Canvas crashes on load | Lever F (Vercel rollback) |
| Login broken / API errors | Lever E (Railway rollback) |
| Specific runtime broken across tenants | Lever B (template image rollback) |
| Platform container regression | Lever A (rollback-latest.sh) |
| Mid-demo stray PR auto-published a bad image | Lever B + investigate why freeze didn't catch it |

## Auth fingerprint (rotate post-demo)

The freeze + rollback procedures assume:

- `CP_ADMIN_API_TOKEN` available via `railway variables --kv --environment production`
- `gh auth token` returns a working PAT with `workflow:write` + `write:packages`
- `crane` installed (`brew install crane`)

After the demo, **rotate** `CP_ADMIN_API_TOKEN` (it's the keys-to-the-kingdom
token for production) — it likely got copy-pasted into shells during
the demo.

```bash
# Generate a new admin token
NEW_TOKEN=$(openssl rand -hex 32)

# Update Railway production env var (and optionally staging)
railway variables --set CP_ADMIN_API_TOKEN="$NEW_TOKEN" --environment production

# Restart CP service to pick up the change
# (Railway auto-restarts on env var change)

# Verify
curl -fsS -H "Authorization: Bearer $NEW_TOKEN" \
  https://api.moleculesai.app/cp/admin/orgs?limit=1
```
