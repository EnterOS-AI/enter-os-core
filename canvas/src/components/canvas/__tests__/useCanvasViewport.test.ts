import { describe, it, expect } from "vitest";
import { pruneStaleKeys, shouldFitGrowing } from "../useCanvasViewport";

// Tests cover the auto-fit gate in isolation. The hook itself is
// effects + refs + React Flow handles, awkward to exercise directly —
// extracting the pure decision into shouldFitGrowing(...) lets us
// pin down the regression-prone logic with unit tests instead.

describe("shouldFitGrowing", () => {
  it("fits the very first time (no prior snapshot)", () => {
    expect(shouldFitGrowing(["a"], undefined, null, 0)).toBe(true);
  });

  it("fits when the prior snapshot is empty", () => {
    expect(shouldFitGrowing(["a", "b"], new Set(), null, 0)).toBe(true);
  });

  it("fits when a brand-new id has been added since the last fit", () => {
    const prev = new Set(["root", "a", "b"]);
    expect(shouldFitGrowing(["root", "a", "b", "c"], prev, null, 0)).toBe(true);
  });

  it("respects user pan when the subtree hasn't grown", () => {
    const prev = new Set(["root", "a", "b"]);
    // Status update on existing node — same membership.
    expect(shouldFitGrowing(["root", "a", "b"], prev, 5_000, 1_000)).toBe(false);
  });

  it("fits when the subtree hasn't grown but the user never panned", () => {
    const prev = new Set(["root", "a", "b"]);
    expect(shouldFitGrowing(["root", "a", "b"], prev, null, 1_000)).toBe(true);
  });

  it("fits when the subtree hasn't grown and the user panned BEFORE the last fit", () => {
    const prev = new Set(["root", "a", "b"]);
    expect(shouldFitGrowing(["root", "a", "b"], prev, 500, 1_000)).toBe(true);
  });

  it("forces fit on delete-then-add even when the count is unchanged", () => {
    // Subtree was [root, a, b, c, d]. Then `d` got removed and a
    // sibling `e` arrived. Same length, different membership — a
    // length-only check would skip the fit and leave `e` off-screen.
    const prev = new Set(["root", "a", "b", "c", "d"]);
    expect(
      shouldFitGrowing(["root", "a", "b", "c", "e"], prev, 5_000, 1_000),
    ).toBe(true);
  });

  it("does NOT fit on shrink-only when the user has panned (deletion alone shouldn't override exploration)", () => {
    const prev = new Set(["root", "a", "b", "c"]);
    expect(shouldFitGrowing(["root", "a", "b"], prev, 5_000, 1_000)).toBe(false);
  });
});

describe("pruneStaleKeys (#2070)", () => {
  it("drops entries whose key is no longer in the live key set", () => {
    const map = new Map<string, Set<string>>([
      ["root-1", new Set(["root-1", "a"])],
      ["root-2", new Set(["root-2", "b"])],
      ["root-3", new Set(["root-3", "c"])],
    ]);
    pruneStaleKeys(map, new Set(["root-1", "root-3"]));
    expect([...map.keys()].sort()).toEqual(["root-1", "root-3"]);
  });

  it("is a no-op when every key is still live", () => {
    const map = new Map<string, Set<string>>([
      ["root-1", new Set(["root-1"])],
      ["root-2", new Set(["root-2"])],
    ]);
    pruneStaleKeys(map, new Set(["root-1", "root-2"]));
    expect(map.size).toBe(2);
  });

  it("clears the map when no live keys remain", () => {
    const map = new Map<string, Set<string>>([
      ["root-1", new Set(["root-1"])],
    ]);
    pruneStaleKeys(map, new Set());
    expect(map.size).toBe(0);
  });

  it("does not add new entries — only deletes stale ones", () => {
    const map = new Map<string, Set<string>>();
    pruneStaleKeys(map, new Set(["root-1", "root-2"]));
    expect(map.size).toBe(0);
  });

  it("preserves value identity for survivors (no rebuild)", () => {
    const survivor = new Set(["root-1", "a"]);
    const map = new Map<string, Set<string>>([
      ["root-1", survivor],
      ["root-2", new Set(["root-2", "b"])],
    ]);
    pruneStaleKeys(map, new Set(["root-1"]));
    expect(map.get("root-1")).toBe(survivor);
  });
});
