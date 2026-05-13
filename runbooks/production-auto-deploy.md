# Production Auto-Deploy

`molecule-core` deploys production tenant code automatically from Gitea Actions.

This runbook is an implementation-specific companion to `runbooks/sop-production-cicd.md`.

## Default Flow

On a push to `main` that touches deployable code, `.gitea/workflows/publish-workspace-server-image.yml`:

1. Builds and pushes platform and tenant ECR images tagged `staging-<sha>` and `staging-latest`.
2. Self-tests the production deploy helper and workflow-YAML linter.
3. Waits for strict required push contexts on the same commit to become `success`.
4. Calls production control-plane `POST /cp/admin/tenants/redeploy-fleet` with `target_tag=staging-<sha>`.
5. Verifies every redeploy result is healthy and every tenant returns the same Git SHA from `/buildinfo`.

The deploy workflow intentionally does not use Gitea `concurrency` because Gitea 1.22.6 can cancel queued runs even when `cancel-in-progress: false`.

## Kill Switch

Set either repository variable or secret:

```text
PROD_AUTO_DEPLOY_DISABLED=true
```

The image publish still runs, but the production redeploy step exits successfully without touching tenants.
Immediately before the production POST, the workflow re-checks the live Gitea repo variable when `PROD_AUTO_DEPLOY_CONTROL_TOKEN` can read Actions variables. If that token is not configured, the job-start value is still honored.

## Tunables

Repository variables:

```text
PROD_CP_URL=https://api.moleculesai.app
PROD_AUTO_DEPLOY_CANARY_SLUG=hongming
PROD_AUTO_DEPLOY_SOAK_SECONDS=60
PROD_AUTO_DEPLOY_BATCH_SIZE=3
PROD_AUTO_DEPLOY_DRY_RUN=false
PROD_MANUAL_REDEPLOY_TARGET_TAG=staging-<known-good-sha>
```

Secrets required:

```text
CP_ADMIN_API_TOKEN
AUTO_SYNC_TOKEN
PROD_AUTO_DEPLOY_CONTROL_TOKEN
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
```

`AUTO_SYNC_TOKEN` is only used to read Gitea commit statuses while waiting for required push contexts.
`PROD_AUTO_DEPLOY_CONTROL_TOKEN` is optional but recommended so the pre-POST kill-switch check can read the live `PROD_AUTO_DEPLOY_DISABLED` Actions variable.

## Manual Fallback

Use `.gitea/workflows/redeploy-tenants-on-main.yml` when the automatic path needs to be rerun or rolled back. Gitea 1.22.6 does not support reliable `workflow_dispatch` inputs, so rollback uses a repo variable:

1. Set `PROD_MANUAL_REDEPLOY_TARGET_TAG=staging-<known-good-sha>`.
2. Dispatch `manual-redeploy-tenants-on-main`.
3. Clear `PROD_MANUAL_REDEPLOY_TARGET_TAG` after the rollback finishes.

With no variable set, the fallback redeploys `staging-<current-main-sha>`.
