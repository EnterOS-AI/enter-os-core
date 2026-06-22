// @vitest-environment jsdom
/**
 * Tests for cssVar — maps ColorToken to a CSS variable string.
 *
 * Exists for the rare case where an inline style="" or SVG fill needs
 * a token value rather than a Tailwind class. The returned var(--color-foo)
 * string follows the live theme without re-renders.
 */
import { describe, it, expect } from "vitest";
import { cssVar } from "../theme";
import type { ColorToken } from "../theme";

describe("cssVar", () => {
  it("returns 'var(--color-surface)' for 'surface'", () => {
    expect(cssVar("surface")).toBe("var(--color-surface)");
  });

  it("returns 'var(--color-ink)' for 'ink'", () => {
    expect(cssVar("ink")).toBe("var(--color-ink)");
  });

  it("returns 'var(--color-accent)' for 'accent'", () => {
    expect(cssVar("accent")).toBe("var(--color-accent)");
  });

  it("returns 'var(--color-good)' for 'good'", () => {
    expect(cssVar("good")).toBe("var(--color-good)");
  });

  it("returns 'var(--color-bad)' for 'bad'", () => {
    expect(cssVar("bad")).toBe("var(--color-bad)");
  });

  it("returns 'var(--color-warn)' for 'warn'", () => {
    expect(cssVar("warn")).toBe("var(--color-warn)");
  });

  it("handles all surface variants", () => {
    const surfaces: ColorToken[] = ["surface", "surface-elevated", "surface-sunken", "surface-card"];
    for (const t of surfaces) {
      expect(cssVar(t)).toBe(`var(--color-${t})`);
    }
  });

  it("handles all ink variants", () => {
    const inks: ColorToken[] = ["ink", "ink-mid", "ink-soft", "ink-mute", "ink-dim"];
    for (const t of inks) {
      expect(cssVar(t)).toBe(`var(--color-${t})`);
    }
  });

  it("handles always-dark tokens", () => {
    const dark: ColorToken[] = ["bg", "bg-elev", "bg-card", "line-strong", "accent-dim", "plasma"];
    for (const t of dark) {
      expect(cssVar(t)).toBe(`var(--color-${t})`);
    }
  });

  it("is a pure function — same input always returns same output", () => {
    const tokens: ColorToken[] = ["surface", "accent", "good", "bad", "warm"];
    for (const t of tokens) {
      for (let i = 0; i < 3; i++) {
        expect(cssVar(t)).toBe(`var(--color-${t})`);
      }
    }
  });
});
