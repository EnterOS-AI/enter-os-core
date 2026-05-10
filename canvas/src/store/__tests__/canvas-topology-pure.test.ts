// @vitest-environment jsdom
/**
 * Tests for pure utility functions in canvas-topology.ts:
 * sortParentsBeforeChildren, defaultChildSlot, childSlotInGrid,
 * parentMinSize, parentMinSizeFromChildren.
 */
import { describe, it, expect } from "vitest";
import {
  sortParentsBeforeChildren,
  defaultChildSlot,
  childSlotInGrid,
  parentMinSize,
  parentMinSizeFromChildren,
} from "../canvas-topology";

// ─── sortParentsBeforeChildren ─────────────────────────────────────────────────

describe("sortParentsBeforeChildren", () => {
  it("returns [] for empty input", () => {
    expect(sortParentsBeforeChildren([])).toEqual([]);
  });

  it("returns single node unchanged", () => {
    const nodes = [{ id: "a", parentId: undefined }];
    expect(sortParentsBeforeChildren(nodes)).toEqual(nodes);
  });

  it("places parent before child", () => {
    // Deliberately reversed so naive iteration would place child first
    const nodes = [
      { id: "child", parentId: "parent" },
      { id: "parent", parentId: undefined },
    ];
    const result = sortParentsBeforeChildren(nodes);
    expect(result[0].id).toBe("parent");
    expect(result[1].id).toBe("child");
  });

  it("places grandparent before parent before child (deep chain)", () => {
    const nodes = [
      { id: "child", parentId: "parent" },
      { id: "grandchild", parentId: "child" },
      { id: "parent", parentId: "grandparent" },
      { id: "grandparent", parentId: undefined },
    ];
    const result = sortParentsBeforeChildren(nodes);
    const ids = result.map((n) => n.id);
    expect(ids).toEqual(["grandparent", "parent", "child", "grandchild"]);
  });

  it("siblings share the same parent", () => {
    const nodes = [
      { id: "b", parentId: "a" },
      { id: "a", parentId: undefined },
      { id: "c", parentId: "a" },
    ];
    const result = sortParentsBeforeChildren(nodes);
    expect(result[0].id).toBe("a");
    expect(new Set(result.slice(1).map((n) => n.id))).toEqual(new Set(["b", "c"]));
  });

  it("no-ops when children already precede parents", () => {
    // Already sorted — output should be in the same order
    const nodes = [
      { id: "root", parentId: undefined },
      { id: "child", parentId: "root" },
    ];
    expect(sortParentsBeforeChildren(nodes)).toEqual(nodes);
  });

  it("handles orphan nodes (no parentId)", () => {
    const nodes = [{ id: "a" }, { id: "b" }];
    expect(sortParentsBeforeChildren(nodes).map((n) => n.id)).toEqual(["a", "b"]);
  });

  it("returns a new array (does not mutate input)", () => {
    const nodes = [{ id: "child", parentId: "parent" }, { id: "parent", parentId: undefined }];
    const result = sortParentsBeforeChildren(nodes);
    expect(result).not.toBe(nodes);
  });

  it("deduplicates already-visited nodes", () => {
    // Child's parent is also in the list — visited guard prevents loops
    const nodes = [
      { id: "child", parentId: "parent" },
      { id: "parent", parentId: undefined },
    ];
    const result = sortParentsBeforeChildren(nodes);
    expect(result.map((n) => n.id)).toEqual(["parent", "child"]);
  });

  it("does not crash when parentId references a missing node", () => {
    const nodes = [
      { id: "orphan", parentId: "ghost" },
      { id: "root", parentId: undefined },
    ];
    // Missing parent is skipped; orphan placed after root
    const result = sortParentsBeforeChildren(nodes);
    expect(result.map((n) => n.id)).toEqual(["root", "orphan"]);
  });
});

// ─── defaultChildSlot ─────────────────────────────────────────────────────────

describe("defaultChildSlot — 2-column grid (240×130 cards)", () => {
  it("slot 0 → column 0, row 0", () => {
    const s = defaultChildSlot(0);
    expect(s).toEqual({ x: 16, y: 130 });
  });

  it("slot 1 → column 1, row 0", () => {
    const s = defaultChildSlot(1);
    expect(s.x).toBe(16 + 240 + 14); // PARENT_SIDE_PADDING + CHILD_DEFAULT_WIDTH + CHILD_GUTTER
    expect(s.y).toBe(130);
  });

  it("slot 2 → column 0, row 1", () => {
    const s = defaultChildSlot(2);
    expect(s.x).toBe(16);
    expect(s.y).toBe(130 + 130 + 14); // row 0 height + gutter
  });

  it("slot 3 → column 1, row 1", () => {
    const s = defaultChildSlot(3);
    expect(s.x).toBe(16 + 240 + 14);
    expect(s.y).toBe(130 + 130 + 14);
  });

  it("slot 4 → column 0, row 2", () => {
    const s = defaultChildSlot(4);
    expect(s.x).toBe(16);
    expect(s.y).toBe(130 + (130 + 14) * 2); // row 1 end + gutter
  });
});

// ─── childSlotInGrid ──────────────────────────────────────────────────────────

describe("childSlotInGrid — variable-size siblings", () => {
  it("empty siblingSizes returns side-padded position", () => {
    const s = childSlotInGrid(0, []);
    expect(s).toEqual({ x: 16, y: 130 });
  });

  it("slot 0 in uniform-size siblings matches defaultChildSlot", () => {
    const sizes = [{ width: 240, height: 130 }, { width: 240, height: 130 }];
    const s = childSlotInGrid(0, sizes);
    expect(s.x).toBe(16);
    expect(s.y).toBe(130);
  });

  it("taller sibling bumps next row down", () => {
    // Column width = max(200, 240) = 240; row 0 height = max(300, 130) = 300
    const sizes = [{ width: 200, height: 300 }, { width: 240, height: 130 }];
    const slot1 = childSlotInGrid(1, sizes);
    // Slot 1 is in column 1, row 0; x = 16 + 1*(240+14)
    expect(slot1.x).toBe(16 + 240 + 14);
    expect(slot1.y).toBe(130);
    // Slot 2 (col 0, row 1) — y must include row 0 height + gutter
    const slot2 = childSlotInGrid(2, sizes);
    expect(slot2.x).toBe(16);
    expect(slot2.y).toBe(130 + 300 + 14);
  });

  it("colW is the maximum sibling width, not the column of the target slot", () => {
    // Column width is always the max — slot at col 0 uses colW of wider col 1 sibling
    const sizes = [{ width: 100, height: 100 }, { width: 300, height: 100 }];
    const slot0 = childSlotInGrid(0, sizes);
    expect(slot0.x).toBe(16); // col 0
    // x for col 1 would be 16 + 300 + 14 = 330
    const slot1 = childSlotInGrid(1, sizes);
    expect(slot1.x).toBe(16 + 300 + 14);
  });
});

// ─── parentMinSize ─────────────────────────────────────────────────────────────

describe("parentMinSize — uniform-size children", () => {
  it("0 children → compact default (210×120)", () => {
    expect(parentMinSize(0)).toEqual({ width: 210, height: 120 });
  });

  it("1 child → 1 col, 1 row", () => {
    const s = parentMinSize(1);
    // width = 16*2 + 1*240 + 0 = 272; height = 130 + 1*130 + 0 + 16 = 276
    expect(s.width).toBe(16 * 2 + 240);
    expect(s.height).toBe(130 + 130 + 16);
  });

  it("2 children → 2 cols, 1 row", () => {
    const s = parentMinSize(2);
    // width = 16*2 + 2*240 + 1*14 = 526; height = 130 + 1*130 + 0 + 16 = 276
    expect(s.width).toBe(16 * 2 + 2 * 240 + 14);
    expect(s.height).toBe(130 + 130 + 16);
  });

  it("3 children → 2 cols, 2 rows", () => {
    const s = parentMinSize(3);
    // width = 16*2 + 2*240 + 1*14 = 526
    expect(s.width).toBe(16 * 2 + 2 * 240 + 14);
    // height = 130 + 2*130 + 1*14 + 16 = 416
    expect(s.height).toBe(130 + 2 * 130 + 14 + 16);
  });

  it("4 children → 2 cols, 2 rows (full grid)", () => {
    const s = parentMinSize(4);
    expect(s.width).toBe(16 * 2 + 2 * 240 + 14);
    expect(s.height).toBe(130 + 2 * 130 + 14 + 16);
  });

  it("5 children → 2 cols, 3 rows", () => {
    const s = parentMinSize(5);
    expect(s.width).toBe(16 * 2 + 2 * 240 + 14);
    expect(s.height).toBe(130 + 3 * 130 + 2 * 14 + 16);
  });
});

// ─── parentMinSizeFromChildren ────────────────────────────────────────────────

describe("parentMinSizeFromChildren — variable-size children", () => {
  it("empty array → compact default (210×120)", () => {
    expect(parentMinSizeFromChildren([])).toEqual({ width: 210, height: 120 });
  });

  it("single child matches defaultChildSlot bounding box", () => {
    const s = parentMinSizeFromChildren([{ width: 240, height: 130 }]);
    // cols=1, rows=1, colW=240
    expect(s.width).toBe(16 * 2 + 240); // 272
    expect(s.height).toBe(130 + 130 + 16); // 276
  });

  it("two equal-width children → same as parentMinSize(2)", () => {
    const fromChildren = parentMinSizeFromChildren([
      { width: 240, height: 130 },
      { width: 240, height: 130 },
    ]);
    expect(fromChildren.width).toBe(parentMinSize(2).width);
    expect(fromChildren.height).toBe(parentMinSize(2).height);
  });

  it("taller child increases height", () => {
    const tall = parentMinSizeFromChildren([{ width: 240, height: 400 }]);
    const short = parentMinSizeFromChildren([{ width: 240, height: 130 }]);
    expect(tall.height).toBeGreaterThan(short.height);
  });

  it("wider child increases width", () => {
    const wide = parentMinSizeFromChildren([{ width: 500, height: 130 }]);
    const narrow = parentMinSizeFromChildren([{ width: 200, height: 130 }]);
    expect(wide.width).toBeGreaterThan(narrow.width);
  });
});
