// @vitest-environment jsdom
/**
 * palette-context: MobileAccentProvider + usePalette hook coverage.
 *
 * Covers:
 *   - usePalette(dark=false) without provider → MOL_LIGHT
 *   - usePalette(dark=true)  without provider → MOL_DARK
 *   - usePalette with provider accent=null        → base palette unchanged
 *   - usePalette with provider accent=base.accent → base palette unchanged (identity guard)
 *   - usePalette with provider accent="#ff0000"  → accent + online overridden
 *   - MobileAccentProvider renders children
 *   - Never mutates the static MOL_LIGHT/MOL_DARK singletons
 *
 * The pure functions (getPalette, normalizeStatus, tierCode) are covered
 * in palette.test.ts — only the React context/hook is tested here.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { MobileAccentProvider, usePalette } from "../palette-context";
import { MOL_DARK, MOL_LIGHT } from "../palette";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Test helpers ──────────────────────────────────────────────────────────────
// Each helper renders exactly one usePalette value as a testid element.
// Using unique testids per scenario avoids "multiple elements" DOM pollution
// when tests run in the same jsdom worker without strict cleanup timing.

function AccentDump({ dark }: { dark: boolean }) {
  const palette = usePalette(dark);
  return <span data-testid="accent-val">{palette.accent}</span>;
}

function OnlineDump({ dark }: { dark: boolean }) {
  const palette = usePalette(dark);
  return <span data-testid="online-val">{palette.online}</span>;
}

// ─── MobileAccentProvider ──────────────────────────────────────────────────────
describe("MobileAccentProvider", () => {
  it("renders children", () => {
    const { getByText } = render(
      <MobileAccentProvider accent={null}>
        <span>child content</span>
      </MobileAccentProvider>,
    );
    expect(getByText("child content").textContent).toBe("child content");
  });
});

// ─── usePalette — no provider ─────────────────────────────────────────────────
describe("usePalette without MobileAccentProvider", () => {
  it("returns MOL_LIGHT when dark=false", () => {
    const { getByTestId } = render(<AccentDump dark={false} />);
    expect(getByTestId("accent-val").textContent).toBe(MOL_LIGHT.accent);
  });

  it("returns MOL_DARK when dark=true", () => {
    const { getByTestId } = render(<AccentDump dark={true} />);
    expect(getByTestId("accent-val").textContent).toBe(MOL_DARK.accent);
  });
});

// ─── usePalette — with MobileAccentProvider ────────────────────────────────────
describe("usePalette with MobileAccentProvider", () => {
  it("returns base palette unchanged when accent=null", () => {
    const { getByTestId } = render(
      <MobileAccentProvider accent={null}>
        <AccentDump dark={false} />
      </MobileAccentProvider>,
    );
    expect(getByTestId("accent-val").textContent).toBe(MOL_LIGHT.accent);
  });

  it("returns base palette unchanged when accent matches base.accent (identity guard)", () => {
    const { getByTestId } = render(
      <MobileAccentProvider accent={MOL_LIGHT.accent}>
        <AccentDump dark={false} />
      </MobileAccentProvider>,
    );
    expect(getByTestId("accent-val").textContent).toBe(MOL_LIGHT.accent);
  });

  it("overrides accent when provider supplies a different colour", () => {
    const CUSTOM = "#ff0000";
    const { getByTestId } = render(
      <MobileAccentProvider accent={CUSTOM}>
        <AccentDump dark={false} />
      </MobileAccentProvider>,
    );
    expect(getByTestId("accent-val").textContent).toBe(CUSTOM);
  });

  it("also overrides online when accent is overridden", () => {
    const CUSTOM = "#ff8800";
    const { getByTestId } = render(
      <MobileAccentProvider accent={CUSTOM}>
        <OnlineDump dark={false} />
      </MobileAccentProvider>,
    );
    expect(getByTestId("online-val").textContent).toBe(CUSTOM);
  });
});

// ─── Immutability ─────────────────────────────────────────────────────────────
describe("MOL_LIGHT and MOL_DARK singletons are never mutated", () => {
  it("MOL_LIGHT.accent unchanged after custom-accent render", () => {
    const before = MOL_LIGHT.accent;
    render(
      <MobileAccentProvider accent="#deadbeef">
        <AccentDump dark={false} />
      </MobileAccentProvider>,
    );
    expect(MOL_LIGHT.accent).toBe(before);
  });

  it("MOL_DARK.accent unchanged after custom-accent render", () => {
    const before = MOL_DARK.accent;
    render(
      <MobileAccentProvider accent="#bada55ff">
        <AccentDump dark={true} />
      </MobileAccentProvider>,
    );
    expect(MOL_DARK.accent).toBe(before);
  });
});
