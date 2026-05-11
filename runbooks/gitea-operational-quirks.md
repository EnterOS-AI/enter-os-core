# Gitea Actions operational quirks (molecule-core)

Documents persistent operational findings about Gitea Actions runner behaviour
that differ from GitHub Actions and require workarounds in workflow YAML or
runbooks.

> Last updated: 2026-05-11 (core-devops-agent)

---

## Gitea 1.22.6 runner network isolation

### Finding

The Gitea Actions runner (container on host `5.78.80.188`) cannot reach the
git remote (`https://git.moleculesai.app`) over HTTPS from inside the runner
container. Any `git fetch`, `git clone`, or `git push` command that contacts
the remote times out at 12–15 s.

This is **not a Gitea Actions bug** — it is an operator-level network policy
where the runner container's network namespace is restricted from reaching the
Gitea host HTTPS endpoint. The runner can reach external hosts (GitHub,
Docker Hub, PyPI) normally.

### Impact

Workflows that rely on `git fetch origin <ref>` or `actions/checkout` with
`fetch-depth: 0` (full history) will hang or time out.

Specifically:
- `actions/checkout@v*` with `fetch-depth: 0` hangs (fetching full repo
  history takes >30 s before hitting the timeout).
- `git fetch origin main --depth=1` times out at ~15 s.
- `git clone <url>` times out at ~15 s.

### Affected workflows

| Workflow | Issue | Workaround |
|---|---|---|
| `harness-replays.yml` detect-changes job | `git fetch origin main --depth=1` times out | Added `timeout 20` + graceful fallback to `run=true` (always run harness) per PR #441 |
| `publish-workspace-server-image.yml` | In-image `git clone` of workspace templates | Pre-clone manifest deps before compose build (Task #173 pattern) |
| Any workflow using `fetch-depth: 0` | Full history fetch times out | Use `fetch-depth: 1` + explicit `git fetch` for needed refs |

### How to diagnose

```bash
# From inside the runner (add as a debug step):
timeout 20 git fetch origin main --depth=1
# If this times out: runner cannot reach git remote
```

### Verification

Confirmed 2026-05-11 by running `timeout 20 git fetch origin main --depth=1`
in the `detect-changes` job of `harness-replays.yml` — consistently times
out at 15 s. Runner can reach `https://api.github.com` and `https://pypi.org`
without issue.

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

## `fetch-depth: 0` on `actions/checkout` times out

`actions/checkout` with `fetch-depth: 0` triggers a full repo history fetch
which exceeds the runner's network timeout to the git remote (~15 s).

**Workaround**: Use `fetch-depth: 1` (default) and add explicit
`git fetch origin <ref> --depth=1` for any additional refs needed.

**Reference**: PR #441 detect-changes fetch step.
