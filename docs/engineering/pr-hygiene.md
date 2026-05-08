# Pull Request Hygiene

**Status:** Guide. Violations are a review-time flag, not a CI gate.
**Audience:** Humans and agents opening PRs in this repo.
**Cross-refs:** [testing-strategy.md](./testing-strategy.md), [backends.md](../architecture/backends.md)

## Why this exists

On 2026-04-23 a backlog audit found **23 open PRs on molecule-core**, of which 8 had accumulated 70-380 files of bloat (+2000/-8000 lines) from stale branch drift. The underlying fix in each was 1-5 files; the rest was merge artifact. Half the PRs were closed that day because they weren't reviewable and the real fix had to be re-extracted onto a clean branch.

This document captures the patterns that avoid that outcome.

## The rules

### 1. Small PRs, single concern

| Change size | Reviewability |
|---|---|
| ≤100 lines | ✅ Good. One sitting. |
| 100-300 lines | ⚠️ Acceptable if genuinely one logical change. |
| 300-1000 lines | 🔴 Too large. Split. |
| 1000+ lines | 🚫 Unreviewable — split before opening. |

**Exception:** complete file deletions and automated refactors where the reviewer only needs to verify intent.

### 2. Branch hygiene — rebase, don't merge-in

When your branch falls behind the base:

**Do:**
```bash
git fetch origin staging
git rebase origin/staging
# resolve conflicts
git push --force-with-lease
```

**Don't:**
```bash
git fetch origin staging
git merge origin/staging  # creates merge commit + pulls ALL of base's files into your diff
```

A merge commit from `origin/staging` brings every base-branch commit into your PR's diff. That's where the 235-file bloat comes from. Once you have it, you can't get rid of it without resetting the branch.

### 3. If your branch has already drifted — cherry-pick onto fresh base

```bash
# Identify your real commits
git log origin/staging..HEAD

# Create a fresh branch off current base
git checkout -b your-branch-clean origin/staging

# Cherry-pick only the commits you actually authored
git cherry-pick abc1234 def5678

# Push and open a new PR; close the old one as "superseded by #N"
git push -u origin your-branch-clean
```

**Don't** try to rebase a drifted branch interactively to remove the base-branch commits. It fights you every merge.

### 4. Target `staging` unless you're doing a staging→main promote

Per branching policy ([feedback memory](../../.claude/projects/-Users-hongming-Documents-GitHub-molecule-monorepo/memory/feedback_no_push_main.md) rule): every change lands on `staging` first. Once validated there, a periodic `chore: sync staging → main` PR promotes the bundle.

Exception: hotfixes that also land on `main` directly with CEO approval.

### 5. Describe the why, not the what

A good PR title:
- `fix(provision): write org_instances row BEFORE readiness check to unblock boot-event auth`

A bad PR title:
- `Update orgs.go`
- `Fix bug`
- `Phase 1`

The body should explain:
- **What's broken / missing** (or what's the opportunity)
- **Why this fix** — especially if there are alternatives you considered
- **What's tested** — which scenarios the test plan covers
- **What's deferred** — if there are follow-ups, file issues and link them

Anti-pattern: `## Summary\n- Fix bug`. That's not a summary; that's a stub.

### 6. Close the loop on review comments

- Comments labeled `Nit:` / `Optional:` / `FYI` can be left for follow-up — but leave a reply acknowledging.
- Critical/required comments need a fix or a justified reply before merge.
- Don't resolve threads without replying — silent resolves read as dismissal.

### 7. CI must be green (or the failure must be acknowledged)

- Never push `--no-verify` unless explicitly requested.
- If a pre-existing failure is blocking merge, document it inline and file a tracking issue — don't silently let it erode the "all green" norm.

## Patterns for specific situations

### Re-targeting an old branch

When a PR was opened weeks ago against `main` but policy now says `staging`:

```bash
git fetch origin staging
git rebase --onto origin/staging old-base HEAD
git push --force-with-lease
# Edit the PR's base branch in GitHub UI
```

### Splitting a large PR

If your PR is already open and the reviewer asks for a split:

1. Identify the cleanest split boundary — usually along file groups or dependency layers.
2. Create two new branches off current staging.
3. Cherry-pick the commits for each concern into its branch.
4. Open two new PRs, close the original as "superseded by #A and #B".

### Marketing / docs-heavy PRs

Marketing content has been moved to an internal repo per commit `93324e7`. If your PR modifies files under `docs/marketing/campaigns/`, `docs/marketing/plans/`, or `docs/marketing/briefs/` (with non-public-facing strategy content):

1. Check if the file still exists on `origin/staging`.
2. If deleted, open the PR in the internal marketing repo instead.
3. Public-facing marketing (blog posts, SEO pages under `docs/blog/`) stays in this repo.

## Signs your PR has a hygiene problem

- **70+ files changed** when your commit message mentions 2-3 files
- **+2000/-3500 lines** but the actual fix is ~100 lines
- **State: DIRTY** in GitHub for >1 day
- Filenames in the diff you don't recognize (someone else's changes in your PR)
- Merge commits in your branch's log named `Merge remote-tracking branch 'origin/staging' into ...`

If you see any of these, don't try to "clean it up in place" — **cherry-pick onto a fresh branch** (rule 3 above).

## Related

- [Issue #1822](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1822) — backend parity drift tracker (example of docs that have to stay current)
- [Postmortem: CP boot-event 401](./postmortem-2026-04-23-boot-event-401.md) — caught before shipping because a reviewer could read the diff
