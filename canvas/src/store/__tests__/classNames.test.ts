// @vitest-environment jsdom
/**
 * Tests for store/classNames helpers — the centralised string
 * manipulation for React Flow's space-separated className strings.
 *
 * Why this is load-bearing: every spawn / parent-pulse / one-shot
 * animation flow runs through these helpers. Dedup correctness
 * matters because React Flow's diffing treats className identity
 * by string equality, so a stray double-class on every render
 * thrashes layout repeatedly. Whitespace handling matters because
 * upstream class strings sometimes arrive with multiple spaces
 * (legacy concat) — the helpers must collapse them.
 *
 * Issue: #1815 follow-up — store/classNames.ts was at 17% coverage.
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import {
  appendClass,
  removeClass,
  scheduleNodeClassRemoval,
} from "../classNames";

// ── appendClass ──────────────────────────────────────────────────────────────

describe("appendClass", () => {
  it("returns just `cls` when existing is undefined", () => {
    expect(appendClass(undefined, "spawn")).toBe("spawn");
  });

  it("returns just `cls` when existing is empty string", () => {
    expect(appendClass("", "spawn")).toBe("spawn");
  });

  it("appends to a single-class existing", () => {
    expect(appendClass("a", "b")).toBe("a b");
  });

  it("does NOT duplicate when class already present (the dedup contract)", () => {
    // The whole reason this lives in classNames.ts: pre-helper, the
    // call sites inlined `${existing} ${cls}` with no dedup, so a
    // tick that fired the same class twice produced "a a" and
    // React Flow treated it as a className change every render
    // (string equality fails) → constant re-render thrash.
    expect(appendClass("a b spawn", "spawn")).toBe("a b spawn");
    expect(appendClass("spawn", "spawn")).toBe("spawn");
  });

  it("collapses multiple spaces in the input (whitespace normalization)", () => {
    // Upstream sometimes arrives with double spaces (legacy concat
    // path). Filter+join normalizes regardless of input shape.
    expect(appendClass("a   b", "c")).toBe("a b c");
  });

  it("ignores leading/trailing whitespace in existing", () => {
    expect(appendClass("  a b  ", "c")).toBe("a b c");
  });
});

// ── removeClass ──────────────────────────────────────────────────────────────

describe("removeClass", () => {
  it("returns empty string when existing is undefined", () => {
    expect(removeClass(undefined, "spawn")).toBe("");
  });

  it("returns empty string when existing is empty", () => {
    expect(removeClass("", "spawn")).toBe("");
  });

  it("removes the named class", () => {
    expect(removeClass("a spawn b", "spawn")).toBe("a b");
  });

  it("removes only exact matches (not substrings)", () => {
    // "spawn" must NOT match "spawn-fast". String split on
    // whitespace + exact compare gives this for free.
    expect(removeClass("spawn spawn-fast", "spawn")).toBe("spawn-fast");
  });

  it("returns empty string when removing the only class", () => {
    expect(removeClass("spawn", "spawn")).toBe("");
  });

  it("is a no-op when class isn't present", () => {
    expect(removeClass("a b c", "missing")).toBe("a b c");
  });

  it("collapses multiple spaces and removes empty entries", () => {
    expect(removeClass("a   spawn   b", "spawn")).toBe("a b");
  });
});

// ── scheduleNodeClassRemoval ─────────────────────────────────────────────────

describe("scheduleNodeClassRemoval", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("calls set() with className-removed nodes after delayMs", () => {
    const get = vi.fn(() => ({
      nodes: [
        { id: "ws-a", className: "spawn animate-pulse" },
        { id: "ws-b", className: "spawn" },
      ],
    }));
    const set = vi.fn();

    scheduleNodeClassRemoval("ws-a", "spawn", 200, get, set);

    // Timer hasn't fired yet — no set call.
    expect(set).not.toHaveBeenCalled();

    vi.advanceTimersByTime(200);

    expect(set).toHaveBeenCalledTimes(1);
    const patch = set.mock.calls[0][0] as {
      nodes: Array<{ id: string; className?: string }>;
    };
    // Target node had `spawn` removed, kept `animate-pulse`.
    const wsA = patch.nodes.find((n) => n.id === "ws-a")!;
    expect(wsA.className).toBe("animate-pulse");
    // Other node UNTOUCHED — class still present, NOT pruned by id mismatch.
    const wsB = patch.nodes.find((n) => n.id === "ws-b")!;
    expect(wsB.className).toBe("spawn");
  });

  it("does not fire before the delay elapses", () => {
    const get = vi.fn(() => ({ nodes: [{ id: "x", className: "spawn" }] }));
    const set = vi.fn();
    scheduleNodeClassRemoval("x", "spawn", 500, get, set);
    vi.advanceTimersByTime(499);
    expect(set).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(set).toHaveBeenCalledTimes(1);
  });

  it("is a no-op when window is undefined (SSR safety)", () => {
    // jsdom defines `window` by default; mock it to undefined for
    // this case so the SSR guard is exercised. Don't `vi.useFakeTimers`
    // here since we're asserting NO timer was ever scheduled.
    vi.useRealTimers();
    const originalWindow = globalThis.window;
    // @ts-expect-error — deliberately undefining window to simulate SSR.
    globalThis.window = undefined;

    const get = vi.fn();
    const set = vi.fn();
    try {
      scheduleNodeClassRemoval("x", "spawn", 100, get, set);
    } finally {
      globalThis.window = originalWindow;
    }

    expect(get).not.toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
  });
});
