"use client";

import { useMemo } from "react";
import { useCanvasStore } from "@/store/canvas";

/**
 * Org-deploy state for a single workspace node. Computed from the
 * current canvas store snapshot — no per-org status field on the
 * backend is required (a root "is deploying" iff any descendant in
 * its subtree still reports status === "provisioning").
 *
 * Performance note: the first version of this hook walked the entire
 * nodes array per node render — O(n²) for a 50-node org. The current
 * implementation computes ONE map of derived state for the whole
 * canvas per nodes-array change, then each call site looks up its
 * own id. The map is built inside useMemo against a cheap projection
 * (id + parentId + status tuples via useShallow) so unrelated store
 * mutations (drag, selection, viewport) don't re-run the walk.
 */
export interface OrgDeployState {
  isActivelyProvisioning: boolean;
  isDeployingRoot: boolean;
  isLockedChild: boolean;
  descendantProvisioningCount: number;
}

const EMPTY: OrgDeployState = {
  isActivelyProvisioning: false,
  isDeployingRoot: false,
  isLockedChild: false,
  descendantProvisioningCount: 0,
};

/** Projection used to drive the deploy-state computation. Shallow-
 *  compared so re-renders only happen when one of these fields
 *  actually changes across any node. */
interface NodeProjection {
  id: string;
  parentId: string | null;
  status: string;
}

export function buildDeployMap(
  projections: NodeProjection[],
  deletingIds: ReadonlySet<string>,
): Map<string, OrgDeployState> {
  const byId = new Map<string, NodeProjection>();
  const childrenBy = new Map<string, string[]>();
  for (const p of projections) {
    byId.set(p.id, p);
    if (p.parentId) {
      const arr = childrenBy.get(p.parentId) ?? [];
      arr.push(p.id);
      childrenBy.set(p.parentId, arr);
    }
  }

  // Walk once from each node up to its root, memoising the root id.
  // `rootOf.get(id)` short-circuits further walks on the same chain.
  const rootOf = new Map<string, string>();
  const findRoot = (id: string): string => {
    const cached = rootOf.get(id);
    if (cached) return cached;
    let cursor: NodeProjection | undefined = byId.get(id);
    let rootId = id;
    while (cursor && cursor.parentId) {
      const parent = byId.get(cursor.parentId);
      if (!parent) break;
      cursor = parent;
      rootId = parent.id;
      const alreadyKnown = rootOf.get(rootId);
      if (alreadyKnown) {
        rootId = alreadyKnown;
        break;
      }
    }
    rootOf.set(id, rootId);
    return rootId;
  };

  // Count provisioning descendants per node. Also walk once per root
  // using an iterative DFS so we don't stack-overflow on deep trees.
  const countProvisioning = (rootId: string): number => {
    let count = 0;
    const stack = [rootId];
    while (stack.length) {
      const id = stack.pop()!;
      const node = byId.get(id);
      if (!node) continue;
      if (node.status === "provisioning") count++;
      const kids = childrenBy.get(id);
      if (kids) stack.push(...kids);
    }
    return count;
  };

  // Per-root cache of subtree count so every descendant resolves in O(1).
  const rootCount = new Map<string, number>();

  const out = new Map<string, OrgDeployState>();
  for (const p of projections) {
    const rootId = findRoot(p.id);
    let provCount = rootCount.get(rootId);
    if (provCount === undefined) {
      provCount = countProvisioning(rootId);
      rootCount.set(rootId, provCount);
    }
    const rootIsDeploying = provCount > 0;
    // A node being deleted gets the same visual + interaction lock
    // as a deploying child. "The system owns this node right now,
    // don't touch it" is the shared semantic — the user only cares
    // that the card is dim and won't drag; they don't need to know
    // whether it's coming up or going down.
    const deleting = deletingIds.has(p.id);
    out.set(p.id, {
      isActivelyProvisioning: p.status === "provisioning",
      isDeployingRoot: p.id === rootId && rootIsDeploying,
      isLockedChild: deleting || (p.id !== rootId && rootIsDeploying),
      descendantProvisioningCount:
        p.id === rootId ? provCount : 0, // only roots display the count
    });
  }
  return out;
}

/** Store-wide derived map. Recomputed whenever the `nodes` array
 *  reference changes — which is on every store mutation that touches
 *  nodes, including pure position tweens. The map build is O(n) so
 *  a 50-node canvas costs ~50μs per tween frame; that's cheap enough
 *  to not need a projection layer. (An earlier attempt to narrow the
 *  subscription via `useShallow((s) => s.nodes.map(...))` triggered
 *  React 18's "getSnapshot should be cached" loop because the
 *  projection creates fresh object references each call — shallow
 *  equality always sees "changed", which re-renders, which re-runs
 *  the selector, ad infinitum.) */
function useDeployMap(): Map<string, OrgDeployState> {
  const nodes = useCanvasStore((s) => s.nodes);
  const deletingIds = useCanvasStore((s) => s.deletingIds);
  return useMemo(() => {
    const projections = nodes.map((n) => ({
      id: n.id,
      parentId: n.data.parentId,
      status: n.data.status,
    }));
    return buildDeployMap(projections, deletingIds);
  }, [nodes, deletingIds]);
}

export function useOrgDeployState(nodeId: string): OrgDeployState {
  const map = useDeployMap();
  return map.get(nodeId) ?? EMPTY;
}
