// @vitest-environment jsdom
"use client";
/**
 * Tests for palette-context.tsx — MobileAccentProvider context + usePalette hook.
 *
 * Test coverage (9 cases):
 * 1. MobileAccentProvider renders children
 * 2. usePalette(false) without provider → MOL_LIGHT
 * 3. usePalette(true) without provider → MOL_DARK
 * 4. accent=null returns base palette unchanged
 * 5. accent=base.accent returns base palette unchanged (identity guard)
 * 6. accent="#custom" overrides both accent and online
 * 7. MOL_LIGHT singleton never mutated
 * 8. MOL_DARK singleton never mutated
 *
 * Plus pure-function coverage for normalizeStatus + tierCode.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import React from "react";
import { render, screen, cleanup } from "@testing-library/react";
import {
  MOL_LIGHT,
  MOL_DARK,
  getPalette,
  normalizeStatus,
  tierCode,
  MobileAccentProvider,
  usePalette,
} from "../palette-context";

// ─── usePalette test helper ───────────────────────────────────────────────────
// usePalette reads document.documentElement.dataset.theme internally.
// We set this before rendering so the hook sees the right value.

function setDataTheme(theme: "light" | "dark") {
  if (typeof document !== "undefined") {
    document.documentElement.dataset.theme = theme;
  }
}

// ─── Pure function tests ──────────────────────────────────────────────────────

describe("normalizeStatus", () => {
  it("returns emerald-400 for online status", () => {
    expect(normalizeStatus("online", false)).toBe("bg-emerald-400");
    expect(normalizeStatus("online", true)).toBe("bg-emerald-400");
  });

  it("returns emerald-400 for degraded status", () => {
    expect(normalizeStatus("degraded", false)).toBe("bg-emerald-400");
    expect(normalizeStatus("degraded", true)).toBe("bg-emerald-400");
  });

  it("returns red-400 for failed status", () => {
    expect(normalizeStatus("failed", false)).toBe("bg-red-400");
    expect(normalizeStatus("failed", true)).toBe("bg-red-400");
  });

  it("returns amber-400 for paused status", () => {
    expect(normalizeStatus("paused", false)).toBe("bg-amber-400");
    expect(normalizeStatus("paused", true)).toBe("bg-amber-400");
  });

  it("returns amber-400 for not_configured status", () => {
    expect(normalizeStatus("not_configured", false)).toBe("bg-amber-400");
  });

  it("returns zinc-400 for unknown status", () => {
    expect(normalizeStatus("unknown", false)).toBe("bg-zinc-400");
    expect(normalizeStatus("", false)).toBe("bg-zinc-400");
  });
});

describe("tierCode", () => {
  it("returns T1 for tier 1", () => {
    expect(tierCode(1)).toBe("T1");
  });

  it("returns T2 for tier 2", () => {
    expect(tierCode(2)).toBe("T2");
  });

  it("returns T4 for tier 4", () => {
    expect(tierCode(4)).toBe("T4");
  });

  it("returns generic T{n} for non-standard tiers", () => {
    expect(tierCode(99)).toBe("T99");
  });
});

// ─── getPalette tests ─────────────────────────────────────────────────────────

describe("getPalette — accent override", () => {
  it("accent=null returns base palette unchanged (light)", () => {
    const result = getPalette(null, false);
    expect(result).toEqual({ ...MOL_LIGHT });
    expect(result).not.toBe(MOL_LIGHT); // returned object is a copy
  });

  it("accent=null returns base palette unchanged (dark)", () => {
    const result = getPalette(null, true);
    expect(result).toEqual({ ...MOL_DARK });
    expect(result).not.toBe(MOL_DARK);
  });

  it("accent=base.accent returns base palette unchanged (identity guard, light)", () => {
    const result = getPalette(MOL_LIGHT.accent, false);
    expect(result).toEqual({ ...MOL_LIGHT });
    expect(result).not.toBe(MOL_LIGHT);
  });

  it("accent=base.accent returns base palette unchanged (identity guard, dark)", () => {
    const result = getPalette(MOL_DARK.accent, true);
    expect(result).toEqual({ ...MOL_DARK });
    expect(result).not.toBe(MOL_DARK);
  });

  it("accent='#custom' overrides accent and online (light)", () => {
    const result = getPalette("#ff0000", false);
    expect(result.accent).toBe("#ff0000");
    expect(result.online).toBe("bg-emerald-400"); // normalizeStatus("online", false)
  });

  it("accent='#custom' overrides accent and online (dark)", () => {
    const result = getPalette("#00ff00", true);
    expect(result.accent).toBe("#00ff00");
    expect(result.online).toBe("bg-emerald-400"); // normalizeStatus("online", true)
  });

  it("MOL_LIGHT singleton is never mutated", () => {
    getPalette("#mutate", false);
    // All fields must still match the original freeze definition
    expect(MOL_LIGHT.accent).toBe("bg-blue-500");
    expect(MOL_LIGHT.online).toBe("bg-emerald-400");
    expect(MOL_LIGHT.surface).toBe("bg-zinc-900");
    expect(MOL_LIGHT.ink).toBe("text-zinc-100");
    expect(MOL_LIGHT.line).toBe("border-zinc-700");
    expect(MOL_LIGHT.bg).toBe("bg-zinc-950");
  });

  it("MOL_DARK singleton is never mutated", () => {
    getPalette("#mutate", true);
    expect(MOL_DARK.accent).toBe("bg-sky-400");
    expect(MOL_DARK.online).toBe("bg-emerald-400");
    expect(MOL_DARK.surface).toBe("bg-zinc-800");
    expect(MOL_DARK.ink).toBe("text-zinc-100");
    expect(MOL_DARK.line).toBe("border-zinc-700");
    expect(MOL_DARK.bg).toBe("bg-zinc-950");
  });

  it("getPalette always returns a new object (no shared mutation risk)", () => {
    const a = getPalette("#a", false);
    const b = getPalette("#b", false);
    expect(a).not.toBe(b);
    expect(a.accent).not.toBe(b.accent);
  });
});

// ─── MobileAccentProvider tests ───────────────────────────────────────────────

describe("MobileAccentProvider", () => {
  beforeEach(() => {
    setDataTheme("light");
  });

  afterEach(() => {
    cleanup();
    if (typeof document !== "undefined") {
      document.documentElement.dataset.theme = "";
    }
  });

  it("renders children", () => {
    render(
      <MobileAccentProvider accent={null}>
        <span data-testid="child">Hello</span>
      </MobileAccentProvider>,
    );
    expect(screen.getByTestId("child")).toBeTruthy();
  });

  // usePalette hook reads data-theme from <html> to determine light/dark.
  // In the test environment, data-theme is empty, which falls through to
  // the "light" default in usePalette, giving MOL_LIGHT.
  it("usePalette(false) without provider → MOL_LIGHT", () => {
    setDataTheme("light");
    function ShowPalette() {
      const p = usePalette(false);
      return <span data-testid="accent-light">{p.accent}</span>;
    }
    render(<ShowPalette />);
    expect(screen.getByTestId("accent-light").textContent).toBe(MOL_LIGHT.accent);
  });

  it("usePalette(true) without provider → MOL_DARK when data-theme=dark", () => {
    setDataTheme("dark");
    function ShowPalette() {
      const p = usePalette(true);
      return <span data-testid="accent-dark">{p.accent}</span>;
    }
    render(<ShowPalette />);
    expect(screen.getByTestId("accent-dark").textContent).toBe(MOL_DARK.accent);
  });
});
