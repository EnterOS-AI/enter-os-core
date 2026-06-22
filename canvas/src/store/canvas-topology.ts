import type { Node, Edge } from "@xyflow/react";
import type { WorkspaceData } from "./socket";
import type { WorkspaceNodeData } from "./canvas";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";

/** Thrown when workspace parent chains contain a cycle. Catching this at
 *  load time lets the UI surface a degraded state instead of hanging. */
export class TopologyCycleError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "TopologyCycleError";
  }
}

const H_SPACING = 320;
const V_SPACING = 200;

// Default card footprint we use when we don't yet have a measured size
// (first render, before React Flow reports dimensions). These match the
// min-width / min-height that WorkspaceNode.tsx sets, so a parent built
// from them will never start too small for its children on first paint.
/**
 * Re-orders a React Flow node array so parents always appear BEFORE
 * their children. React Flow requires this ordering; when it's
 * violated RF logs "Parent node ... not found" and renders the child
 * at canvas-absolute coords (losing the parent-relative transform).
 *
 * We call this every time nestNode / batchNest mutates parentId —
 * without a re-sort a freshly-nested child can appear AFTER its new
 * parent in the array, which breaks the next drag.
 */
export function sortParentsBeforeChildren<T extends { id: string; parentId?: string }>(
  nodes: T[],
): T[] {
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const visited = new Set<string>();
  const visiting = new Set<string>();
  const out: T[] = [];

  const visit = (n: T) => {
    if (visited.has(n.id)) return;
    if (visiting.has(n.id)) {
      throw new TopologyCycleError(
        `cyclic parent chain detected at workspace ${n.id}`,
      );
    }
    visiting.add(n.id);
    if (n.parentId) {
      const parent = byId.get(n.parentId);
      if (parent && !visited.has(parent.id)) visit(parent);
    }
    visiting.delete(n.id);
    visited.add(n.id);
    out.push(n);
  };

  try {
    for (const n of nodes) visit(n);
  } catch (err) {
    if (err instanceof TopologyCycleError) {
      // Fail-closed: a corrupt cycle is not recoverable by re-ordering.
      // Return the input unchanged so callers (e.g. drag re-parent) don't
      // hang, and let the next full hydrate surface the topology error.
      return nodes;
    }
    throw err;
  }

  // Separate roots, valid children, and orphans:
  // - roots: no parentId — true tree roots
  // - valid children: has parentId pointing to an existing node
  // - orphans: has parentId but the referenced parent is not in the node list
  // Ordering: roots → valid children → orphans
  const roots = out.filter((n) => !n.parentId);
  const children = out.filter(
    (n) => n.parentId !== undefined && byId.has(n.parentId),
  );
  const orphans = out.filter(
    (n) => n.parentId !== undefined && !byId.has(n.parentId),
  );
  return [...roots, ...children, ...orphans];
}

// Grid-slot defaults for children laid under a parent. The card
// component (WorkspaceNode.tsx) renders leaves at exactly w-[300px] /
// min-h-[176px], so a slot stride of CHILD_DEFAULT_WIDTH + CHILD_GUTTER
// guarantees cards never bleed into their neighbour's slot. Keep these
// in sync with the Go mirror in workspace-server/internal/handlers/org.go
// — changing one without the other leads to import-time / runtime drift.
export const CHILD_DEFAULT_WIDTH = 300;
export const CHILD_DEFAULT_HEIGHT = 176;
// Parent header space — reserves room above the child grid so the
// parent's own name + runtime pill + clamped role + currentTask
// banner aren't covered by the first row of child cards. The
// currentTask banner appears on freshly-provisioning agents (initial
// prompt gets queued as their current task) and adds ~30px below the
// role; without this headroom, the first child overlaps the amber
// banner and makes the parent card look broken on import. Keep in
// sync with the Go mirror in org.go.
export const PARENT_HEADER_PADDING = 130;
export const PARENT_SIDE_PADDING = 16;
export const PARENT_BOTTOM_PADDING = 16;
export const CHILD_GUTTER = 14;


/**
 * A deterministic grid slot for the n-th child inside a parent, counted
 * left-to-right then top-to-bottom. Used to lay out org-imported teams
 * and to rescue children whose stored position puts them outside the
 * parent's bounding box. 2-column grid is wide enough to read but
 * narrow enough to keep the parent card from becoming a widescreen.
 *
 * Leaf-sized slots only — for variable-size siblings (mix of leaves
 * and nested parents), use `childSlotInGrid` below instead.
 */
export function defaultChildSlot(index: number): { x: number; y: number } {
  const col = index % 2;
  const row = Math.floor(index / 2);
  const x = PARENT_SIDE_PADDING + col * (CHILD_DEFAULT_WIDTH + CHILD_GUTTER);
  const y =
    PARENT_HEADER_PADDING + row * (CHILD_DEFAULT_HEIGHT + CHILD_GUTTER);
  return { x, y };
}

export interface NodeSize {
  width: number;
  height: number;
}

/** Grid column count for laying children inside a parent. Matches the
 *  Go server mirror (childGridColumnCount). */
const GRID_COLS = 2;

/** Utility: per-row max height in a size[] laid out column-major. */
function rowHeightsOf(sizes: NodeSize[], cols: number): number[] {
  const rows = Math.ceil(sizes.length / cols);
  const out = new Array(rows).fill(0);
  sizes.forEach((s, i) => {
    const row = Math.floor(i / cols);
    out[row] = Math.max(out[row], s.height);
  });
  return out;
}

/** Uniform column width = max of all sibling widths. Keeps the grid
 *  rectangular (alternative: variable col widths — visually unstable
 *  when one sibling is much wider than the rest). */
function colWidthOf(sizes: NodeSize[]): number {
  return sizes.reduce((m, s) => Math.max(m, s.width), 0);
}

/**
 * Grid slot for the n-th sibling when siblings have variable sizes
 * (e.g., a mix of leaves and nested parents). Uniform column width +
 * per-row max height, so bigger nested parents push their row down
 * without displacing columns.
 */
export function childSlotInGrid(
  index: number,
  siblingSizes: NodeSize[],
): { x: number; y: number } {
  if (siblingSizes.length === 0) return { x: PARENT_SIDE_PADDING, y: PARENT_HEADER_PADDING };
  const cols = Math.min(GRID_COLS, siblingSizes.length);
  const col = index % cols;
  const row = Math.floor(index / cols);
  const colW = colWidthOf(siblingSizes);
  const rowHs = rowHeightsOf(siblingSizes, cols);
  const x = PARENT_SIDE_PADDING + col * (colW + CHILD_GUTTER);
  let y = PARENT_HEADER_PADDING;
  for (let r = 0; r < row; r++) y += rowHs[r] + CHILD_GUTTER;
  return { x, y };
}

/**
 * Minimum parent size that still fits `childCount` uniformly-sized
 * children. Leaf-slot variant — kept for back-compat with callers that
 * don't have per-child sizes (bumpZOrder, arrangeChildren).
 */
export function parentMinSize(childCount: number): { width: number; height: number } {
  if (childCount <= 0) {
    return { width: 210, height: 120 };
  }
  const cols = Math.min(GRID_COLS, childCount);
  const rows = Math.ceil(childCount / cols);
  const width =
    PARENT_SIDE_PADDING * 2 +
    cols * CHILD_DEFAULT_WIDTH +
    (cols - 1) * CHILD_GUTTER;
  const height =
    PARENT_HEADER_PADDING +
    rows * CHILD_DEFAULT_HEIGHT +
    (rows - 1) * CHILD_GUTTER +
    PARENT_BOTTOM_PADDING;
  return { width, height };
}

/**
 * Minimum parent size that fits a set of (possibly non-uniform)
 * children. Uniform column width, per-row max height — matches the
 * geometry produced by `childSlotInGrid`. Used when a parent has
 * grandchildren and a leaf-slot-sized grid can't hold the real,
 * bigger nested cards.
 */
export function parentMinSizeFromChildren(children: NodeSize[]): NodeSize {
  if (children.length === 0) return { width: 210, height: 120 };
  const cols = Math.min(GRID_COLS, children.length);
  const rows = Math.ceil(children.length / cols);
  const colW = colWidthOf(children);
  const rowHs = rowHeightsOf(children, cols);
  const totalRowH = rowHs.reduce((a, b) => a + b, 0);
  return {
    width: PARENT_SIDE_PADDING * 2 + colW * cols + CHILD_GUTTER * (cols - 1),
    height:
      PARENT_HEADER_PADDING +
      totalRowH +
      CHILD_GUTTER * (rows - 1) +
      PARENT_BOTTOM_PADDING,
  };
}

/**
 * Computes auto-layout positions for workspaces that have no persisted position
 * (x === 0 AND y === 0). Workspaces with an existing non-zero position are used
 * as anchors and are never moved.
 *
 * Returns a Map of workspace IDs → {x, y} for every workspace that was assigned
 * a computed position (i.e. only the ones that were at 0,0). Callers should
 * persist these back to the API so the positions survive reload.
 */
export function computeAutoLayout(
  workspaces: WorkspaceData[]
): Map<string, { x: number; y: number }> {
  const overrides = new Map<string, { x: number; y: number }>();

  // Separate anchored (already positioned) from zero-position workspaces
  const anchored = new Set<string>();
  for (const ws of workspaces) {
    if (ws.x !== 0 || ws.y !== 0) {
      anchored.add(ws.id);
    }
  }

  // If every workspace is already positioned, nothing to do
  const needsLayout = workspaces.filter((ws) => !anchored.has(ws.id));
  if (needsLayout.length === 0) return overrides;

  // Build parent→children map
  const children = new Map<string | null, WorkspaceData[]>();
  for (const ws of workspaces) {
    const pid = ws.parent_id ?? null;
    if (!children.has(pid)) children.set(pid, []);
    children.get(pid)!.push(ws);
  }

  // Sort children by name for deterministic layout
  for (const list of children.values()) {
    list.sort((a, b) => a.name.localeCompare(b.name));
  }

  // Assigned positions (includes anchors from the original data + computed overrides)
  const assigned = new Map<string, { x: number; y: number }>();
  for (const ws of workspaces) {
    if (anchored.has(ws.id)) {
      assigned.set(ws.id, { x: ws.x, y: ws.y });
    }
  }

  // BFS from root nodes that need layout
  // Track the next X offset per depth row to spread siblings horizontally
  const rowNextX = new Map<number, number>();

  // Enqueue root-level nodes that need layout
  const queue: Array<{ ws: WorkspaceData; depth: number }> = [];
  const rootsNeedingLayout = (children.get(null) ?? []).filter(
    (ws) => !anchored.has(ws.id)
  );
  for (const ws of rootsNeedingLayout) {
    queue.push({ ws, depth: 0 });
  }

  while (queue.length > 0) {
    const { ws, depth } = queue.shift()!;

    // Skip if already assigned (e.g. anchored)
    if (assigned.has(ws.id)) {
      // Still enqueue its unpositioned children
      const kids = (children.get(ws.id) ?? []).filter(
        (c) => !anchored.has(c.id) && !assigned.has(c.id)
      );
      for (const kid of kids) {
        queue.push({ ws: kid, depth: depth + 1 });
      }
      continue;
    }

    // Find parent's x as the center hint for this node
    const parentPos = ws.parent_id ? assigned.get(ws.parent_id) : undefined;
    const parentX = parentPos?.x ?? 0;

    // Place node at the next available slot in this row
    const currentRowX = rowNextX.get(depth) ?? (parentX - H_SPACING / 2);
    const x = Math.max(currentRowX, parentX);
    const y = depth * V_SPACING;

    assigned.set(ws.id, { x, y });
    overrides.set(ws.id, { x, y });
    rowNextX.set(depth, x + H_SPACING);

    // Enqueue children that need layout
    const kids = (children.get(ws.id) ?? []).filter(
      (c) => !anchored.has(c.id) && !assigned.has(c.id)
    );
    for (const kid of kids) {
      queue.push({ ws: kid, depth: depth + 1 });
    }
  }

  return overrides;
}

/**
 * Converts raw workspace data from the API into React Flow nodes and edges.
 * Accepts an optional layoutOverrides map (from computeAutoLayout) to override
 * positions for workspaces that were at 0,0.
 *
 * `currentParentSizes` carries the LIVE measured/grown dimensions of parent
 * nodes from the existing client store. The auto-rescue heuristic below
 * (line ~445) compares each child's stored relative position against its
 * parent's bbox; without the live size, the bbox is whatever the
 * grid-derived initial min-size formula produced. That falsely rescued
 * children dragged into the user-grown area on every periodic rehydrate
 * (socket.ts:87 fires every 30s if no WS events seen) — observed
 * 2026-04-25 as "child jumps to weird location, then settles 30s later".
 *
 * Parent/child rendering model: every workspace is a first-class React Flow
 * node (full card). When a workspace has parent_id set, its RF `parentId` is
 * set to the parent's id and its position is stored RELATIVE to the parent
 * origin — React Flow renders the child inside the parent's coordinate space,
 * so moving the parent automatically moves all children. The DB keeps
 * absolute x/y; the abs→rel conversion happens here on load, and the
 * reverse translation happens in savePosition.
 */
export function buildNodesAndEdges(
  workspaces: WorkspaceData[],
  layoutOverrides: Map<string, { x: number; y: number }> = new Map(),
  currentParentSizes: Map<string, { width: number; height: number }> = new Map(),
): {
  nodes: Node<WorkspaceNodeData>[];
  edges: Edge[];
} {
  // React Flow requires parent nodes to appear before children in the nodes
  // array. Topological-sort by depth-first walk from roots so children come
  // after their parent regardless of the order the API returned them.
  const byId = new Map(workspaces.map((w) => [w.id, w]));
  const visited = new Set<string>();
  const visiting = new Set<string>();
  const sorted: WorkspaceData[] = [];
  function visit(ws: WorkspaceData) {
    if (visited.has(ws.id)) return;
    if (visiting.has(ws.id)) {
      throw new TopologyCycleError(
        `cyclic parent chain detected at workspace ${ws.id}`,
      );
    }
    visiting.add(ws.id);
    if (ws.parent_id && byId.has(ws.parent_id) && !visited.has(ws.parent_id)) {
      visit(byId.get(ws.parent_id)!);
    }
    visiting.delete(ws.id);
    visited.add(ws.id);
    sorted.push(ws);
  }
  workspaces.forEach(visit);

  // Resolve each workspace's absolute position (apply layout override if any).
  const absPos = new Map<string, { x: number; y: number }>();
  for (const ws of workspaces) {
    const o = layoutOverrides.get(ws.id);
    absPos.set(ws.id, { x: o?.x ?? ws.x, y: o?.y ?? ws.y });
  }

  // Count children per parent so we can size parents to fit their team
  // before any runtime measurement comes back.
  const childCounts = new Map<string, number>();
  for (const ws of workspaces) {
    if (ws.parent_id) {
      childCounts.set(ws.parent_id, (childCounts.get(ws.parent_id) ?? 0) + 1);
    }
  }

  // Index direct children per parent for post-order subtree sizing.
  // We walk `sorted` in REVERSE (post-order — children first) so
  // subtreeSize[parent] sees its grandchildren-inclusive sizes via the
  // already-computed subtreeSize[child].
  const childrenByParent = new Map<string, WorkspaceData[]>();
  for (const ws of workspaces) {
    if (ws.parent_id && byId.has(ws.parent_id)) {
      const arr = childrenByParent.get(ws.parent_id) ?? [];
      arr.push(ws);
      childrenByParent.set(ws.parent_id, arr);
    }
  }
  const subtreeSize = new Map<string, NodeSize>();
  for (let i = sorted.length - 1; i >= 0; i--) {
    const ws = sorted[i];
    const kids = childrenByParent.get(ws.id) ?? [];
    if (kids.length === 0 || ws.collapsed) {
      subtreeSize.set(ws.id, { width: CHILD_DEFAULT_WIDTH, height: CHILD_DEFAULT_HEIGHT });
    } else {
      const kidSizes = kids.map((k) =>
        subtreeSize.get(k.id) ?? { width: CHILD_DEFAULT_WIDTH, height: CHILD_DEFAULT_HEIGHT },
      );
      subtreeSize.set(ws.id, parentMinSizeFromChildren(kidSizes));
    }
  }

  // Track each parent's initial size so we can reset children that land
  // outside those bounds. Parents without children fall back to the leaf
  // default; parents with children get the grid-derived minimum — which
  // now accounts for grandchildren via subtreeSize, so a nested parent
  // no longer overflows its slot.
  const parentSize = new Map<string, { width: number; height: number }>();
  for (const ws of workspaces) {
    // Reuse subtreeSize — it already accounts for nested grandchildren.
    parentSize.set(
      ws.id,
      subtreeSize.get(ws.id) ?? { width: CHILD_DEFAULT_WIDTH, height: CHILD_DEFAULT_HEIGHT },
    );
  }

  // Running index of children already placed per parent — used to hand
  // out default grid slots for children whose stored position is outside
  // the parent's computed box.
  const nextChildIndex = new Map<string, number>();

  // Depth per node so children always render above parents (and above
  // parent's root-level siblings). React Flow uses a flat zIndex, so a
  // child inherits zIndex = parent.zIndex + 1 — xyflow issue #4012.
  const depthById = new Map<string, number>();
  for (const ws of sorted) {
    const d = ws.parent_id ? (depthById.get(ws.parent_id) ?? 0) + 1 : 0;
    depthById.set(ws.id, d);
  }

  // Mark each node as hidden if any ancestor is collapsed. Walk from
  // the root so children inherit the flag efficiently. (Parents stay
  // visible; only descendants are hidden so the parent renders as a
  // compact header-only card.)
  const hiddenById = new Map<string, boolean>();
  for (const ws of sorted) {
    if (!ws.parent_id) {
      hiddenById.set(ws.id, false);
      continue;
    }
    const parent = byId.get(ws.parent_id);
    const parentHidden = hiddenById.get(ws.parent_id) ?? false;
    hiddenById.set(ws.id, parentHidden || !!parent?.collapsed);
  }

  const nodes: Node<WorkspaceNodeData>[] = sorted.map((ws) => {
    const abs = absPos.get(ws.id)!;
    const hasParent = !!ws.parent_id && byId.has(ws.parent_id);
    let position = abs;
    if (hasParent) {
      const pa = absPos.get(ws.parent_id!)!;
      position = { x: abs.x - pa.x, y: abs.y - pa.y };

      // Auto-rescue on load: fires only when the child's bounding box
      // is FULLY outside the parent's computed bbox by at least one
      // child-width/height. Two real failure modes this covers:
      //
      //   - Legacy data: a child whose stored absolute coords predate
      //     the nesting assignment, so abs→rel produces a huge offset
      //     far past any parent edge.
      //   - Corrupt org-imports with positions in a different
      //     coordinate space.
      //
      // Rejected heuristics we deliberately avoid:
      //   - `position.x < 0` alone — catches legitimate drift when the
      //     user drags the parent past a child that had small positive
      //     stored coords (child's relative goes mildly negative, but
      //     the layout is still recoverable).
      //   - Raw magnitude like `> 8000` — doesn't scale with parent
      //     size; a user who resized the parent huge could legitimately
      //     place a child at 9000px.
      //
      // Children slightly past the initial min-size (user had resized
      // the parent larger on a previous session) are NEVER rescued —
      // the bbox-overlap test gives them room. The manual "Arrange
      // Children" context command is still the escape hatch for that
      // bucket of data.
      // Pure bbox-overlap test — self-calibrating without a magic
      // margin. Rescue iff the child's bbox has ZERO overlap with the
      // parent's bbox (the child would render completely detached).
      //   drift case (position.x = -40, CHILD_WIDTH = 260):
      //     child.right = 220, overlaps parent.left = 0 → kept
      //   screenshot case (position.x = -420, CHILD_WIDTH = 260):
      //     child.right = -160, doesn't overlap parent.left = 0 → rescued
      //   user resized larger (parent.width now 800, position.x = 500):
      //     child.left = 500 < parent.right = 800 → overlaps → kept
      //   legacy huge positive (position.x = 50000):
      //     child.left = 50000 >= parent.right → no overlap → rescued
      const initialPsize = parentSize.get(ws.parent_id!)!;
      // Use the larger of (initial min, currently grown) for the bbox
      // test. Without this, a child the user dragged into the grown
      // area appears "outside" the (smaller) initial bbox and the
      // rescue below false-fires on every periodic rehydrate, jumping
      // the child to a stale grid slot. Live grown dims arrive via
      // currentParentSizes from hydrate(); on first load (empty
      // store), the map is empty and we fall back to the initial min
      // — preserving the original rescue semantics for genuinely
      // detached legacy data.
      const liveParentSize = currentParentSizes.get(ws.parent_id!);
      const psize = liveParentSize
        ? {
            width: Math.max(initialPsize.width, liveParentSize.width),
            height: Math.max(initialPsize.height, liveParentSize.height),
          }
        : initialPsize;
      const myW = subtreeSize.get(ws.id)?.width ?? CHILD_DEFAULT_WIDTH;
      const myH = subtreeSize.get(ws.id)?.height ?? CHILD_DEFAULT_HEIGHT;
      const overlapsX =
        position.x + myW > 0 && position.x < psize.width;
      const overlapsY =
        position.y + myH > 0 && position.y < psize.height;
      if (!overlapsX || !overlapsY) {
        const idx = nextChildIndex.get(ws.parent_id!) ?? 0;
        nextChildIndex.set(ws.parent_id!, idx + 1);
        // Use sibling-size-aware grid so a nested parent doesn't collide
        // with a leaf sibling in the next row.
        const siblings = (childrenByParent.get(ws.parent_id!) ?? []).map(
          (c) => subtreeSize.get(c.id) ?? { width: CHILD_DEFAULT_WIDTH, height: CHILD_DEFAULT_HEIGHT },
        );
        position = childSlotInGrid(idx, siblings);
      }
    }
    const node: Node<WorkspaceNodeData> = {
      id: ws.id,
      type: "workspaceNode",
      position,
      data: {
        name: ws.name,
        status: ws.status,
        tier: ws.tier,
        agentCard: ws.agent_card,
        activeTasks: ws.active_tasks,
        collapsed: ws.collapsed,
        role: ws.role,
        lastErrorRate: ws.last_error_rate,
        lastSampleError: ws.last_sample_error,
        url: ws.url,
        parentId: ws.parent_id,
        currentTask: ws.current_task || "",
        runtime: ws.runtime || "",
        workspaceAccess: ws.workspace_access,
        maxConcurrentTasks: ws.max_concurrent_tasks ?? null,
        needsRestart: false,
        budgetLimit: ws.budget_limit ?? null,
        budgetUsed: ws.budget_used ?? null,
        // #2054 — server-declared per-workspace provisioning timeout.
        // Falls through to the runtime profile when null/absent.
        provisionTimeoutMs: ws.provision_timeout_ms ?? null,
        // Workspace abilities — defaults preserved for old platform versions
        // that don't yet include these columns in the GET response.
        broadcastEnabled: ws.broadcast_enabled ?? false,
        talkToUserEnabled: ws.talk_to_user_enabled ?? true,
        // A2A delivery mode (task #227). Absent on older ws-server builds
        // — leave undefined so the chat UI's "?? 'push'" fallback applies.
        deliveryMode: ws.delivery_mode,
        compute: ws.compute,
        // Org-level platform agent ('platform') vs ordinary workspace. The map
        // view hides the platform root (it's the undeletable org anchor) via
        // stripPlatformRootForMap; the shell home tree keeps it as ROOT.
        kind: ws.kind ?? WORKSPACE_KIND.Workspace,
      },
    };
    if (hasParent) {
      // React Flow native parent binding: children render inside parent's
      // coordinate space and move with the parent. No `extent: 'parent'` —
      // the user can drag a child out to un-nest (handled in Canvas.tsx
      // onNodeDragStop with a bbox hit test).
      node.parentId = ws.parent_id!;
    }
    // Stack children above their ancestors (xyflow #4012).
    node.zIndex = depthById.get(ws.id) ?? 0;
    // Collapse: descendants of a collapsed parent get hidden so the
    // parent renders as a compact header-only card.
    if (hiddenById.get(ws.id)) {
      node.hidden = true;
    }
    // Seed every node with an explicit starting size so the initial
    // grid layout is stable before React Flow has measured the DOM.
    //   - Parents (has children, not collapsed): sized to fit the
    //     child grid via parentMinSize so children don't render
    //     outside the bounds on first paint.
    //   - Collapsed parents: leaf-sized (header-only card).
    //   - Leaves: leaf-sized — they land in their grid slot cleanly.
    //
    // Sizes are fully system-controlled (free-resize was removed): these
    // initial values stand, and at runtime React Flow re-measures leaves
    // from their fixed-size card CSS while parents grow to fit children
    // (growParentsToFitChildren). Width/height are never persisted.
    const kids = childCounts.get(ws.id) ?? 0;
    if (kids > 0 && !ws.collapsed) {
      const size = parentSize.get(ws.id)!;
      node.width = size.width;
      node.height = size.height;
    } else {
      node.width = CHILD_DEFAULT_WIDTH;
      node.height = CHILD_DEFAULT_HEIGHT;
    }
    return node;
  });

  // Edges stay empty — the visual parent/child cue is the enclosing card.
  const edges: Edge[] = [];

  return { nodes, edges };
}

/**
 * Extracts skill names from an agent card's skills array.
 */
export function extractSkillNames(agentCard: Record<string, unknown> | null): string[] {
  if (!agentCard) return [];
  const skills = agentCard.skills;
  if (!Array.isArray(skills)) return [];
  return skills
    .map((skill: Record<string, unknown>) => String(skill.name || skill.id || ""))
    .filter(Boolean);
}

/**
 * Returns the configuration status reported by the workspace, or null
 * when the agent card doesn't carry one (older runtime, or pre-PR #2756
 * worker).
 *
 * Pairs with molecule-core PR #2756: when adapter.setup() fails, the
 * runtime mounts a not-configured handler AND advertises the failure
 * via agent_card.configuration_status = "not_configured" +
 * configuration_error = "<reason>". Canvas reads both to render a
 * "needs config" tile instead of a confused "online but silent" state.
 *
 * Returns null (not undefined) so callers can distinguish "no info"
 * from explicit values via a strict equality check.
 */
export function getConfigurationStatus(
  agentCard: Record<string, unknown> | null,
): "ready" | "not_configured" | null {
  if (!agentCard) return null;
  const raw = agentCard.configuration_status;
  if (raw === "ready" || raw === "not_configured") return raw;
  return null;
}

/**
 * Returns the configuration error string from the agent card when
 * configuration_status is "not_configured", or null otherwise.
 *
 * Already redacted server-side via secret_redactor (PR #2778) — safe to
 * render in the UI verbatim.
 */
export function getConfigurationError(
  agentCard: Record<string, unknown> | null,
): string | null {
  if (!agentCard) return null;
  if (getConfigurationStatus(agentCard) !== "not_configured") return null;
  const raw = agentCard.configuration_error;
  return typeof raw === "string" && raw.length > 0 ? raw : null;
}

/**
 * Map-view filter: removes the org-level platform agent (the concierge) from
 * the node graph. The platform agent is the undeletable org ROOT — every other
 * workspace hangs under it — so it is surfaced as the shell's org anchor
 * (topbar + Home tree), NOT as a draggable/deletable map node.
 *
 * Its direct children are promoted to top-level: React Flow stores child
 * positions RELATIVE to the parent, so when the parent is dropped each child is
 * converted back to an absolute position (parent.position + child.position) and
 * its parent binding cleared. Edges touching the platform node are dropped.
 *
 * The store keeps the full node set (the shell's Home agent tree renders the
 * platform as ROOT); only the map's React Flow input is stripped.
 */
export function stripPlatformRootForMap(
  nodes: Node<WorkspaceNodeData>[],
  edges: Edge[],
): { nodes: Node<WorkspaceNodeData>[]; edges: Edge[] } {
  const platformIds = new Set(
    nodes.filter((n) => n.data.kind === WORKSPACE_KIND.Platform).map((n) => n.id),
  );
  if (platformIds.size === 0) return { nodes, edges };

  const posById = new Map(nodes.map((n) => [n.id, n.position]));
  const outNodes = nodes
    .filter((n) => !platformIds.has(n.id))
    .map((n) => {
      const pid = n.parentId;
      if (pid && platformIds.has(pid)) {
        const parentPos = posById.get(pid) ?? { x: 0, y: 0 };
        return {
          ...n,
          parentId: undefined,
          extent: undefined,
          position: {
            x: parentPos.x + n.position.x,
            y: parentPos.y + n.position.y,
          },
          data: { ...n.data, parentId: null },
        } as Node<WorkspaceNodeData>;
      }
      return n;
    });

  const outEdges = edges.filter(
    (e) => !platformIds.has(e.source) && !platformIds.has(e.target),
  );
  return { nodes: outNodes, edges: outEdges };
}
