# Testing Strategy

**Status:** Policy. Update when tier definitions or thresholds change.
**Audience:** Everyone writing or reviewing code in this repo.
**Cross-refs:** [backends.md](../architecture/backends.md), [pr-hygiene.md](./pr-hygiene.md), [postmortem-2026-04-23-boot-event-401.md](./postmortem-2026-04-23-boot-event-401.md)

## The short version

- **Don't chase 100% coverage.** The last 15-20% costs as much as the first 80% and mostly adds brittle tests of trivial getters, error branches that can't fire, and stdlib wrappers.
- **Different code classes have different floors.** Auth at 80% is scarier than a DTO at 50%. Match the test investment to the risk.
- **Tests should pay rent.** A test that runs lines but asserts nothing meaningful isn't catching bugs — it's just dragging refactors down.

## Tiered coverage floors

Every Go package, every TypeScript module, every Python module fits one of these tiers. The tier determines the minimum acceptable coverage — and the review standard.

| Tier | Examples | Line floor | Branch floor | Review standard |
|---|---|---|---|---|
| **1. Auth / secrets / crypto** | `tokens`, `session_auth`, `wsauth_middleware`, `crypto/envelope`, `cp_tenant_auth` | **90%** | **85%** | Every branch tested. Adversarial scenarios (cross-tenant, expired token, null origin, malformed header). Timing considered. |
| **2. Handlers with side effects** | `workspace_provision`, `workspace_crud`, `container_files`, `terminal`, `registry` | **75%** | 70% | Happy + main error paths. DB mocks. Ownership / tenant-isolation checks. |
| **3. State machines + workers** | `scheduler`, `provisioner`, `healthsweep`, `orphan-sweeper`, `boot_ready` | **75%** | 70% | Every state transition tested, plus the transitions that *shouldn't* fire. |
| **4. Config / business logic** | `budget`, `orgtoken` (validation), `templates`, `derive-provider`, `redaction` | **70%** | 65% | Standard unit-test territory. Table-driven preferred. |
| **5. Plain DTOs / generated** | `models/*`, proto-generated Go, TypeScript interfaces | none | none | Writing tests here is theatre. Don't. |
| **6. CLI glue / cmd/*** | `cmd/server`, `cmd/molecli` | smoke only | — | Integration tests / E2E cover these. One startup-smoke test per binary. |
| **7. Third-party wrappers** | `awsapi`, `cloudflareapi`, `stripeapi`, `neonapi` | integration | — | Unit tests mock vendor shape, not behavior. Real behavior covered by staging integration. |

### Why a blanket percentage is wrong

- A `models/` package at 90% means you wrote tests for `func (w Workspace) ID() string { return w.id }`. No bugs caught, but coverage number is green.
- A `tokens` package at 75% means some rejection branch isn't covered. Maybe the *exact* branch that lets a revoked token still authenticate.
- Blanket targets make the first case look equivalent to the second. They aren't.

## Current state (as of 2026-04-23)

Run `go test ./... -cover` in each repo for up-to-date numbers. Snapshot:

### workspace-server (Go)

| Package | Actual | Tier | Target | Gap |
|---|---:|---|---:|---:|
| `internal/handlers/tokens.go` | **0%** | 1 | 90% | 90 |
| `internal/handlers/workspace_provision.go` | **0%** | 2 | 75% | 75 |
| `internal/middleware/wsauth_middleware.go` | ~48% | 1 | 90% | 42 |
| `internal/provisioner` | 45% | 3 | 75% | 30 |
| `internal/scheduler` | 49% | 3 | 75% | 26 |
| `internal/channels` | 40% | 4 | 70% | 30 |
| `internal/orgtoken` | 88% | 4 | 70% | — |
| `internal/crypto` | 91% | 1 | 90% | — |
| `internal/supervised` | 93% | 3 | 75% | — |
| `internal/plugins` | 94% | 4 | 70% | — |
| `internal/envx` | 100% | 5 | none | — |

### molecule-controlplane (Go)

| Package | Actual | Tier | Target | Gap |
|---|---:|---|---:|---:|
| `internal/awsapi` | 18% | 7 | integration | — |
| `internal/provisioner` | 48% | 3 | 75% | 27 |
| `internal/handlers` | 60% | 2 | 75% | 15 |
| `internal/billing` | 60% | 4 | 70% | 10 |
| `internal/crypto` | 68-80% | 1 | 90% | 10-22 |
| `internal/auth` | 96% | 1 | 90% | — |
| `internal/middleware` | 97% | 1 | 90% | — |
| `internal/reserved` | 100% | 5 | none | — |
| `internal/httpx` | 100% | 4 | 70% | — |

### canvas (TypeScript)

**No coverage instrumentation today.** 900 tests / 58 files pass, but coverage isn't measured. See issue #1815 for the fix: set a 70% line floor in `vitest.config.ts` and gate CI on it.

### workspace (Python)

**No pytest/coverage config.** See issue #1818: set up `pytest-cov` with `--cov-fail-under=75` (ratchet from current baseline over 2-3 weeks).

## Writing a good test

A good test:
- **Asserts a specific outcome**, not that a function runs without error.
- **Covers the exact branch that bugs would live in** — cross-tenant access, revoked-but-cached token, race on state transition.
- **Uses table-driven patterns** when the code is a dispatch with N cases. One test row per case.
- **Mocks at system boundaries** (DB, HTTP, time), not at internal package boundaries.
- **Survives refactors** — tests behavior, not internal state.

A bad test:
- Tests a getter that just returns a field.
- Mocks the function under test itself.
- Relies on `time.Sleep` or clock timing to assert order.
- Asserts `nil == nil` to boost coverage.

## Enforcement

### CI gates

- **Go**: `go test ./... -cover` + a pre-commit script that compares coverage to `.coverage-baseline` and fails on drops > 2 points in a tier-1 package.
- **TypeScript**: `vitest --coverage` with thresholds in `vitest.config.ts`. Fails CI if below.
- **Python**: `pytest --cov-fail-under=75` in the Python CI job.

### Review expectations

- Any PR touching a tier-1 package that lowers its coverage needs an explicit reviewer sign-off and justification.
- New code should arrive at or above its tier's floor.
- Untested files in tier-1 or tier-2 should be flagged in review, not waved through.

## Related

- [Issue #1821](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1821) — policy tracking issue
- [Issue #1815](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1815) — Canvas coverage instrumentation
- [Issue #1818](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1818) — Python pytest-cov
- [Issue #1814](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1814) — workspace_provision_test.go unblock
- [Issue #1816](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1816) — tokens.go coverage
- [Issue #1819](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1819) — wsauth_middleware coverage
