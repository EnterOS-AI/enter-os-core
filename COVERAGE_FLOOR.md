# Coverage Floor

CI enforces coverage gates on two surfaces — `workspace-server` (Go) and
`workspace/` (Python). All defined in `.github/workflows/ci.yml`.

## Current floors (2026-04-23)

| Gate | Threshold | What fails |
|---|---|---|
| **Total floor** | `25%` | `go tool cover -func` reports total below floor |
| **Critical-path per-file floor** | `10%` | Any non-test source file in a security-critical path with coverage ≤10% |
| **Per-file report** | advisory | Printed in CI log, sorted worst-first, does not fail |

Total floor starts at 25% (unchanged from pre-#1823 to keep this PR strictly
additive). The new protection is the critical-path per-file floor, which
directly closes the gap that prompted the issue. Ratchet plan below begins
the month after to let the team first observe the gate in action.

## Security-critical paths (Gate 2)

Changes to these paths have historically introduced security issues (CWE-22,
CWE-78, KI-005, SSRF) or billing/auth risk. Coverage must not drop to zero.

- `internal/handlers/tokens*`
- `internal/handlers/workspace_provision*`
- `internal/handlers/a2a_proxy*`
- `internal/handlers/registry*`
- `internal/handlers/secrets*`
- `internal/middleware/wsauth*`
- `internal/crypto*`

## Ratchet plan

Floor ratchets upward on a fixed cadence. Any ratchet is a PR — reviewable,
reversible, and creates history. The table below is the intended schedule.

| Date | Total floor | Critical-path floor | Notes |
|---|---|---|---|
| 2026-04-23 | 25% | 10% | Initial gate (this file). |
| 2026-05-23 | 30% | 20% | First ratchet |
| 2026-06-23 | 40% | 30% | |
| 2026-07-23 | 50% | 40% | |
| 2026-08-23 | 55% | 50% | |
| 2026-09-23 | 60% | 60% | |
| 2026-10-23 | 70% | 70% | Target steady-state |

The target end-state matches the per-role QA prompts which specify
"coverage >80% on changed files". CI enforces the floor; reviewers still
enforce the per-PR bar.

## Exceptions

If a critical-path file genuinely cannot have coverage above the floor (e.g.
thin wrapper around a third-party SDK with no branches to test), add an entry
here with:

1. **File**: `internal/handlers/example.go`
2. **Reason**: Why coverage can't hit the floor
3. **Tracking issue**: GitHub issue for the real fix
4. **Expiry**: 14 days from entry date; after expiry either coverage is fixed
   or the issue is closed as "accepted technical debt"

### Active exceptions

*(none — add here if you need to land code that legitimately can't clear the floor)*

## Why this gate exists

Issue #1823: an external audit found critical files at 0% coverage despite
test files existing with hundreds of lines. The existing CI step measured
coverage but didn't enforce a meaningful threshold. Any file could go from
80% → 0% and CI stayed green, because the single gate (total ≥25%) ignored
per-file distribution.

This gate makes "no untested critical paths merged" a mechanical property of
the CI, not a behavioural property of QA agents or individual reviewers —
which is the only way to make it survive fleet outages, agent rotations, or
QA process changes.

## Python (workspace/) — added 2026-05-04 from #2790

The Python side has its own gates in the `python-lint` job:

| Gate | Threshold | Where |
|---|---|---|
| **Total floor** | `86%` | `workspace/pytest.ini` `--cov-fail-under=86` (issue #1817) |
| **Critical-path per-file floor** | `75%` | Inline shell step after the pytest run |

### Critical-path Python files

These handle multi-tenant routing, auth tokens, and inbox dispatch. A
coverage drop here is the same risk shape as a Go-side `tokens*` /
`secrets*` file regressing below 10%.

- `workspace/a2a_mcp_server.py` — MCP dispatcher (PR #2766 / #2771)
- `workspace/mcp_cli.py` — molecule-mcp standalone CLI entry
- `workspace/a2a_tools.py` — workspace-scoped tool implementations
- `workspace/inbox.py` — multi-workspace inbox + per-workspace cursors
- `workspace/platform_auth.py` — per-workspace token resolver

### Why 75% (vs 86% total)

The total floor averages ~6000 lines across `workspace/`. A single MCP
file could drop to ~50% with no CI complaint as long as other modules
compensate. The per-file floor closes that distribution gap. 75% sits
below current actuals (80–96% as of 2026-05-04) — strictly additive,
no existing PR fails.

### Python ratchet plan

| Date | Total | Per-file critical | Notes |
|---|---|---|---|
| 2026-05-04 | 86% | 75% | Initial gate (this file). |
| 2026-06-04 | 86% | 80% | First ratchet — at-floor files must catch up. |
| 2026-07-04 | 88% | 85% | |
| 2026-08-04 | 90% | 90% | Target steady-state. |

### Why this Python gate exists

Issue #2790, after the PR #2766 → PR #2771 cycle. PR #2766 added
multi-workspace routing through `a2a_tools.py` + `a2a_mcp_server.py`,
shipped to main with green CI, but the dispatcher silently dropped a
load-bearing kwarg for 4 of 9 tools — caught only by post-merge code
review. The structural drift gate (`test_dispatcher_schema_drift.py`,
PR #2791) catches the schema↔dispatcher mismatch class; this floor
catches the broader "MCP-critical file regressed" class.
