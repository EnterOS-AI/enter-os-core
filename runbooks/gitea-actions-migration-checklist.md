# Gitea Actions migration checklist (molecule-core)

Created 2026-05-11 as part of **RFC `molecule-ai/internal#219` §1** — the
sweep of `.github/workflows/*.yml` files in `molecule-core` after the
2026-05-06 GitHub → Gitea migration. Documents which workflows were
retired, which were ported, and the reasoning for each.

The sweep used the four-surface audit pattern from saved memory
`feedback_gitea_actions_migration_audit_pattern`:

1. **YAML** — drop `workflow_dispatch.inputs`, `merge_group`,
   `environment:`. Adjust `runs-on:`. Set `env.GITHUB_SERVER_URL`
   per `feedback_act_runner_github_server_url`.
2. **Cache** — verify `actions/cache@v4` / `upload-artifact` pin
   compatibility with Gitea 1.22.x runner.
3. **Token** — auto-injected `GITHUB_TOKEN` works for same-repo
   operations; cross-repo dispatch needs explicit secret.
4. **Docs** — top-of-file "Ported from .github/workflows/X.yml on
   YYYY-MM-DD per RFC internal#219 §1 sweep" comment.

Per RFC §1 contract, all ports land with `continue-on-error: true` on
every job to surface bugs without blocking; a follow-up PR flips
`continue-on-error: false` after triage.

## Category A — already mirrored (deleted .github/ copy)

These workflows had a working `.gitea/workflows/X.yml` twin at the time
of the sweep. The `.github/` copies were silently dead (Gitea Actions
in molecule-core only registers `.gitea/workflows/`) and have been
removed.

| File | .gitea/ twin |
|---|---|
| `publish-runtime.yml` | `.gitea/workflows/publish-runtime.yml` (ported via issue #206) |
| `secret-scan.yml` | `.gitea/workflows/secret-scan.yml` |

## Category B — GitHub-only, retired

These workflows depend on GitHub-specific surface (merge queue, GitHub
auto-merge primitive, github.com REST API, GHCR registry, CodeQL action
that hits api.github.com bundle endpoints) that Gitea does not provide.
No equivalent Gitea-side workflow is needed; the underlying mechanism
either doesn't exist on Gitea or has been replaced by a different
pipeline.

| File | Why retired |
|---|---|
| `auto-tag-runtime.yml` | Superseded by `.gitea/workflows/publish-runtime-autobump.yml` (auto-bump-on-workspace-edit). The autobump only does patch bumps; the deleted workflow supported `release:minor` / `release:major` PR-label-driven bumps. Follow-up issue should track restoring label-driven minor/major if anyone uses it. |
| `branch-protection-drift.yml` | Targets `Molecule-AI/molecule-core` on GitHub via `gh api /repos/.../branch-protection` — entirely GitHub-API specific. `tools/branch-protection/drift_check.sh` and `apply.sh` reference the GitHub schema (status_check_contexts, dismiss_stale_reviews, etc.) which differs from Gitea's `branch_protections` shape. Rebuilding for Gitea is out of scope for the RFC #219 sweep; follow-up issue needed for Gitea-compatible branch-protection drift detection. |
| `check-merge-group-trigger.yml` | The workflow's own header (lines 18-23) documents that it's vacuously satisfied on Gitea — Gitea has no merge queue, no `merge_group:` event type, no `gh-readonly-queue/...` refs. Nothing to lint. |
| `codeql.yml` | The workflow's own header (lines 3-67) documents that `github/codeql-action/init@v4` hits api.github.com bundle endpoints not implemented by Gitea (observed: `::error::404 page not found` in Initialize CodeQL step). Per Hongming decision 2026-05-07 (task #156): CodeQL is ADVISORY/non-blocking until a Gitea-compatible SAST pipeline lands. Replacement options (Semgrep self-host, Sonatype, GitHub-mirror-for-SAST) tracked in #156. |
| `pr-guards.yml` | The workflow's own header documents that Gitea has no `gh pr merge --auto` primitive — the guard is a structural no-op on Gitea. Branch protection on `main` does NOT reference any `pr-guards` check name; deletion is safe. |
| `promote-latest.yml` | Uses `imjasonh/setup-crane` against `ghcr.io/molecule-ai/platform` — the GHCR registry was retired during the 2026-05-06 Gitea migration (per `canary-verify.yml` header notes, the canonical tenant image moved to ECR `153263036946.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/platform-tenant`). The workflow can no longer find any image to retag. Follow-up issue suggested if an ECR-based retag promote is desired. |

## Category C — ported to .gitea/

These workflows had real ongoing CI value but no Gitea-side equivalent.
Each was ported to `.gitea/workflows/X.yml` with:

- `workflow_dispatch.inputs` removed (Gitea 1.22.6 parser rejects them —
  per `feedback_gitea_workflow_dispatch_inputs_unsupported`)
- `merge_group:` trigger removed (no merge queue)
- `environment:` blocks removed (Gitea has no environments)
- `dorny/paths-filter@v4` replaced with inline `git diff` (per the
  pattern established in PR#372 ci.yml port)
- `env.GITHUB_SERVER_URL: https://git.moleculesai.app` set at workflow
  level (belt-and-suspenders for `actions/checkout` etc.)
- `continue-on-error: true` on every job (RFC §1 contract — surface
  defects without blocking; follow-up PR flips after triage)
- Top-of-file header: "Ported from .github/workflows/X.yml on
  YYYY-MM-DD per RFC internal#219 §1 sweep."

See the C-1 / C-2 / C-3 sweep PRs for the file lists and per-file
adjustments.

## Category D — parser-rejected (none for molecule-core)

The RFC #219 §1 brief lists 7 workflows as parser-rejected (`audit-orphan-instances`,
`bake-thin-ami`, `bench-provision-time`, `cache-probe`, `deploy-pipeline`,
`e2e-tunnel-reboot`, `persona-author-check`). Verification against
molecule-core's tree (and the `docker logs molecule-gitea-1` parser-rejection
log) shows these workflows belong to other repos:

- `audit-orphan-instances`, `bake-thin-ami`, `bench-provision-time`,
  `deploy-pipeline`, `e2e-tunnel-reboot` live in `molecule-ai/molecule-controlplane`
- `cache-probe`, `persona-author-check` live in `molecule-ai/internal`

For molecule-core, **Category D is empty**.

## Verification

After all sweep PRs land:

```bash
# Should produce nothing.
ls .github/workflows/*.yml | grep -vF ci.yml

# Should list 6 working workflows from the .gitea/ port directory + the
# C-1/C-2/C-3 ports.
ls .gitea/workflows/*.yml
```

Gitea Actions server should produce NO `[W] ignore invalid workflow`
lines for any `.gitea/workflows/X.yml` in molecule-core when commits
land on `main`:

```bash
ssh root@5.78.80.188 'docker logs molecule-gitea-1 --since 10m 2>&1 \
  | grep "ignore invalid workflow" \
  | grep -i molecule-core'
# Expected: empty.
```
