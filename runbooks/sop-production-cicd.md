# SOP: Production CI/CD Changes

Production CI/CD changes are higher risk than ordinary CI edits. They can publish images, deploy tenants, promote tags, mutate branch protection, or change merge behavior. This SOP separates rules that must be enforced by code from rules that require human judgment.

## Programmatic Gates

The workflow YAML linter is the first line of enforcement:

```bash
python3 .gitea/scripts/lint-workflow-yaml.py --workflow-dir .gitea/workflows
```

It must reject:

- Gitea-hostile syntax such as `workflow_dispatch.inputs`, `workflow_run`, workflow name collisions, slash-containing workflow names, and unsupported cross-repo action references.
- Production deploy workflows that rely on `concurrency.cancel-in-progress: false` for serialization.
- Production deploy workflows that print raw control-plane responses or raw `.error` fields into CI logs.
- Production redeploy workflows with no kill switch or rollback/pin control.

Production deploy helpers must also unit-test:

- Disable-flag parsing.
- Required status context selection.
- Terminal status handling for `failure`, `error`, `cancelled`, `canceled`, and `skipped`.
- Production control-plane URL guards.
- Rollback target/pin handling when applicable.

## Required PR Evidence

Every production CI/CD PR must include concrete answers for:

- Root cause: what production failure mode or process gap is being closed.
- Deploy gate: which exact contexts must be green before production side effects.
- Kill switch: how to stop deployment without reverting the PR.
- Verification: how production state is proven after deployment.
- Logging: proof that CI logs do not contain raw production runtime, SSM, or secret-adjacent output.
- Rollback: the exact command, variable, or workflow to return to a known-good tag/digest.

## Human Review

Production CI/CD PRs need non-author review across these roles:

- DevOps: Gitea Actions semantics, branch protection, merge queue, and runner behavior.
- SRE: rollout order, tenant health checks, observability, and partial-deploy recovery.
- Security: secrets, token scopes, log redaction, and production endpoint targeting.

Critical or Required review findings must be closed with one of:

- A code change plus verification.
- An evidence-backed rejection.
- A follow-up issue only if the finding is explicitly not merge-blocking.

Acknowledgement alone is not closure.

## Production Defaults

Production deploys should fail closed:

- Missing tenant result: fail.
- Tenant unhealthy: fail.
- `/buildinfo` unreachable: fail.
- SHA mismatch: fail.
- Required status cancelled/skipped/missing past timeout: fail.

Staging may tolerate warnings during rollout development; production should not.

## Gitea 1.22.6 Constraints

Do not design production CI/CD around unsupported or unreliable features:

- No `workflow_run`.
- No reliable `workflow_dispatch.inputs`.
- Do not assume `concurrency.cancel-in-progress: false` serializes queued runs.
- Do not rely on a masked aggregate status as the only production deploy gate.

If these constraints change after a Gitea upgrade, update this SOP and the workflow linter in the same PR.
