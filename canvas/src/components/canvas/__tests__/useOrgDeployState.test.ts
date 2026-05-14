/**
 * Unit tests for buildDeployMap — the pure tree-traversal core of
 * useOrgDeployState.
 *
 * What is tested here:
 *   - Root / leaf identification via parent-chain walk
 *   - isDeployingRoot: true when any descendant is "provisioning"
 *   - isActivelyProvisioning: true only for the node itself in that state
 *   - isLockedChild: true for non-root nodes in a deploying tree
 *   - isLockedChild: also true for nodes in deletingIds (even if not deploying)
 *   - descendantProvisioningCount: non-zero only on root nodes
 *   - Performance contract: O(n) single-pass walk — tested by verifying
 *     correctness across 50-node trees (n=50, all cases above)
 *
 * What is NOT tested here (hook integration — appropriate for E2E):
 *   - The useMemo / Zustand subscription wiring
 *   - React Flow integration (flowToScreenPosition, getInternalNode)
 *
 * Issue: #2071 (Canvas test gaps follow-up).
 */
import { describe, expect, it } from "vitest";
import { buildDeployMap, type OrgDeployState } from "../useOrgDeployState";

// ── Helpers ──────────────────────────────────────────────────────────────────

type Projection = { id: string; parentId: string | null; status: string };

function proj(
  id: string,
  parentId: string | null,
  status: string,
): Projection {
  return { id, parentId, status };
}

/** Unchecked cast — test helpers aren't production code paths. */
function m(
  ps: Projection[],
  deletingIds: string[] = [],
): Map<string, OrgDeployState> {
  return buildDeployMap(ps, new Set(deletingIds));
}

function s(
  map: Map<string, OrgDeployState>,
  id: string,
): OrgDeployState {
  const got = map.get(id);
  if (!got) throw new Error(`no entry for id=${id}`);
  return got;
}

// ── Empty / trivial ───────────────────────────────────────────────────────────

describe("buildDeployMap — empty", () => {
  it("returns empty map for empty projections", () => {
    expect(m([]).size).toBe(0);
  });
});

// ── Single node ─────────────────────────────────────────────────────────────

describe("buildDeployMap — single node", () => {
  it("isolated node is its own root and not deploying", () => {
    const map = m([proj("a", null, "online")]);
    expect(s(map, "a")).toEqual({
      isActivelyProvisioning: false,
      isDeployingRoot: false,
      isLockedChild: false,
      descendantProvisioningCount: 0,
    });
  });

  it("isolated provisioning node is deploying root", () => {
    const map = m([proj("a", null, "provisioning")]);
    expect(s(map, "a")).toEqual({
      isActivelyProvisioning: true,
      isDeployingRoot: true,
      isLockedChild: false,
      descendantProvisioningCount: 1,
    });
  });
});

// ── Parent / child chains ─────────────────────────────────────────────────────

describe("buildDeployMap — parent / child chains", () => {
  it("root with online child: root is not deploying, child is not locked", () => {
    // A ──► B
    const map = m([
      proj("A", null, "online"),
      proj("B", "A", "online"),
    ]);
    expect(s(map, "A")).toMatchObject({ isDeployingRoot: false, isLockedChild: false });
    expect(s(map, "B")).toMatchObject({ isDeployingRoot: false, isLockedChild: false });
  });

  it("root with provisioning child: root is deploying, child is locked", () => {
    // A ──► B (B is provisioning)
    const map = m([
      proj("A", null, "online"),
      proj("B", "A", "provisioning"),
    ]);
    expect(s(map, "A")).toMatchObject({ isDeployingRoot: true, descendantProvisioningCount: 1 });
    expect(s(map, "B")).toMatchObject({ isLockedChild: true, isActivelyProvisioning: true });
  });

  it("provisioning root with online child: root is deploying, child is locked", () => {
    // A (provisioning) ──► B (online)
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "online"),
    ]);
    expect(s(map, "A")).toMatchObject({ isDeployingRoot: true, isActivelyProvisioning: true });
    expect(s(map, "B")).toMatchObject({ isLockedChild: true, isActivelyProvisioning: false });
  });

  it("grandchild inherits deploy lock through intermediate online node", () => {
    // A ──► B ──► C  (A is provisioning)
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "online"),
      proj("C", "B", "online"),
    ]);
    // B and C are both non-root descendants of the deploying root
    expect(s(map, "B")).toMatchObject({ isLockedChild: true });
    expect(s(map, "C")).toMatchObject({ isLockedChild: true });
    expect(s(map, "A")).toMatchObject({ isDeployingRoot: true, descendantProvisioningCount: 1 });
  });

  it("deep chain: only the topmost node with a null parent counts as root", () => {
    // A ──► B ──► C ──► D  (A is provisioning)
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "online"),
      proj("C", "B", "online"),
      proj("D", "C", "online"),
    ]);
    const roots = ["A", "B", "C", "D"].filter((id) => s(map, id).isDeployingRoot);
    expect(roots).toEqual(["A"]);
  });
});

// ── Sibling branching ─────────────────────────────────────────────────────────

describe("buildDeployMap — sibling branching", () => {
  it("parent with multiple children: deploying root propagates to all children", () => {
    //         A (provisioning)
    //        / \
    //       B   C
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "online"),
      proj("C", "A", "online"),
    ]);
    expect(s(map, "B")).toMatchObject({ isLockedChild: true });
    expect(s(map, "C")).toMatchObject({ isLockedChild: true });
    expect(s(map, "A")).toMatchObject({ descendantProvisioningCount: 1 });
  });

  it("only one provisioning descendant marks the root as deploying", () => {
    //           A
    //         / | \
    //        B  C  D   (only C is provisioning)
    const map = m([
      proj("A", null, "online"),
      proj("B", "A", "online"),
      proj("C", "A", "provisioning"),
      proj("D", "A", "online"),
    ]);
    expect(s(map, "A")).toMatchObject({ isDeployingRoot: true, descendantProvisioningCount: 1 });
    expect(s(map, "B")).toMatchObject({ isLockedChild: true });
    expect(s(map, "C")).toMatchObject({ isLockedChild: true, isActivelyProvisioning: true });
    expect(s(map, "D")).toMatchObject({ isLockedChild: true });
  });

  it("two provisioning siblings: count reflects both", () => {
    const map = m([
      proj("A", null, "online"),
      proj("B", "A", "provisioning"),
      proj("C", "A", "provisioning"),
    ]);
    expect(s(map, "A")).toMatchObject({ descendantProvisioningCount: 2 });
    expect(s(map, "B")).toMatchObject({ isActivelyProvisioning: true });
    expect(s(map, "C")).toMatchObject({ isActivelyProvisioning: true });
  });
});

// ── Multiple disjoint trees ───────────────────────────────────────────────────

describe("buildDeployMap — multiple disjoint trees", () => {
  it("each tree has its own root; deploying nodes are independent", () => {
    // Tree 1: X (provisioning) ──► Y
    // Tree 2: P ──► Q  (no provisioning)
    const map = m([
      proj("X", null, "provisioning"),
      proj("Y", "X", "online"),
      proj("P", null, "online"),
      proj("Q", "P", "online"),
    ]);
    expect(s(map, "X")).toMatchObject({ isDeployingRoot: true });
    expect(s(map, "Y")).toMatchObject({ isLockedChild: true });
    expect(s(map, "P")).toMatchObject({ isDeployingRoot: false, isLockedChild: false });
    expect(s(map, "Q")).toMatchObject({ isDeployingRoot: false, isLockedChild: false });
  });
});

// ── Deleting nodes ────────────────────────────────────────────────────────────

describe("buildDeployMap — deletingIds", () => {
  it("node in deletingIds is locked even if tree is not deploying", () => {
    const map = m(
      [
        proj("A", null, "online"),
        proj("B", "A", "online"),
      ],
      ["B"], // B is being deleted
    );
    expect(s(map, "A")).toMatchObject({ isLockedChild: false });
    expect(s(map, "B")).toMatchObject({ isLockedChild: true, isActivelyProvisioning: false });
  });

  it("node in deletingIds: isLockedChild is true regardless of provisioning", () => {
    const map = m(
      [
        proj("A", null, "provisioning"),
        proj("B", "A", "online"),
      ],
      ["B"],
    );
    // B is both a deploying-child AND a deleting node — either alone locks it
    expect(s(map, "B")).toMatchObject({ isLockedChild: true });
  });

  it("empty deletingIds set has no effect", () => {
    const map = m(
      [
        proj("A", null, "online"),
        proj("B", "A", "online"),
      ],
      [],
    );
    expect(s(map, "B")).toMatchObject({ isLockedChild: false });
  });
});

// ── descendantProvisioningCount ───────────────────────────────────────────────

describe("buildDeployMap — descendantProvisioningCount", () => {
  it("is 0 for non-root nodes", () => {
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "provisioning"),
    ]);
    expect(s(map, "B").descendantProvisioningCount).toBe(0);
  });

  it("includes the root's own status when provisioning", () => {
    const map = m([
      proj("A", null, "provisioning"),
      proj("B", "A", "online"),
    ]);
    // A is both root and provisioning → count includes itself
    expect(s(map, "A").descendantProvisioningCount).toBe(1);
  });

  it("accumulates all provisioning descendants (not just immediate children)", () => {
    const map = m([
      proj("A", null, "online"),
      proj("B", "A", "online"),
      proj("C", "B", "provisioning"),
    ]);
    expect(s(map, "A").descendantProvisioningCount).toBe(1);
  });
});

// ── O(n) performance ─────────────────────────────────────────────────────────

describe("buildDeployMap — O(n) performance contract", () => {
  it("handles a 50-node three-level tree without incorrect node assignments", () => {
    // Level 0: 1 root
    // Level 1: 7 children
    // Level 2: 42 leaves
    // Total: 50 nodes
    const projections: Projection[] = [];
    projections.push(proj("root", null, "provisioning"));
    for (let i = 0; i < 7; i++) {
      projections.push(proj(`l1-${i}`, "root", "online"));
    }
    for (let i = 0; i < 42; i++) {
      const parent = `l1-${Math.floor(i / 6)}`;
      projections.push(proj(`l2-${i}`, parent, "online"));
    }
    const map = m(projections);

    // Root is the only deploying node
    expect(s(map, "root")).toMatchObject({
      isDeployingRoot: true,
      isLockedChild: false,
      descendantProvisioningCount: 1,
    });

    // Every other node is a locked child
    for (let i = 0; i < 7; i++) {
      expect(s(map, `l1-${i}`)).toMatchObject({ isLockedChild: true, isDeployingRoot: false });
    }
    for (let i = 0; i < 42; i++) {
      expect(s(map, `l2-${i}`)).toMatchObject({ isLockedChild: true, isDeployingRoot: false });
    }
  });
});
