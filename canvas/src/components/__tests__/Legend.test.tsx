// @vitest-environment jsdom
/**
 * Tests for Legend component.
 *
 * Covers: open/closed state, localStorage persistence, palette-offset
 * positioning, status/tier/comm items rendering.
 */
import React from "react";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { Legend } from "../Legend";
import { useCanvasStore } from "@/store/canvas";

// ─── Mock localStorage ────────────────────────────────────────────────────────

const localStorageMock = (() => {
  let store: Record<string, string> = {};
  return {
    getItem: vi.fn((key: string) => store[key] ?? null),
    setItem: vi.fn((key: string, value: string) => { store[key] = value; }),
    removeItem: vi.fn((key: string) => { delete store[key]; }),
    clear: () => { store = {}; },
    getStore: () => store,
  };
})();
Object.defineProperty(window, "localStorage", { value: localStorageMock });

// ─── Mock canvas store ────────────────────────────────────────────────────────

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn(),
}));

afterEach(() => {
  cleanup();
  localStorageMock.clear();
  vi.clearAllMocks();
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("Legend — initial render (localStorage open)", () => {
  it("renders the legend panel when localStorage has no saved preference", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    expect(screen.getByText("Legend")).toBeTruthy();
  });

  it("renders the legend panel when localStorage has open=1", () => {
    localStorageMock.getItem.mockReturnValueOnce("1");
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    expect(screen.getByText("Legend")).toBeTruthy();
  });

  it("renders the collapsed pill when localStorage has open=0", () => {
    localStorageMock.getItem.mockReturnValueOnce("0");
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    // Collapsed pill shows "ⓘ Legend"
    expect(screen.getByText("Legend")).toBeTruthy();
    // Hide button should not be in the open panel
    expect(screen.queryByTitle("Hide legend")).toBeNull();
  });
});

describe("Legend — open panel content", () => {
  beforeEach(() => {
    localStorageMock.getItem.mockReturnValue("1");
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
  });

  it("renders the Status section with status items", () => {
    render(<Legend />);
    expect(screen.getByText("Status")).toBeTruthy();
    // All statuses from LEGEND_STATUSES
    expect(screen.getByText("Online")).toBeTruthy();
    expect(screen.getByText("Offline")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
  });

  it("renders the Tier section", () => {
    render(<Legend />);
    expect(screen.getByText("Tier")).toBeTruthy();
    expect(screen.getByText("Sandboxed")).toBeTruthy();
    expect(screen.getByText("Standard")).toBeTruthy();
    expect(screen.getByText("Privileged")).toBeTruthy();
    expect(screen.getByText("Full Access")).toBeTruthy();
  });

  it("renders the Communication section", () => {
    render(<Legend />);
    expect(screen.getByText("Communication")).toBeTruthy();
    expect(screen.getByText("A2A Out")).toBeTruthy();
    expect(screen.getByText("A2A In")).toBeTruthy();
    expect(screen.getByText("Task")).toBeTruthy();
    expect(screen.getByText("Error")).toBeTruthy();
  });

  it("renders the hide button", () => {
    render(<Legend />);
    expect(screen.getByTitle("Hide legend")).toBeTruthy();
  });
});

describe("Legend — close and reopen", () => {
  it("closes when the hide button is clicked and persists to localStorage", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    fireEvent.click(screen.getByTitle("Hide legend"));
    // localStorage should be updated to "0"
    expect(localStorageMock.setItem).toHaveBeenCalledWith(
      "molecule.legend.open",
      "0"
    );
  });

  it("reopens when the collapsed pill is clicked and persists to localStorage", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    // Initially open — close it
    fireEvent.click(screen.getByTitle("Hide legend"));
    // Collapsed pill appears
    expect(screen.getByTitle("Show legend")).toBeTruthy();
    // Reopen
    fireEvent.click(screen.getByTitle("Show legend"));
    expect(localStorageMock.setItem).toHaveBeenLastCalledWith(
      "molecule.legend.open",
      "1"
    );
  });
});

describe("Legend — palette offset positioning", () => {
  // The panel has data-testid="legend-panel" so we can select it reliably.
  // screen.getByText("Legend") also appears in the collapsed pill, so the
  // old .closest("div") approach matched the wrong element in the DOM.
  it("uses left-4 when template palette is NOT open", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    // The outer panel div is the one with position classes (fixed bottom-6).
    // screen.getByText("Legend") returns the inner heading text; get its
    // closest ancestor with position-related classes (bottom-6).
    const panel = screen.getByText("Legend").closest("div[class*='bottom-6']");
    expect(panel?.className).toContain("left-4");
  });

  it("uses left-[296px] when template palette IS open", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: true } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    const panel = screen.getByText("Legend").closest("div[class*='bottom-6']");
    expect(panel?.className).toContain("left-[296px]");
  });
});

describe("Legend — aria attributes", () => {
  it("the hide button has aria-label", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    const hideBtn = screen.getByTitle("Hide legend");
    expect(hideBtn.getAttribute("aria-label")).toBe("Hide legend");
  });

  it("the show legend pill has aria-label", () => {
    vi.mocked(useCanvasStore).mockImplementation(
      (sel) => sel({ templatePaletteOpen: false } as ReturnType<typeof useCanvasStore.getState>)
    );
    render(<Legend />);
    fireEvent.click(screen.getByTitle("Hide legend"));
    const pill = screen.getByTitle("Show legend");
    expect(pill.getAttribute("aria-label")).toBe("Show legend");
  });
});
