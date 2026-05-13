# Gitea Merge Queue

Gitea 1.22.6 does not provide a real merge queue. Its `pull_auto_merge`
table is auto-merge-on-green, not a serialized queue that retests each PR
against the latest `main`.

`gitea-merge-queue` is the external queue for `molecule-core`.

## Queue Contract

Add the `merge-queue` label to an open PR when it is ready to merge.

The bot processes one PR per tick:

1. Confirms `main` is green.
2. Selects the oldest open PR carrying `merge-queue`.
3. Skips PRs with `merge-queue-hold`.
4. Rejects fork PRs because the queue may only update same-repo branches.
5. If the PR head does not contain current `main`, calls Gitea's
   `/pulls/{n}/update?style=merge` endpoint and waits for CI on the new head.
6. Merges only after the current PR head has required contexts green:
   - `CI / all-required (pull_request)`
   - `sop-checklist / all-items-acked (pull_request)`

The workflow is serialized with `concurrency`, so two queued PRs cannot be
merged against the same observed `main`.

## Operator Commands

Queue a PR:

```bash
curl -fsS -X POST \
  -H "Authorization: token $GITEA_TOKEN" \
  -H "Content-Type: application/json" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/issues/<PR>/labels" \
  -d '{"labels":["merge-queue"]}'
```

Temporarily hold a queued PR:

```bash
curl -fsS -X POST \
  -H "Authorization: token $GITEA_TOKEN" \
  -H "Content-Type: application/json" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/issues/<PR>/labels" \
  -d '{"labels":["merge-queue-hold"]}'
```

Run the bot manually from a trusted checkout:

```bash
GITEA_TOKEN="$DEVOPS_ENGINEER_TOKEN" \
GITEA_HOST=git.moleculesai.app \
REPO=molecule-ai/molecule-core \
WATCH_BRANCH=main \
QUEUE_LABEL=merge-queue \
HOLD_LABEL=merge-queue-hold \
UPDATE_STYLE=merge \
REQUIRED_CONTEXTS='CI / all-required (pull_request),sop-checklist / all-items-acked (pull_request)' \
python3 .gitea/scripts/gitea-merge-queue.py
```

Dry run:

```bash
python3 .gitea/scripts/gitea-merge-queue.py --dry-run
```

## Branch Protection

`main` should keep direct merges restricted to the non-bypass merge actor
used by the queue. Normal humans and agents should not merge directly.

`block_on_outdated_branch` should be enabled as a defense in depth, but it
does not replace the queue. The queue still performs its own current-main
check immediately before merge because branch protection alone cannot
serialize two already-green PRs.

## Failure Handling

If `main` is not green, the queue pauses and does not merge anything.

If a queued PR is stale, the queue updates the PR branch and comments on the
PR. It does not merge until CI runs on the updated head.

If the queue workflow fails, treat it as a CI/CD incident. Do not bypass by
manually merging unless the human operator explicitly accepts the risk.
