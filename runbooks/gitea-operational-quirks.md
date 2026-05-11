# Gitea Actions operational quirks (molecule-core)

Documents persistent operational findings about Gitea Actions runner behaviour
that differ from GitHub Actions and require workarounds in workflow YAML or
runbooks.

> Last updated: 2026-05-11 (core-devops-agent)

---

## Large repo causes fetch timeout on Gitea Actions runner

### Finding

The Gitea Actions runner (container on host `5.78.80.188`) can reach the git
remote (`https://git.moleculesai.app`) over HTTPS — a single-commit shallow
fetch (`--depth=1`) succeeds in ~16 s. However, fetching the **full compressed
repo history** (~75+ MB) exceeds the runner's network timeout window (~15 s).

This is **not a Gitea Actions bug** and **not a network isolation policy** —
it is a repo-size constraint. The runner can reach external hosts (GitHub,
Docker Hub, PyPI) without issue.

### Impact

Workflows that rely on `actions/checkout` with `fetch-depth: 0` (full history)
or `git clone` will time out.

Specifically:
- `actions/checkout@v*` with `fetch-depth: 0` hangs (fetching full repo
  history takes >15 s before hitting the timeout).
- `git clone <url>` hangs for the same reason.
- `git fetch origin <ref> --depth=1` **succeeds** in ~16 s — this is the
  working pattern.

### Affected workflows

| Workflow | Issue | Workaround |
|---|---|---|
| `harness-replays.yml` detect-changes job | `fetch-depth: 0` + `git clone` time out | Added `timeout 20 git fetch origin base.ref --depth=1` + `continue-on-error: true` + fallback to `run=true` per PR #441 |
| `publish-workspace-server-image.yml` | In-image `git clone` of workspace templates | Pre-clone manifest deps before compose build (Task #173 pattern) |
| Any workflow using `fetch-depth: 0` | Full history fetch times out | Use `fetch-depth: 1` + explicit `git fetch` for needed refs |

### How to diagnose

```bash
# From inside the runner (add as a debug step):
timeout 20 git fetch origin main --depth=1
# If this SUCCEEDS (~16s): runner can reach the git remote — the repo is
#   too large for full-history fetch.
# If this times out: true network isolation (unlikely; check firewall rules).
```

### Verification

Confirmed 2026-05-11 by running `timeout 20 git fetch origin base.ref --depth=1`
in the `detect-changes` job of `harness-replays.yml` — **succeeds in ~16 s**.
Runner can reach `https://api.github.com` and `https://pypi.org` without issue,
confirming this is a repo-size constraint, not network isolation.

### References

- PR #441: fix for `harness-replays.yml` detect-changes
- Task #173: pre-clone manifest deps pattern for compose build
- internal#102: tracking customer-private + marketplace third-party repos
- `feedback_oss_first_repo_visibility_default`: 5 workspace-template repos
  flipped public to allow pre-clone without auth

---

## `continue-on-error` only works at step level, not job level

### Finding

Gitea Actions (1.22.6) does not honour `continue-on-error: true` at the **job**
level the way GitHub Actions does. A job with `continue-on-error: true` that
fails still reports `status: failure` in the commit status API.

Only `continue-on-error: true` at the **step** level works as expected.

### Impact

If you want a job to always "pass" in the status API (so dependent jobs can
run and the overall CI does not show `failure`), you must add
`continue-on-error: true` to every step that can fail, AND ensure each step
exits with code 0 (e.g., append `|| true` to commands that might fail).

### Affected workflows

| Workflow | Fix |
|---|---|
| `harness-replays.yml` detect-changes | Added `continue-on-error: true` to fetch step + decide step; added `|| true` to `DIFF=$(git diff ...)` per PR #441 |

### How to diagnose

```yaml
# WRONG — job reports as failure despite flag
jobs:
  my-job:
    continue-on-error: true   # ← ignored by Gitea
    steps:
      - run: git diff ...    # ← if this fails, job = failure
        # job-level flag does not help

# RIGHT — step-level flag prevents step from failing
jobs:
  my-job:
    steps:
      - run: git diff ... || true  # ← step exits 0
        continue-on-error: true     # ← belt and suspenders
```

### References

- Gitea Actions quirk #10 (from migration checklist)
- PR #441: fix applied to `harness-replays.yml`

---

## `workflow_dispatch.inputs` not supported

Gitea 1.22.6 parser rejects `workflow_dispatch.inputs`. Drop from all workflow
YAML files ported from GitHub Actions. Manual triggers should use
`workflow_dispatch` without `inputs:`.

**Reference**: `feedback_gitea_workflow_dispatch_inputs_unsupported`

---

## `merge_group` not supported

Gitea has no merge queue concept. Drop `merge_group:` triggers from all
workflow YAML files.

---

## `environment:` blocks not supported

Gitea has no environments concept. Drop `environment:` from all workflow YAML
files. Secrets and variables are repo-level.

---

## Gitea combined status reports `failure` when all contexts are `null`

### Finding

When ALL individual status contexts for a commit have `state: null` (no runner
has reported yet), Gitea reports the combined commit status as `failure`. This
is a Gitea Actions bug — it conflates "no status reported yet" with "failed".

### Impact

- The `main-red-watchdog` workflow opens a `[main-red]` issue for every
  scheduled workflow run where the combined state is `failure` — even when
  the failure is entirely due to Gitea's combined-status bug.
- This causes spurious `[main-red]` issues that waste SRE time investigating
  non-existent failures.
- **This is especially confusing for `schedule:`-only workflows** (canary,
  sweep jobs, synth-E2E): Gitea attributes their scheduled runs to `main`'s
  HEAD commit, so if a scheduled run fires while all contexts are still
  `state: null`, the watchdog opens a `[main-red]` issue on the latest main
  commit even though that commit itself is perfectly fine.

### How to diagnose

Always check the **individual context `state` fields**, not the combined
`state`/`combined_state`. In the `/repos/{org}/{repo}/commits/{sha}/statuses`
API response, look for `"state": null` on every entry — if all are null, the
combined `failure` is Gitea's bug, not a real CI failure.

```json
{
  "combined_state": "failure",   // ← Gitea bug when all are null
  "contexts": [
    { "context": "CI / Lint", "state": null },  // still running
    { "context": "CI / Test", "state": null }   // still running
  ]
}
```

### Affected workflows

All workflows, but especially `schedule:`-only workflows that run on `main`.
The main-red-watchdog (`.gitea/workflows/main-red-watchdog.yml`) is the
primary consumer of combined status and is affected.

### References

- Issue #481: first real-world case of this bug (2026-05-11)
- `feedback_no_such_thing_as_flakes`: watchdog directive
