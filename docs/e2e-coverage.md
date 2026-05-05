# E2E coverage matrix

This document is the source of truth for which E2E suites guard which surfaces and which gates are wired up where. Read this before adding a new E2E or moving a check between branches.

## Suites

| Workflow file | Job (= required-check name) | What it covers | Cron |
|---|---|---|---|
| `e2e-api.yml` | `E2E API Smoke Test` | A2A handshake, registry/register, /workspaces/:id/a2a forward, structured-event emission. Lightweight enough to run on every PR. | — |
| `e2e-staging-canvas.yml` | `Canvas tabs E2E` | Canvas-tab Playwright UX checks against staging — config tab, secrets tab, agent-card tab, Activity hydration. | weekly Sun 08:00 UTC |
| `e2e-staging-saas.yml` | `E2E Staging SaaS` | Full lifecycle: org creation → workspace provision (CP path) → A2A delegation → status/heartbeat → workspace delete → EC2 termination. The integration test that catches the silent-drop bug class (#2486 / #2811 / #2813 / #2814). | daily 07:00 UTC |
| `e2e-staging-external.yml` | `E2E Staging External Runtime` | External-runtime registration + heartbeat staleness sweep + `/registry/peers` resolution. Validates the OSS-templated workspace path. | daily 07:30 UTC |
| `e2e-staging-sanity.yml` | `Intentional-failure teardown sanity` | Inverted assertion — the run MUST fail. Validates the leak-detection self-check itself; not for general gating. | weekly Mon 06:00 UTC |
| `continuous-synth-e2e.yml` | `Synthetic E2E against staging` | Standing background coverage between PR runs. Catches drift in production-like staging that PR-time E2Es miss. | every 15 min |

## Required-check status (branch protection)

| Suite | staging required | main required |
|---|---|---|
| `E2E API Smoke Test` | ✅ this PR | ✅ |
| `Canvas tabs E2E` | ✅ this PR | (see follow-up) |
| `E2E Staging SaaS` | ❌ — needs always-emit refactor | ❌ |
| `E2E Staging External Runtime` | ❌ — needs always-emit refactor | ❌ |
| `Intentional-failure teardown sanity` | ❌ inverted assertion, never required | ❌ |
| `Synthetic E2E against staging` | ❌ cron-only, not a per-PR gate | ❌ |

## Why the always-emit pattern matters

Branch protection requires a *check name* to land at SUCCESS for every PR. Workflows with `paths:` filters that exclude a PR never run, so the check name never appears, and the PR sits BLOCKED forever.

The pattern that supports being required is:

1. Workflow always triggers on push/PR to the protected branch.
2. A `detect-changes` job uses `dorny/paths-filter` to decide if real work runs.
3. The protected job runs unconditionally and either (a) does real work when paths matched, or (b) emits a no-op SUCCESS step when paths skipped.

`e2e-api.yml` and `e2e-staging-canvas.yml` already have this shape. `e2e-staging-saas.yml` and `e2e-staging-external.yml` use plain `paths:` filters and need the refactor before they can be required (filed as follow-up).

## Adding a new E2E suite

1. Pick a verb: smoke test, full lifecycle, fault-injection, drift detection. Pre-existing suites split along these lines.
2. Use the always-emit shape so the check name can be made required.
3. Add a row to the matrix above.
4. Decide cron cadence based on cost + how fast drift would otherwise be caught.
5. If you want it required, add to the relevant branch protection via `tools/branch-protection/apply.sh` (this PR adds the script).

## When to break glass — temporarily skip a required E2E

Don't. If an E2E is intermittently flaky, fix the test or move it out of required. The point of a required check is that it's load-bearing; bypassing one with admin override teaches the next operator the gate is optional.

If a Production incident requires bypassing, document the override in the incident postmortem with a same-week followup to either fix the test or rip the check out of required.

## Related issues / PRs

- #2486 — silent-drop bug class that the SaaS E2E now catches
- PR #2811 — `provisionWorkspaceAuto` consolidation (org-import SaaS gate)
- PR #2824 — `StopWorkspaceAuto` mirror (closes #2813 + #2814)
- Follow-up: refactor `e2e-staging-saas` + `e2e-staging-external` to always-emit (so they can be required)
