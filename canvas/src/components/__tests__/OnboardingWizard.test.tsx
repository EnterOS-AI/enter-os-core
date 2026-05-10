// @vitest-environment jsdom
/**
 * Tests for OnboardingWizard component.
 *
 * Covers: renders only when not dismissed, renders 4 steps, dismiss
 * button, localStorage persistence, progress bar width, step navigation,
 * auto-advance from welcome→api-key on nodes change, aria-live region.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OnboardingWizard } from "../OnboardingWizard";
import { useCanvasStore } from "@/store/canvas";

const mockStoreState = {
  nodes: [] as Array<{ id: string; data: Record<string, unknown> }>,
  selectedNodeId: null as string | null,
  panelTab: "chat" as string,
  agentMessages: {} as Record<string, unknown[]>,
  setPanelTab: vi.fn(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (sel: (s: typeof mockStoreState) => unknown) => sel(mockStoreState),
    { getState: () => mockStoreState },
  ),
}));

const STORAGE_KEY = "molecule-onboarding-complete";

const localStorageMock = (() => {
  let store: Record<string, string> = {};
  return {
    getItem: vi.fn((key: string): string | null => store[key] ?? null),
    setItem: vi.fn((key: string, value: string) => { store[key] = value; }),
    removeItem: vi.fn((key: string) => { delete store[key]; }),
    clear: () => { store = {}; },
    getStore: () => store,
  };
})();
Object.defineProperty(window, "localStorage", { value: localStorageMock });

afterEach(() => {
  cleanup();
  localStorageMock.clear();
  vi.clearAllMocks();
  // Reset mutable store properties (mockStoreState is const, so mutate fields)
  mockStoreState.nodes = [];
  mockStoreState.selectedNodeId = null;
  mockStoreState.panelTab = "chat";
  mockStoreState.agentMessages = {};
  mockStoreState.setPanelTab = vi.fn();
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("OnboardingWizard — visibility", () => {
  it("renders nothing when localStorage has the complete flag", () => {
    localStorageMock.getItem.mockReturnValueOnce("true");
    render(<OnboardingWizard />);
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("renders the wizard for first-time users (no localStorage flag)", () => {
    localStorageMock.getItem.mockReturnValueOnce(null);
    render(<OnboardingWizard />);
    expect(screen.getByRole("complementary", { name: "Onboarding guide" })).toBeTruthy();
  });
});

describe("OnboardingWizard — steps", () => {
  beforeEach(() => {
    localStorageMock.getItem.mockReturnValue(null);
  });

  it("renders step 1 'Welcome to Molecule AI' on first paint", () => {
    render(<OnboardingWizard />);
    expect(screen.getByText("Welcome to Molecule AI")).toBeTruthy();
    expect(screen.getByText("Step 1 of 4")).toBeTruthy();
  });

  it("renders the 'Skip guide' button", () => {
    render(<OnboardingWizard />);
    expect(screen.getByRole("button", { name: "Skip onboarding guide" })).toBeTruthy();
  });

  it("renders the progress bar", () => {
    render(<OnboardingWizard />);
    // Progress bar is inside a div
    const bar = document.body.querySelector(".h-full.bg-gradient-to-r");
    expect(bar).toBeTruthy();
    // Step 1 should be 25% wide
    expect(bar?.getAttribute("style")).toContain("25%");
  });

  it("advances to step 2 'Set your API key' when Next is clicked", () => {
    render(<OnboardingWizard />);
    expect(screen.getByText("Welcome to Molecule AI")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Set your API key")).toBeTruthy();
    expect(screen.getByText("Step 2 of 4")).toBeTruthy();
  });

  it("advances to step 3 'Send your first message' when Next is clicked twice", () => {
    render(<OnboardingWizard />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Send your first message")).toBeTruthy();
    expect(screen.getByText("Step 3 of 4")).toBeTruthy();
  });

  it("shows 'Get Started' button on the last step", () => {
    render(<OnboardingWizard />);
    // Navigate to done step
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("You're all set!")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Get Started" })).toBeTruthy();
  });

  it("dismisses the wizard when 'Skip guide' is clicked", () => {
    render(<OnboardingWizard />);
    expect(screen.getByRole("complementary")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Skip onboarding guide" }));
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("persists the dismissed state to localStorage when dismissed", () => {
    render(<OnboardingWizard />);
    fireEvent.click(screen.getByRole("button", { name: "Skip onboarding guide" }));
    expect(localStorageMock.setItem).toHaveBeenCalledWith(STORAGE_KEY, "true");
  });
});

describe("OnboardingWizard — auto-advance", () => {
  beforeEach(() => {
    localStorageMock.getItem.mockReturnValue(null);
  });

  it("auto-advances from welcome to api-key when nodes appear", async () => {
    const { unmount } = render(<OnboardingWizard />);
    expect(screen.getByText("Welcome to Molecule AI")).toBeTruthy();

    // Simulate a node being added to the store and re-render
    mockStoreState.nodes = [{ id: "ws-1", data: {} }];
    render(<OnboardingWizard />);

    await waitFor(() => {
      expect(screen.queryByText("Welcome to Molecule AI")).toBeNull();
    });
    expect(screen.getByText("Set your API key")).toBeTruthy();
    unmount();
  });
});

describe("OnboardingWizard — accessibility", () => {
  beforeEach(() => {
    localStorageMock.getItem.mockReturnValue(null);
  });

  it("has aria-live='polite' region for step announcements", () => {
    render(<OnboardingWizard />);
    const liveRegion = document.body.querySelector('[aria-live="polite"]');
    expect(liveRegion).toBeTruthy();
    expect(liveRegion?.textContent).toMatch(/onboarding step 1/i);
  });

  it("has role=complementary with aria-label", () => {
    render(<OnboardingWizard />);
    expect(screen.getByRole("complementary", { name: "Onboarding guide" })).toBeTruthy();
  });
});
