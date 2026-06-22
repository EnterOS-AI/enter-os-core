/**
 * Tests for `cn` — the canvas's className-merging helper. Wraps
 * `twMerge(clsx(inputs))` so callers can:
 *   1. Combine class strings + arrays + objects (clsx),
 *   2. Resolve Tailwind conflicts so the LAST value wins on the same
 *      utility (twMerge — e.g. `cn("p-2", "p-4")` → "p-4").
 *
 * Tiny surface but load-bearing — every component that conditionally
 * styles uses this. A regression that loses Tailwind-merge dedup would
 * show as silent class duplication in the rendered DOM (cosmetic, but
 * accumulates and breaks `:where()` rules + theme overrides).
 *
 * Issue: #1815 follow-up — `src/lib/utils.ts` was at 0% coverage.
 */
import { describe, it, expect } from "vitest";
import { cn } from "../utils";

describe("cn", () => {
  it("returns a single class unchanged", () => {
    expect(cn("text-red-500")).toBe("text-red-500");
  });

  it("joins multiple positional classes", () => {
    expect(cn("text-red-500", "bg-zinc-900")).toBe("text-red-500 bg-zinc-900");
  });

  it("flattens array inputs (clsx-style)", () => {
    expect(cn(["text-red-500", "bg-zinc-900"])).toBe(
      "text-red-500 bg-zinc-900",
    );
  });

  it("respects truthy / falsy conditional object syntax", () => {
    expect(
      cn({ "text-red-500": true, "text-blue-500": false, "bg-zinc-900": true }),
    ).toBe("text-red-500 bg-zinc-900");
  });

  it("dedups conflicting Tailwind utilities — last wins", () => {
    // The single load-bearing reason for twMerge over plain clsx —
    // a regression here would silently double-apply padding tokens
    // and confuse the visible style.
    expect(cn("p-2", "p-4")).toBe("p-4");
    expect(cn("text-red-500", "text-blue-500")).toBe("text-blue-500");
  });

  it("keeps non-conflicting Tailwind utilities", () => {
    // Make sure the dedup is keyed on utility group, not blanket
    // merge. p-2 and m-2 don't conflict; both must survive.
    expect(cn("p-2", "m-4")).toBe("p-2 m-4");
  });

  it("handles a mix of all input shapes", () => {
    expect(
      cn(
        "base-class",
        ["array-class-1", "array-class-2"],
        { "object-true": true, "object-false": false },
        "trailing-class",
      ),
    ).toBe(
      "base-class array-class-1 array-class-2 object-true trailing-class",
    );
  });

  it("handles empty / nullish inputs without throwing", () => {
    expect(cn()).toBe("");
    expect(cn("")).toBe("");
    expect(cn(null, undefined, false)).toBe("");
    expect(cn("active", null, "highlighted", undefined)).toBe(
      "active highlighted",
    );
  });
});
