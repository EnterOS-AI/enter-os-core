# Incident: SaaS tenant provisioning 401 on /cp/tenants/boot-event

**Date:** 2026-04-23
**Severity:** High — every new SaaS tenant blocked
**Detection path:** E2E Staging SaaS run 24848425822 failed at "tenant provisioning"; investigation of CP Railway logs surfaced the auth mismatch.
**Status:** Fix pushed on [molecule-controlplane#238](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/238).
**Related:** [issue #239](https://git.moleculesai.app/molecule-ai/molecule-controlplane/issues/239) (Cloudflare DNS record quota), [testing-strategy.md](../engineering/testing-strategy.md)

## Summary

For ~3 days leading up to 2026-04-23, every new SaaS tenant failed to transition from `provisioning` → `running`. The EC2 instance would boot, read its `admin_token` from AWS Secrets Manager, and attempt to POST `/cp/tenants/boot-event` on the control plane. Every request got 401 Unauthorized. Without a successful boot event, CP would wait 4 minutes, fall through to a canary probe (which also failed due to an unrelated Cloudflare DNS quota issue), and write a `status='failed'` row. The tenant would then be stuck forever.

## Root cause

**A race between EC2 boot and the DB write of `org_instances.admin_token`.**

The flow was:

1. CP `provisionTenant()` called `Provision()`, which:
   - Generated `admin_token = generatePassword()`
   - Wrote it to AWS Secrets Manager
   - Returned it in the `Result` struct
2. EC2 launched in parallel; user-data started running.
3. **Before** CP's `provisionTenant()` wrote the `org_instances` row, it called `WaitForTenantReady()` — a 4-minute poll of `org_instance_boot_events`.
4. EC2 finished its early boot stages (~60-90s) and started POSTing `/cp/tenants/boot-event` with the `admin_token` from Secrets Manager.
5. CP's inline auth on that endpoint does:
   ```sql
   SELECT org_id FROM org_instances WHERE admin_token = $1 AND admin_token != ''
   ```
   No row existed yet. → 401.
6. Every subsequent boot-event post: 401.
7. `WaitForTenantReady` saw no events (because 401s never write to `org_instance_boot_events`). After 4 minutes it returned `false`.
8. Fell through to canary. Canary failed (unrelated — Cloudflare DNS quota exceeded, so the tenant's hostname didn't resolve).
9. `insertFailedInstance` wrote a row **without** `admin_token`. Tenant stuck in `failed`.

### The commit that introduced the bug

[molecule-controlplane#235](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/235) — "fix(provision): wait for tenant boot-event before falling back to canary". Merged 2026-04-22.

Before #235, readiness was determined via a canary probe through Cloudflare's edge — which didn't need CP-side auth, so the INSERT ordering didn't matter. #235 made boot-events the primary readiness signal but didn't move the INSERT earlier. The race was latent before but became load-bearing after.

## Detection

**What should have caught it:**

- ❌ Unit tests on `provisionTenant` — existed, but they used `fakeProv` and `noopCanaryOK` that bypassed the real auth flow. They asserted the INSERT happened eventually; they didn't assert the INSERT happened *before* boot-event auth.
- ❌ Integration tests — CP has no end-to-end integration test that provisions a real tenant with real auth against a real DB. The E2E Staging SaaS flow is the closest, and it only ran in CI after merge.
- ✅ E2E Staging SaaS — did catch it, but ~20 hours after merge. Blast radius by then: every new tenant in staging, including all E2E runs.

**What actually caught it:**

Manual investigation of CP Railway logs for the failed E2E run. Grepping for the tenant org_id + examining the `[GIN] POST /cp/tenants/boot-event` status codes revealed the 401 pattern.

## Timeline

| Time (UTC) | Event |
|---|---|
| 2026-04-22 ~late | PR #235 merged to controlplane main — introduces the race |
| 2026-04-22 → 23 | Nightly E2E Staging SaaS fails (no alert wired) |
| 2026-04-23 07:14 | E2E on main also fails with the same signature |
| 2026-04-23 morning | Investigation starts; misattributed to hermes provider 401 (separate known bug) |
| 2026-04-23 17:09 | Fresh E2E run 24848425822 dispatched on staging sha `6539908` |
| 2026-04-23 17:13 | Run fails with "tenant provisioning failed" |
| 2026-04-23 ~17:15 | Railway logs inspection reveals the 401s on `/cp/tenants/boot-event` |
| 2026-04-23 17:30 | Root cause identified — admin_token not in DB when EC2 phones home |
| 2026-04-23 ~17:50 | Fix pushed on controlplane `fix/provision-readiness-boot-events` |
| 2026-04-23 ~18:00 | PR #238 opened, CI running |

## Fix

Write the `org_instances` row with `status='provisioning'` and `admin_token` **immediately after** `Provision()` returns, **before** `WaitForTenantReady()`. Flip `status='running'` once readiness passes.

```go
// NEW: early INSERT so boot-events can authenticate
if _, err := h.db.ExecContext(ctx, `
    INSERT INTO org_instances (org_id, ..., admin_token, status)
    VALUES ($1, ..., $8, 'provisioning')
    ON CONFLICT (org_id) DO UPDATE SET ..., status = 'provisioning'
`, ...); err != nil {
    h.insertFailedInstance(ctx, org.ID, ...)
    return
}

// THEN wait for readiness — boot-events will now authenticate
bootReady, _ := provisioner.WaitForTenantReady(ctx, h.db, org.ID, 4*time.Minute)

// ... canary fallback as before ...

// Finally, transition to 'running'
h.db.ExecContext(ctx, `UPDATE org_instances SET status = 'running' WHERE org_id = $1`, org.ID)
```

See [molecule-controlplane#238](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/238) for the full diff.

## Lessons

### 1. "Write state before dependent reads" is a general pattern

The same chicken-and-egg shape applies anywhere a newly-provisioned entity phones home for its own state. Future auth-gated callbacks should follow the rule: **persist the credential in the validation store BEFORE the entity can call back with it.** Include in code review checklist for provisioning-adjacent changes.

### 2. Unit tests that use fakes can't catch auth-flow races

The existing `TestProvisionTenant_*` tests used `fakeProv` and `noopCanaryOK` that elided the real auth check. They asserted the shape of DB writes but not the temporal ordering relative to an external caller's expectation. For provisioning flows specifically, we need an integration-test tier that exercises real HTTP → real DB with the actual auth middleware.

**Action:** Add a CP integration-test target (`make test-integration`) that spins up a real Postgres + CP binary + a fake EC2 that mimics user-data's boot-event POST cadence. File as follow-up.

### 3. E2E failures need faster detection

E2E Staging SaaS failed silently overnight. Nobody knew until someone manually ran `gh run list` and saw the red dots. The alert latency from merge to awareness was ~20 hours.

**Action:** Wire E2E Staging SaaS failures to a push notification or Telegram alert channel. File as follow-up.

### 4. Code comments should describe invariants, not the happy path

The `provisionTenant` function had comments describing what each block did, but nothing stating **"this function must write `org_instances.admin_token` before any code path that triggers an external callback using it."** If that invariant had been written down, the #235 author would likely have noticed the ordering change broke it.

**Action:** When landing this fix, add the invariant to a doc comment at the top of `provisionTenant`.

### 5. Separate unrelated failures — don't conflate

Early investigation blamed the hermes provider 401 bug (a separate, known issue affecting hermes-agent startup after tenant came up). Those 401s come from `hermes-agent error 401` in the workspace-server logs, not from CP Railway logs. Two different 401s with totally different causes. **When debugging, always check which component is emitting the 401 before assuming it's the known one.**

## Follow-ups

- [ ] Land [molecule-controlplane#238](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/238)
- [ ] Redeploy staging-api, verify E2E goes green
- [ ] Add CP integration test suite (see lesson #2)
- [ ] Wire E2E failure → notification (see lesson #3)
- [ ] Add invariant comment in `provisionTenant` (see lesson #4)
- [ ] Cloudflare DNS quota cleanup — [molecule-controlplane#239](https://git.moleculesai.app/molecule-ai/molecule-controlplane/issues/239)
