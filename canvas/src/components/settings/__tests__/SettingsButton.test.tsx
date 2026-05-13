// @vitest-environment jsdom
/**
 * SettingsButton — gear icon in top bar, toggles SettingsPanel.
 *
 * Per spec §1.1:
 *   - Gear icon, aria-label="Settings"
 *   - aria-expanded reflects panel open state
 *   - Tooltip shows keyboard shortcut
 *   - Active state class when panel open
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs.
 *
 * Covers:
 *   - Button has aria-label="Settings"
 *   - Gear SVG has aria-hidden="true"
 *   - aria-expanded is false when panel closed
 *   - aria-expanded is true when panel open
 *   - Toggle calls openPanel / closePanel
 *   - Active class applied when panel open
 *   - Tooltip content shows correct shortcut
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

// ResizeObserver polyfill required by Radix Tooltip's use-size hook
globalThis.ResizeObserver = class ResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
};

import { SettingsButton } from "../SettingsButton";

// ─── Store mock ────────────────────────────────────────────────────────────────

const _mockIsPanelOpen = vi.fn<() => boolean>(() => false);
const _mockOpenPanel = vi.fn();
const _mockClosePanel = vi.fn();

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: (selector?: (s: {
    isPanelOpen: boolean;
    openPanel: () => void;
    closePanel: () => void;
  }) => unknown) => {
    const state = {
      isPanelOpen: _mockIsPanelOpen(),
      openPanel: _mockOpenPanel,
      closePanel: _mockClosePanel,
    };
    return selector ? selector(state) : state;
  },
}));

// Mock navigator for isMac detection
Object.defineProperty(navigator, "userAgent", {
  configurable: true,
  value: "Macintosh",
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

beforeEach(() => {
  _mockIsPanelOpen.mockReturnValue(false);
  _mockOpenPanel.mockClear();
  _mockClosePanel.mockClear();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("SettingsButton — render", () => {
  it("button has aria-label='Settings'", () => {
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.getAttribute("aria-label")).toBe("Settings");
  });

  it("gear SVG has aria-hidden='true'", () => {
    render(<SettingsButton />);
    const svg = document.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("aria-expanded is false when panel is closed", () => {
    _mockIsPanelOpen.mockReturnValue(false);
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.getAttribute("aria-expanded")).toBe("false");
  });

  it("aria-expanded is true when panel is open", () => {
    _mockIsPanelOpen.mockReturnValue(true);
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.getAttribute("aria-expanded")).toBe("true");
  });

  it("button has settings-button class", () => {
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.className).toContain("settings-button");
  });

  it("active class applied when panel is open", () => {
    _mockIsPanelOpen.mockReturnValue(true);
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.className).toContain("settings-button--active");
  });

  it("active class NOT applied when panel is closed", () => {
    _mockIsPanelOpen.mockReturnValue(false);
    render(<SettingsButton />);
    const btn = document.querySelector("button");
    expect(btn?.className).not.toContain("settings-button--active");
  });
});

// ─── Interaction ───────────────────────────────────────────────────────────────

describe("SettingsButton — interaction", () => {
  it("clicking when panel closed calls openPanel", () => {
    _mockIsPanelOpen.mockReturnValue(false);
    render(<SettingsButton />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    btn.click();
    expect(_mockOpenPanel).toHaveBeenCalledTimes(1);
    expect(_mockClosePanel).not.toHaveBeenCalled();
  });

  it("clicking when panel open calls closePanel", () => {
    _mockIsPanelOpen.mockReturnValue(true);
    render(<SettingsButton />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    btn.click();
    expect(_mockClosePanel).toHaveBeenCalledTimes(1);
    expect(_mockOpenPanel).not.toHaveBeenCalled();
  });

  it("tooltip shows Mac shortcut on Mac", async () => {
    Object.defineProperty(navigator, "userAgent", {
      configurable: true,
      value: "Macintosh",
    });
    render(<SettingsButton />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    act(() => { fireEvent.focus(btn); });
    // Wait for Radix tooltip delay (300ms) + render
    await waitFor(() => {
      const tooltipText = document.body.textContent ?? "";
      expect(tooltipText).toContain("Settings");
      expect(tooltipText).toContain("⌘");
    });
  });

  it("tooltip shows Ctrl+ shortcut on non-Mac", async () => {
    Object.defineProperty(navigator, "userAgent", {
      configurable: true,
      value: "Windows",
    });
    render(<SettingsButton />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    act(() => { fireEvent.focus(btn); });
    await waitFor(() => {
      const tooltipText = document.body.textContent ?? "";
      expect(tooltipText).toContain("Settings");
      expect(tooltipText).toContain("Ctrl");
    });
  });
});
