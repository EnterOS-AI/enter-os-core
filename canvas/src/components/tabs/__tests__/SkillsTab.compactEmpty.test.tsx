// @vitest-environment jsdom
//
// Pins the compact-when-empty layout for the SkillsTab Plugins section
// (issue #2971, reported on production 2026-05-05).
//
// Three states matter for layout:
//   1. installed.length === 0 + registry closed + load completed → COMPACT pill
//   2. installed.length > 0  → FULL panel + installed list
//   3. registry open (showRegistry=true) → FULL panel + registry browser
//
// The compact-empty path is the new behavior; the other two were
// pre-existing. This test pins all three so a future refactor that
// over-collapses (showing compact when plugins are installed) or
// over-expands (showing full panel on empty load) fails loudly.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string, opts?: unknown) => apiGet(path, opts),
    post: vi.fn(() => Promise.resolve({})),
    del: vi.fn(),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

beforeEach(() => {
  apiGet.mockReset();
  Element.prototype.scrollIntoView = vi.fn();
});

import { SkillsTab } from "../SkillsTab";

const minimalData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: "",
  agentCard: undefined,
} as unknown as Parameters<typeof SkillsTab>[0]["data"];

describe("SkillsTab Plugins compact-empty layout", () => {
  it("renders compact pill when installed.length === 0 and registry closed", async () => {
    // Both fetches return empty arrays — workspace is fresh, no plugins.
    apiGet.mockImplementation((path: string) => {
      if (path.endsWith("/plugins") || path === "/plugins" || path === "/plugins/sources") {
        return Promise.resolve([]);
      }
      return Promise.resolve([]);
    });
    render(<SkillsTab workspaceId="ws-fresh" data={minimalData} />);

    // Wait for the installedLoaded gate to flip — without that the
    // component renders a "loading" state, not the compact pill.
    await waitFor(() => {
      expect(screen.getByLabelText(/Plugins \(none installed\)/i)).toBeTruthy();
    });

    // Compact assertions: the rounded-xl panel chrome MUST NOT be in
    // the DOM (we'd see two "Plugins" labels — one in the header,
    // one in the pill — if the layout regressed to "always full
    // panel"). The compact form has exactly one "Plugins" label.
    const labels = screen.getAllByText("Plugins");
    expect(labels).toHaveLength(1);

    // The full-panel chrome's id="plugins-section" should NOT be
    // rendered when we're in compact mode.
    expect(document.getElementById("plugins-section")).toBeNull();
  });

  it("renders full panel when installed.length > 0", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path.endsWith("/plugins")) {
        return Promise.resolve([
          { name: "memory-postgres", version: "1.0.0", description: "memory backend", supported_on_runtime: true },
        ]);
      }
      return Promise.resolve([]);
    });
    render(<SkillsTab workspaceId="ws-installed" data={minimalData} />);

    await waitFor(() => {
      expect(screen.getByText(/1 installed/i)).toBeTruthy();
    });

    // Full-panel chrome MUST be present — id pin.
    expect(document.getElementById("plugins-section")).not.toBeNull();
    // Compact pill ariaLabel MUST NOT be present.
    expect(screen.queryByLabelText(/Plugins \(none installed\)/i)).toBeNull();
  });

  it("expands to full panel when user clicks + Install Plugin from compact pill", async () => {
    apiGet.mockImplementation(() => Promise.resolve([]));
    render(<SkillsTab workspaceId="ws-expand" data={minimalData} />);

    // Start compact — wait for the compact pill to settle so we click
    // the right button (initial render before installedLoaded flips
    // doesn't have either layout, and the post-load compact pill is
    // what we want to interact with).
    await waitFor(() => {
      expect(screen.getByLabelText(/Plugins \(none installed\)/i)).toBeTruthy();
    });
    const installBtn = screen.getByRole("button", { name: /\+ Install Plugin/i });
    expect(installBtn.getAttribute("aria-expanded")).toBe("false");

    fireEvent.click(installBtn);

    // After click, registry opens → full panel renders. The compact
    // pill's aria-label should be gone; the full-panel id should
    // appear. Generous waitFor — a registry fetch may also fire in
    // the React effect chain, and we want to assert the compact →
    // full transition without racing it.
    await waitFor(
      () => {
        expect(document.getElementById("plugins-section")).not.toBeNull();
      },
      { timeout: 3000 },
    );
    expect(screen.queryByLabelText(/Plugins \(none installed\)/i)).toBeNull();
  });

  it("does NOT collapse to compact while initial load is pending (avoid flash)", () => {
    // Returning a never-resolving promise means installedLoaded stays
    // false. The compact pill MUST NOT render in this state — that
    // would flash compact → full as the load completes, which looks
    // janky. The component shows a loading shell instead (the
    // existing pre-fix behavior).
    apiGet.mockImplementation(() => new Promise(() => {}));
    render(<SkillsTab workspaceId="ws-loading" data={minimalData} />);

    // Synchronous assertion — no waitFor — since we want to confirm
    // the compact pill is NOT rendered before any network round-trip
    // finishes.
    expect(screen.queryByLabelText(/Plugins \(none installed\)/i)).toBeNull();
  });
});
