/**
 * Transient "recently deleted" map keyed by workspace id.
 *
 * `removeSubtree` calls `markDeleted(ids)` on every removal; `hydrate`
 * calls `wasRecentlyDeleted(id)` to filter out incoming workspaces
 * whose ids match a fresh tombstone — prevents an in-flight
 * GET /workspaces from resurrecting just-deleted nodes via hydrate.
 *
 * TTL is shared with the WS-fallback poll cadence so a single
 * round-trip is covered. Module-level (not store state) so it doesn't
 * trigger React Flow re-renders. (#2069)
 */

import { FALLBACK_POLL_MS } from "./socket";

const TOMBSTONE_TTL_MS = FALLBACK_POLL_MS;

const tombstones = new Map<string, number>();

function gcExpired(now: number): void {
  for (const [id, deletedAt] of tombstones) {
    if (now - deletedAt >= TOMBSTONE_TTL_MS) {
      tombstones.delete(id);
    }
  }
}

export function markDeleted(ids: Iterable<string>): void {
  const now = Date.now();
  gcExpired(now);
  for (const id of ids) {
    tombstones.set(id, now);
  }
}

export function wasRecentlyDeleted(id: string): boolean {
  const deletedAt = tombstones.get(id);
  if (deletedAt === undefined) return false;
  if (Date.now() - deletedAt >= TOMBSTONE_TTL_MS) {
    tombstones.delete(id);
    return false;
  }
  return true;
}

/** Test-only: clear the module-level map between tests. Production code
 *  must not call this — the map is intentionally process-lifetime. */
export function __resetTombstonesForTest(): void {
  tombstones.clear();
}

/** Test-only: inspect the current tombstone count. */
export function __tombstoneCountForTest(): number {
  return tombstones.size;
}
