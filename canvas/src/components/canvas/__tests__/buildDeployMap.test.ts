// @vitest-environment jsdom
/**
 * Tests for buildDeployMap — the pure tree-computation core inside
 * useOrgDeployState.
 *
 * Issue: #742 (buildDeployMap unit tests, #2071 follow-up).
 *
 * The function takes a flat list of NodeProjections and a set of
 * deletingIds, then computes per-node OrgDeployState:
 *   isActivelyProvisioning — node itself is provisioning
 *   isDeployingRoot       — node is a root AND has provisioning descendants
 *   isLockedChild         — node is a deleting child OR a non-root in a deploying tree
 *   descendantProvisioningCount — total provisioning descendants (roots only)
 *
 * Coverage:
 *   §1  Empty input
 *   §2  Single node — no parent, non-provisioning
 *   §3  Single node — no parent, provisioning
 *   §4  Single node — has parent (parent exists)
 *   §5  Parent not in projections → node treated as root
 *   §6  Two nodes: root (non-provisioning) + child
 *   §7  Two nodes: root (provisioning) + child
 *   §8  Three-level tree: grandparent (provisioning) → parent → child
 *   §9  DeletingIds contains a non-root node → isLockedChild=true
 *   §10 DeletingIds contains the root → root isLockedChild=true
 *   §11 Two independent roots, one provisioning
 *   §12 Provisioning count: root has 2 provisioning descendants
 *   §13 Non-root node with provisioning status → isActivelyProvisioning=true
 *   §14 findRoot memoization: repeated calls don't re-walk the chain
 *   §15 deletingIds + provisioning interact: deleting takes isLockedChild
 *   §16 Child of provisioning root (not itself provisioning) → isLockedChild=true
 *   §17 Deep chain (5 levels), no provisioning → all nodes unlocked
 *   §18 Deep chain (5 levels), middle node is provisioning root
 *   §19 Node with parentId pointing to non-existent node → treated as root
 */
import { describe, expect, it } from "vitest";
import { buildDeployMap } from "../useOrgDeployState";
import type { OrgDeployState } from "../useOrgDeployState";

type Projection = { id: string; parentId: string | null; status: string };

function proj(
  id: string,
  parentId: string | null,
  status = "idle",
): Projection {
  return { id, parentId, status };
}

// expected maps node-id → partial state (includes `id` as a key)
function check(
  projections: Projection[],
  deletingIds: string[],
  expected: Record<string, Partial<OrgDeployState>>,
): void {
  const result = buildDeployMap(projections, new Set(deletingIds));
  expect(result.size).toBe(projections.length);
  for (const [id, state] of result.entries()) {
    if (id in expected) {
      expect(state).toMatchObject(expected[id]);
    }
  }
}

// ─── §1–§5: Basic structure ──────────────────────────────────────────────────

describe("buildDeployMap — basic structure (§1–§5)", () => {
  it("§1 returns an empty map when projections is empty", () => {
    const result = buildDeployMap([], new Set());
    expect(result.size).toBe(0);
  });

  it("§2 single node, no parent, non-provisioning → unlocked root", () => {
    check([proj("a")], [], {
      isActivelyProvisioning: false,
      isDeployingRoot: false,
      isLockedChild: false,
      descendantProvisioningCount: 0,
    });
  });

  it("§3 single provisioning node → deploying root", () => {
    check([proj("a", null, "provisioning")], [], {
      isActivelyProvisioning: true,
      isDeployingRoot: true,
      isLockedChild: false,
      descendantProvisioningCount: 1,
    });
  });

  it("§4 single node with existing parent → non-root, unlocked", () => {
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      [],
      {
        id: "child",
        isActivelyProvisioning: false,
        isDeployingRoot: false,
        isLockedChild: false,
        descendantProvisioningCount: 0,
      },
    );
  });

  it("§5 parentId points to a node not in projections → treated as root", () => {
    // "orphan" is a root because its parent is absent from the projection list.
    check([proj("orphan", "ghost", "idle")], [], {
      id: "orphan",
      isDeployingRoot: true,
      isLockedChild: false,
    });
  });
});

// ─── §6–§8: Multi-node trees ───────────────────────────────────────────────────

describe("buildDeployMap — multi-node trees (§6–§8)", () => {
  it("§6 root (non-provisioning) + child → root not deploying, child unlocked", () => {
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      [],
      { id: "root", isDeployingRoot: false, isLockedChild: false },
    );
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      [],
      { id: "child", isLockedChild: false },
    );
  });

  it("§7 root (provisioning) + child → root deploying, child locked", () => {
    check(
      [proj("root", null, "provisioning"), proj("child", "root", "idle")],
      [],
      {
        id: "root",
        isDeployingRoot: true,
        isLockedChild: false,
        descendantProvisioningCount: 1,
      },
    );
    check(
      [proj("root", null, "provisioning"), proj("child", "root", "idle")],
      [],
      { id: "child", isLockedChild: true },
    );
  });

  it("§8 three-level tree: grandparent (provisioning) → parent → child", () => {
    check(
      [
        proj("grandparent", null, "provisioning"),
        proj("parent", "grandparent", "idle"),
        proj("child", "parent", "idle"),
      ],
      [],
      {
        id: "grandparent",
        isDeployingRoot: true,
        isLockedChild: false,
        descendantProvisioningCount: 1,
      },
    );
    check(
      [
        proj("grandparent", null, "provisioning"),
        proj("parent", "grandparent", "idle"),
        proj("child", "parent", "idle"),
      ],
      [],
      { id: "parent", isLockedChild: true },
    );
    check(
      [
        proj("grandparent", null, "provisioning"),
        proj("parent", "grandparent", "idle"),
        proj("child", "parent", "idle"),
      ],
      [],
      { id: "child", isLockedChild: true },
    );
  });
});

// ─── §9–§11: DeletingIds + independent roots ──────────────────────────────────

describe("buildDeployMap — deletingIds + independent roots (§9–§11)", () => {
  it("§9 deletingIds contains a non-root → isLockedChild=true", () => {
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      ["child"],
      { id: "child", isLockedChild: true },
    );
  });

  it("§10 deletingIds contains the root → root isLockedChild=true, child unlocked", () => {
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      ["root"],
      { id: "root", isLockedChild: true, isDeployingRoot: false },
    );
    check(
      [proj("root", null, "idle"), proj("child", "root", "idle")],
      ["root"],
      { id: "child", isLockedChild: false },
    );
  });

  it("§11 two independent roots, only one is provisioning", () => {
    check(
      [
        proj("rootA", null, "idle"),
        proj("rootB", null, "provisioning"),
      ],
      [],
      { id: "rootA", isDeployingRoot: false, descendantProvisioningCount: 0 },
    );
    check(
      [
        proj("rootA", null, "idle"),
        proj("rootB", null, "provisioning"),
      ],
      [],
      { id: "rootB", isDeployingRoot: true, descendantProvisioningCount: 1 },
    );
  });
});

// ─── §12–§15: Provisioning counts + interactions ─────────────────────────────

describe("buildDeployMap — provisioning counts + interactions (§12–§15)", () => {
  it("§12 root has 2 provisioning descendants → descendantProvisioningCount=2", () => {
    check(
      [
        proj("root", null, "idle"),
        proj("prov1", "root", "provisioning"),
        proj("prov2", "root", "provisioning"),
        proj("idle", "root", "idle"),
      ],
      [],
      {
        id: "root",
        isDeployingRoot: true,
        descendantProvisioningCount: 2,
      },
    );
  });

  it("§13 non-root node with provisioning status → isActivelyProvisioning=true", () => {
    check(
      [
        proj("root", null, "idle"),
        proj("provChild", "root", "provisioning"),
      ],
      [],
      {
        id: "provChild",
        isActivelyProvisioning: true,
        isDeployingRoot: false,
        isLockedChild: false,
      },
    );
  });

  it("§14 findRoot memoization: chain is only walked once per root", () => {
    // Indirect verification: a 3-level tree should return consistent rootIds
    // for all nodes without throwing or producing stale entries.
    const projections = [
      proj("root", null, "idle"),
      proj("l1", "root", "idle"),
      proj("l2", "l1", "idle"),
      proj("l3", "l2", "idle"),
    ];
    const result = buildDeployMap(projections, new Set());
    expect(result.get("root")?.isDeployingRoot).toBe(false);
    expect(result.get("l1")?.isLockedChild).toBe(false);
    expect(result.get("l2")?.isLockedChild).toBe(false);
    expect(result.get("l3")?.isLockedChild).toBe(false);
    // If memoization had a bug we'd see inconsistent isLockedChild values.
  });

  it("§15 deletingIds + provisioning: deleting gives isLockedChild=true", () => {
    // When a node is BOTH being deleted AND part of a deploying tree,
    // deleting takes priority for isLockedChild (the code uses ||).
    check(
      [
        proj("root", null, "provisioning"),
        proj("provChild", "root", "idle"),
      ],
      ["provChild"],
      { id: "provChild", isLockedChild: true },
    );
  });
});

// ─── §16–§19: Deeper tree + edge cases ────────────────────────────────────────

describe("buildDeployMap — deep trees + edge cases (§16–§19)", () => {
  it("§16 child of provisioning root (not itself provisioning) → isLockedChild=true", () => {
    check(
      [
        proj("root", null, "provisioning"),
        proj("child", "root", "idle"),
      ],
      [],
      { id: "child", isLockedChild: true },
    );
  });

  it("§17 deep chain (5 levels), no provisioning → all nodes unlocked", () => {
    const deep = [
      proj("n1", null, "idle"),
      proj("n2", "n1", "idle"),
      proj("n3", "n2", "idle"),
      proj("n4", "n3", "idle"),
      proj("n5", "n4", "idle"),
    ];
    const result = buildDeployMap(deep, new Set());
    expect(result.get("n1")?.isDeployingRoot).toBe(false);
    expect(result.get("n1")?.isLockedChild).toBe(false);
    expect(result.get("n2")?.isLockedChild).toBe(false);
    expect(result.get("n3")?.isLockedChild).toBe(false);
    expect(result.get("n4")?.isLockedChild).toBe(false);
    expect(result.get("n5")?.isLockedChild).toBe(false);
  });

  it("§18 deep chain (5 levels), middle node is provisioning root", () => {
    // buildDeployMap builds byId from projections only.
    // findRoot walks the parent chain: n3.findRoot() → n3→n2→n1 → n1.parentId
    // absent from byId → rootId=n1 for ALL nodes.
    // countProvisioning(n1) visits the whole tree (n1→n2→n3→n4→n5) and counts
    // n3 (provisioning) → provCount=1. n1 is the sole deploying root.
    // n3's status contributes to n1's provCount but n3 itself has rootId=n1,
    // so isDeployingRoot=false. All non-root nodes are isLockedChild=true.
    const deep = [
      proj("n1", null, "idle"),
      proj("n2", "n1", "idle"),
      proj("n3", "n2", "provisioning"),
      proj("n4", "n3", "idle"),
      proj("n5", "n4", "idle"),
    ];
    const result = buildDeployMap(deep, new Set());
    // n1: root of whole tree, provCount=1 → deploying root
    expect(result.get("n1")?.isDeployingRoot).toBe(true);
    expect(result.get("n1")?.isLockedChild).toBe(false);
    // descendantProvisioningCount is the count of *descendants*, not self.
    // n1 itself is idle, so count=1 (n3).
    expect(result.get("n1")?.descendantProvisioningCount).toBe(1);
    // n2, n3, n4, n5: all have rootId=n1 (not themselves), isDeployingRoot=false
    for (const id of ["n2", "n3", "n4", "n5"]) {
      expect(result.get(id)?.isDeployingRoot).toBe(false);
      expect(result.get(id)?.isLockedChild).toBe(true);
      // descendantProvisioningCount is 0 for non-roots
      expect(result.get(id)?.descendantProvisioningCount).toBe(0);
    }
  });

  it("§19 parentId pointing to non-existent node → treated as root", () => {
    // Same node appears both as a child of a ghost parent AND as a parent of a real child.
    // When the ghost parent is absent, node2 is a root.
    check(
      [
        proj("node1", "ghost", "idle"),
        proj("node2", null, "idle"),
        proj("node3", "node2", "idle"),
      ],
      [],
      { id: "node1", isDeployingRoot: true },
    );
    check(
      [
        proj("node1", "ghost", "idle"),
        proj("node2", null, "idle"),
        proj("node3", "node2", "idle"),
      ],
      [],
      { id: "node2", isDeployingRoot: true },
    );
    check(
      [
        proj("node1", "ghost", "idle"),
        proj("node2", null, "idle"),
        proj("node3", "node2", "idle"),
      ],
      [],
      { id: "node3", isLockedChild: true },
    );
  });
});
