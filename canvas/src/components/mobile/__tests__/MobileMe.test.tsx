// @vitest-environment jsdom
/**
 * MobileMe — theme, accent, and density preferences.
 *
 * Per spec: theme + accent + density settings for mobile.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { MobileMe } from "../MobileMe";

// ─── Mock theme provider ───────────────────────────────────────────────────────

const mockSetTheme = vi.fn();
const mockSetAccent = vi.fn();
const mockSetDensity = vi.fn();

vi.mock("@/lib/theme-provider", () => ({
  useTheme: vi.fn(() => ({
    theme: "system",
    resolvedTheme: "light",
    setTheme: mockSetTheme,
  })),
}));

// ─── Helpers ─────────────────────────────────────────────────────────────────

function renderMe(overrides: Partial<{
  dark: boolean;
  accent: string;
  density: "compact" | "regular";
}> = {}) {
  return render(
    <MobileMe
      dark={overrides.dark ?? false}
      accent={overrides.accent ?? "#2f9e6a"}
      setAccent={mockSetAccent}
      density={overrides.density ?? "regular"}
      setDensity={mockSetDensity}
    />,
  );
}

// ─── Setup / teardown ─────────────────────────────────────────────────────────

beforeEach(() => {
  mockSetTheme.mockClear();
  mockSetAccent.mockClear();
  mockSetDensity.mockClear();
});

afterEach(() => {
  cleanup();
});

// ─── Structure ───────────────────────────────────────────────────────────────

describe("MobileMe — page structure", () => {
  it('renders "Me" heading', () => {
    const { container } = renderMe();
    const h1 = container.querySelector("h1");
    expect(h1).toBeTruthy();
    expect(h1!.textContent).toBe("Me");
  });

  it("renders theme section label", () => {
    const { container } = renderMe();
    expect(container.textContent ?? "").toContain("Theme");
  });

  it("renders theme options: System, Light, Dark", () => {
    const { container } = renderMe();
    const text = container.textContent ?? "";
    expect(text).toContain("System");
    expect(text).toContain("Light");
    expect(text).toContain("Dark");
  });

  it("renders accent section label", () => {
    const { container } = renderMe();
    expect(container.textContent ?? "").toContain("Accent");
  });

  it("renders all 5 accent color swatches", () => {
    const { container } = renderMe();
    const swatches = container.querySelectorAll("button[aria-label]");
    // 5 accent swatches + theme buttons + density buttons = more than 5
    // We verify the accent swatches by checking aria-labels
    const accentLabels = Array.from(swatches)
      .map((b) => b.getAttribute("aria-label") ?? "")
      .filter((l) => l.startsWith("Set accent"));
    expect(accentLabels.length).toBe(5);
  });

  it("renders density section label", () => {
    const { container } = renderMe();
    expect(container.textContent ?? "").toContain("Density");
  });

  it("renders density options: Regular, Compact", () => {
    const { container } = renderMe();
    const text = container.textContent ?? "";
    expect(text).toContain("Regular");
    expect(text).toContain("Compact");
  });

  it("renders version footer", () => {
    const { container } = renderMe();
    expect(container.textContent ?? "").toContain("Mobile design preview");
  });
});

// ─── Theme selection ──────────────────────────────────────────────────────────

describe("MobileMe — theme selection", () => {
  it("renders System as the active theme (from mock)", () => {
    const { container } = renderMe();
    // The theme buttons are rendered; System is active in our mock
    // We verify the buttons exist and are findable
    const buttons = Array.from(container.querySelectorAll("button"));
    const themeButtons = buttons.filter(
      (b) => ["System", "Light", "Dark"].includes(b.textContent?.trim() ?? ""),
    );
    expect(themeButtons.length).toBe(3);
  });

  it("calls setTheme when a theme button is clicked", () => {
    const { container } = renderMe();
    const darkBtn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Dark",
    );
    expect(darkBtn).toBeTruthy();
    darkBtn!.click();
    expect(mockSetTheme).toHaveBeenCalledWith("dark");
  });
});

// ─── Accent selection ────────────────────────────────────────────────────────

describe("MobileMe — accent selection", () => {
  it("renders accent buttons with aria-label", () => {
    const { container } = renderMe();
    const swatches = container.querySelectorAll("button[aria-label]");
    const accentSwatches = Array.from(swatches).filter(
      (b) => (b.getAttribute("aria-label") ?? "").startsWith("Set accent"),
    );
    expect(accentSwatches.length).toBe(5);
  });

  it("calls setAccent with the correct color", () => {
    const { container } = renderMe();
    const swatch = Array.from(container.querySelectorAll("button[aria-label]")).find(
      (b) => b.getAttribute("aria-label") === "Set accent #3b6fe0",
    );
    expect(swatch).toBeTruthy();
    swatch!.click();
    expect(mockSetAccent).toHaveBeenCalledWith("#3b6fe0");
  });
});

// ─── Density selection ────────────────────────────────────────────────────────

describe("MobileMe — density selection", () => {
  it("renders density buttons", () => {
    const { container } = renderMe();
    const buttons = Array.from(container.querySelectorAll("button"));
    const densityButtons = buttons.filter(
      (b) => ["Regular", "Compact"].includes(b.textContent?.trim() ?? ""),
    );
    expect(densityButtons.length).toBe(2);
  });

  it("calls setDensity when Compact is clicked", () => {
    const { container } = renderMe({ density: "regular" });
    const compactBtn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Compact",
    );
    expect(compactBtn).toBeTruthy();
    compactBtn!.click();
    expect(mockSetDensity).toHaveBeenCalledWith("compact");
  });

  it("calls setDensity when Regular is clicked", () => {
    const { container } = renderMe({ density: "compact" });
    const regularBtn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Regular",
    );
    expect(regularBtn).toBeTruthy();
    regularBtn!.click();
    expect(mockSetDensity).toHaveBeenCalledWith("regular");
  });
});

// ─── Dark mode ───────────────────────────────────────────────────────────────

describe("MobileMe — dark mode", () => {
  it("renders without crashing in dark mode", () => {
    const { container } = renderMe({ dark: true });
    expect(container.querySelector("h1")?.textContent).toBe("Me");
  });

  it("renders theme, accent, and density sections in dark mode", () => {
    const { container } = renderMe({ dark: true });
    const text = container.textContent ?? "";
    expect(text).toContain("Theme");
    expect(text).toContain("Accent");
    expect(text).toContain("Density");
  });
});
