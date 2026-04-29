# scripts/

Operational and one-off scripts for molecule-core. Most are
self-documenting — see the header comments in each file.

## RFC #2251 coordinator task-bound harnesses

There are three related scripts; pick the right one:

| Script | Purpose | Targets |
|---|---|---|
| `measure-coordinator-task-bounds.sh` | **Canonical** v1 harness for the RFC #2251 / Issue 4 reproduction. Provisions a PM coordinator + Researcher child via `claude-code-default` + `langgraph` templates, sends a synthesis-heavy A2A kickoff, observes elapsed time + heartbeat trace. | OSS-shape platform — localhost or any `/workspaces`-shaped endpoint. Has tenant/admin-token guards for non-localhost runs. |
| `measure-coordinator-task-bounds-runner.sh` | Generalised runner for the same measurement contract but with **arbitrary template + secret + model combinations** (Hermes/MiniMax, etc.). Useful for cross-runtime variants without modifying the canonical harness. | Same as above (local or SaaS via `MODE=saas`). |
| `measure-coordinator-task-bounds.sh` (in [molecule-controlplane](https://github.com/Molecule-AI/molecule-controlplane)) | **Production-shape** variant that bootstraps a real staging tenant via `POST /cp/admin/orgs`, then runs the same measurement against `<slug>.staging.moleculesai.app`. | Staging controlplane only — refuses to run against production. |

See `reference_harness_pair_pattern` (auto-memory) for when to use which
and the cross-repo design rationale.

### Common safety pattern across all three

- **Cleanup trap** on EXIT/INT/TERM auto-deletes provisioned resources.
- **`DRY_RUN=1`** prints plan + auth fingerprint, exits before any
  state mutation. Run this before pointing at staging or any shared
  infrastructure.
- **Non-target guard** refuses arbitrary endpoints (the controlplane
  variant is locked to `staging-api.moleculesai.app`; the OSS variant
  requires explicit auth + tenant scoping for non-localhost PLATFORM).
- **Cleanup failures emit `cleanup_*_failed` events** with remediation
  hints; no silenced curl. ADMIN_TOKEN expiring mid-run surfaces as a
  structured event rather than a silent leak.

### Heartbeat trace caveat

If `heartbeat_trace.raw == "<endpoint_unavailable>"`, the per-workspace
`/heartbeat-history` endpoint isn't wired on the target build — the
bound measurement is INCONCLUSIVE on the platform-ceiling question.
Either wire the endpoint or replace with the equivalent Datadog query.

## Other scripts

- `cleanup-rogue-workspaces.sh` — emergency teardown for leaked
  workspaces. Prompts for confirmation. Pair with the harnesses if a
  cleanup trap fails (see `cleanup_*_failed` events).
- `canary-smoke.sh` — quick smoke test for canary releases.
- `dev-start.sh` — local-dev platform bring-up.

The rest are self-documenting in their header comments.
