# Memory Plugin Contract — Changelog

Every breaking or operationally-relevant change to the v1 plugin
contract or the workspace-server-side wiring lands here. Plugin
authors should subscribe to PRs touching this file.

## [Unreleased] — fixup wave 1 (post-RFC-#2728 self-review)

A self-review of the initial 11-PR rollout (PRs #2729-#2742) flagged
two correctness bugs and three operational hazards. This wave fixes
all of them. Order matches operator-impact severity.

### Critical: backfill idempotency via `MemoryWrite.id` (#2744)

**The bug.** The backfill CLI claimed idempotent on re-run, but
`gen_random_uuid()` in the plugin's INSERT meant every retry created
a fresh row. Operators retrying a failed `-apply` would silently
double their memory count.

**The fix.** Optional `id` field on `MemoryWrite`. When supplied,
plugins MUST upsert. The backfill now forwards `agent_memories.id`
to `MemoryWrite.id`, so retries update in place.

**Plugin author action.** If your plugin uses
`INSERT INTO ... DEFAULT gen_random_uuid()`, switch to
`INSERT ... ON CONFLICT (id) DO UPDATE` when `id` is set. The wire
contract is forward-compatible — plugins that ignore the field still
work for production agent commits (which leave `id` empty), but they
will silently corrupt backfill retries.

### Critical: `memory-backfill -verify` mode (#2747)

**The miss.** The original PR-7 task spec called for a parity-check
mode but it never landed. Operators had no way to confirm a
migration succeeded short of "no errors logged."

**The fix.** New `-verify` flag samples N workspaces, queries
`agent_memories` direct, runs an equivalent plugin search via the
namespace resolver, multiset-compares contents. Reports mismatches
to stdout and exits non-zero so CI can gate the cutover.

```bash
memory-backfill -verify                        # default sample 50
memory-backfill -verify -verify-sample=200     # bigger
memory-backfill -verify -workspace=<uuid>      # one workspace
```

### Important: `expires_at` validation (#2746)

**The bug.** `commit_memory_v2` silently dropped malformed
`expires_at` strings. Agent passes `expires_at: "tomorrow"`, gets a
200, memory has no TTL — agent thinks it set a TTL, didn't.

**The fix.** Returns
`fmt.Errorf("invalid expires_at: must be RFC3339")` on parse
failure. Plugin is not called in this case.

**Plugin author action.** None — this is a workspace-server-side
fix. But: if your plugin advertises the `ttl` capability, make sure
you actually evict expired rows on read (not just on a janitor cron
that runs once a day). The harness in `testing-your-plugin.md` has
a TTL-eviction test you should run.

### Important: audit log JSON via `json.Marshal` (#2746)

**The bug.** `auditOrgWrite` built `activity_logs.metadata` via
`fmt.Sprintf` with `%q`. For ASCII (today's UUID + hex digest) this
coincidentally produces valid JSON; for unicode or control bytes it
silently produces non-JSON.

**The fix.** Replaced with `json.Marshal(map[string]string{...})`.
Same wire shape today, won't regress when metadata grows.

**Plugin author action.** None — workspace-server-internal.

### Operator action: staging verification (#292)

**Status.** Tracked as task #292. PR-merged ≠ verified. Operator
must:
1. Provision a staging tenant, set `MEMORY_PLUGIN_URL`
2. Run real `commit_memory_v2` from a workspace
3. `memory-backfill -dry-run` against staging data
4. `memory-backfill -apply`, then `-verify`
5. Set `MEMORY_V2_CUTOVER=true`, verify admin export still works
6. Run a legacy `commit_memory` from a workspace, verify it lands
   in plugin storage via the PR-6 shim

### Other follow-ups still open

- **#289**: admin export O(workspaces) → O(namespaces) — N+1 pattern
  in `exportViaPlugin` (1000-workspace tenants run 1000× resolver
  CTEs + 1000× plugin searches today).
- **#291**: workspace deletion must call `DELETE
  /v1/namespaces/{name}` — orphans accumulate today.
- **#293**: real-subprocess boot E2E — current PR-11 is integration
  (httptest + sqlmock), not E2E.

These are tracked but deferred; they're operationally annoying, not
incident-shaped.

## [v1.0.0] — initial release (RFC #2728, PRs #2729-#2742)

Initial plugin contract + 11-PR rollout. See
[issue #2728](https://git.moleculesai.app/molecule-ai/molecule-core/issues/2728)
for the full RFC.

Endpoints: `/v1/health`, `/v1/namespaces/{name}` (PUT/PATCH/DELETE),
`/v1/namespaces/{name}/memories` (POST), `/v1/search` (POST),
`/v1/memories/{id}` (DELETE).

Capabilities: `embedding`, `fts`, `ttl`, `pin`, `propagation`.

Operator runbook: see [README.md § Replacing the built-in plugin](README.md#replacing-the-built-in-plugin).
