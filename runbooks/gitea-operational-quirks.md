# Gitea Actions operational quirks (molecule-core)

Documents persistent operational findings about Gitea Actions runner behaviour
that differ from GitHub Actions and require workarounds in workflow YAML or
runbooks.

> Last updated: 2026-05-12 (infra-runtime-be-agent)

---

## Quirk #1 — Large repo causes fetch timeout on Gitea Actions runner

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

## Quirk #2 — `continue-on-error` only works at step level, not job level

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

- Quirk #10 (this document): Gitea does NOT auto-populate `secrets.GITHUB_TOKEN`
- PR #441: fix applied to `harness-replays.yml`

---

## Quirk #3 — `workflow_dispatch.inputs` not supported

Gitea 1.22.6 parser rejects `workflow_dispatch.inputs`. Drop from all workflow
YAML files ported from GitHub Actions. Manual triggers should use
`workflow_dispatch` without `inputs:`.

**Reference**: `feedback_gitea_workflow_dispatch_inputs_unsupported`

---

## Quirk #4 — `merge_group` not supported

Gitea has no merge queue concept. Drop `merge_group:` triggers from all
workflow YAML files.

---

## Quirk #5 — `environment:` blocks not supported

Gitea has no environments concept. Drop `environment:` from all workflow YAML
files. Secrets and variables are repo-level.

---

## Quirk #6 — Gitea combined status reports `failure` when all contexts are `null`

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

---

## Quirk #7 — TBD

*[Placeholder — document here when a new Gitea Actions quirk is discovered.]*

### Finding

*[What Gitea Actions does differently from GitHub Actions.]*

### Impact

*[Which workflows or operations are affected.]*

### Workaround

*[How to work around this quirk.]*

### References

- internal#[N]: first observation

---

## Quirk #8 — TBD

*[Placeholder — document here when a new Gitea Actions quirk is discovered.]*

### Finding

*[What Gitea Actions does differently from GitHub Actions.]*

### Impact

*[Which workflows or operations are affected.]*

### Workaround

*[How to work around this quirk.]*

### References

- internal#[N]: first observation

---

## Quirk #9 — TBD

*[Placeholder — document here when a new Gitea Actions quirk is discovered.]*

### Finding

*[What Gitea Actions does differently from GitHub Actions.]*

### Impact

*[Which workflows or operations are affected.]*

### Workaround

*[How to work around this quirk.]*

### References

- internal#[N]: first observation

---

## Quirk #10 — Gitea does NOT auto-populate `secrets.GITHUB_TOKEN`

### Finding

Gitea Actions (1.22.6) does **not** auto-populate `secrets.GITHUB_TOKEN`
the way GitHub Actions does. A workflow that references `secrets.GITHUB_TOKEN`
without explicitly provisioning a named secret gets an empty string — not a
read-only token scoped to the repo.

### Impact

Workflows that call the Gitea REST API using `secrets.GITHUB_TOKEN` as auth
receive **HTTP 401** on every API call. Affected workflows in molecule-core:

| Workflow | Symptom | Workaround |
|---|---|---|
| `gate-check-v3.yml` | Reports BLOCKED on every PR | Provision `SOP_TIER_CHECK_TOKEN`; update workflow to use it |
| `qa-review.yml` | Fails immediately on PR open | Same — needs named secret |
| `security-review.yml` | Fails immediately on PR open | Same — needs named secret |

### How to diagnose

Add a debug step to the failing workflow:

```yaml
- name: Diagnose token
  run: |
    echo "Token present: ${{ secrets.GITHUB_TOKEN != '' }}"
    curl -sS --fail -H "Authorization: token ${{ secrets.GITHUB_TOKEN }}" \
      "$GITHUB_SERVER_URL/api/v1/user" | jq -r '.login'
    # Expected (GitHub): prints your username.
    # Actual (Gitea): HTTP 401 or empty string.
```

### References

- internal#325: root-cause analysis and token provisioning
- `feedback_gitea_no_auto_supplied_github_token`

---

## Quirk #11 — PR-create event dispatcher races — only 1 of N workflows fires on `pull_request opened`

### Finding

When a PR is created via the Gitea web UI or API, the Gitea Actions event
dispatcher may fire **only 1 of N eligible workflows** on the initial
`pull_request opened` event. All other eligible workflows are silently dropped.

This was observed on molecule-core PR #558 (created 2026-05-11T19:54:10Z):
12+ workflows had no `paths:` filter and should have fired, but only
`sop-tier-check.yml` dispatched.

Concurrent PRs created within the same minute received 12–30 dispatches each,
confirming this is specific to the PR-create event dispatch, not a general
runner capacity issue.

### Impact

- PRs may not run the full CI suite on first open.
- `gate-check-v3`, `secret-scan`, `qa-review`, and `security-review` can be
  silently absent from the PR's status checks.
- Branch protection may block merge even though CI is effectively green.

### How to diagnose

```bash
# List workflow runs for the PR:
gh run list --event pull_request --repo molecule-ai/molecule-core \
  | grep "$(gh pr view $PR --json number --jq '.number')"

# Expected: 12+ runs on PR open.
# Actual (when race fires): only 1 run.
```

### Workaround

Force a second dispatch by pushing a no-op synchronize commit:

```bash
git commit --allow-empty -m "chore: trigger workflows [skip ci]"
git push
```

The synchronize event fires a second `pull_request` event, which reliably
triggers all eligible workflows.

### References

- internal#329: first observation on PR #558
- `feedback_gitea_pr_create_dispatcher_race`

---

## When you find a new quirk

Copy the template below, increment the quirk number, and fill in the finding,
impact, workaround, and references. Place the new section in the **correct
numerical position** (before the next higher-numbered quirk). Update this
section's final paragraph to remove the next slot's number.

### Template

```markdown
## Quirk #N — <short title>

### Finding

<What Gitea Actions does differently from GitHub Actions.>

### Impact

<Which workflows or operations are affected. Include an affected workflows
table if more than one is affected.>

### How to diagnose

<Shell commands or API calls that confirm this is the quirk, not a real failure.>

### Workaround

<How to work around this quirk in workflow YAML or operations.>

### References

- internal#[N]: first observation
- <Any Gitea issue, feedback label, or upstream bug tracker reference>
```

---

## Open questions for Gitea 1.23

- [ ] **act_runner concurrent-job cap**: issue #305 — runner saturation under
  merge burst; needs `max_concurrent_jobs` cap configured on act_runner
- [ ] **Infisical→Gitea secret-sync**: issue #307 — eliminate manual secret
  PUTs by wiring an Infisical cron to the Gitea API
- [ ] **PR-create dispatcher race resolution**: internal #329 — is there a
  Gitea fix or config knob to disable the race? File upstream bug if not
- [ ] **GITHUB_TOKEN auto-population**: internal #325 — is this on the
  Gitea 1.23 roadmap? If not, the workaround (named secret) is the permanent
  answer

