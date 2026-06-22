// @vitest-environment jsdom
//
// Regression guard for the Plugins-tab async loading states (user-
// reported: the tab shows the EMPTY state — "0 installed", registry
// "Registry returned 0 plugins" — while the fetch is still in flight,
// so it looks broken until data arrives a second later).
//
// Pins, for each async section:
//   - installed plugins: skeleton while pending, NOT the "0 installed"
//     empty look; rows on resolve-with-data; empty (compact pill) on
//     resolve-empty.
//   - install-dialog registry: skeleton while pending, registry rows
//     when the /plugins fetch returns entries (the "no registry / still
//     no registry" report — confirm the dialog lists the registry).
//
// A future refactor that re-introduces the empty-during-load flash, or
// drops the registry list from the dialog, fails here.

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

// Returns a controllable deferred promise so a test can hold a fetch
// "in flight" and assert the loading state before resolving.
function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

const REGISTRY_PLUGINS = [
  { name: "browser-automation", version: "0.2.0", description: "Drive a headless browser", tags: ["web"], skills: [], author: "" },
  { name: "image-gen", version: "0.1.1", description: "Generate images", tags: ["media"], skills: [], author: "" },
];

describe("SkillsTab Plugins async loading states", () => {
  it("shows a loading skeleton (not the empty state) while installed plugins are in flight", async () => {
    const installedDef = deferred<unknown>();
    apiGet.mockImplementation((path: string) => {
      if (path === `/workspaces/ws-1/plugins`) return installedDef.promise;
      return Promise.resolve([]); // /plugins, /plugins/sources
    });

    render(<SkillsTab workspaceId="ws-1" data={minimalData} />);

    // While the installed fetch is pending: skeleton present, and the
    // compact "0 installed" empty pill MUST NOT be shown.
    await waitFor(() => {
      expect(screen.getByTestId("plugin-skeleton")).toBeTruthy();
    });
    expect(screen.queryByLabelText(/Plugins \(none installed\)/i)).toBeNull();

    // Resolve empty → the skeleton goes away and the empty (compact)
    // state appears only AFTER the fetch resolves.
    installedDef.resolve([]);
    await waitFor(() => {
      expect(screen.getByLabelText(/Plugins \(none installed\)/i)).toBeTruthy();
    });
    expect(screen.queryByTestId("plugin-skeleton")).toBeNull();
  });

  it("shows installed rows after the fetch resolves with data", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === `/workspaces/ws-2/plugins`) {
        return Promise.resolve([
          { name: "memory-postgres", version: "1.0.0", description: "memory backend", supported_on_runtime: true },
        ]);
      }
      return Promise.resolve([]);
    });

    render(<SkillsTab workspaceId="ws-2" data={minimalData} />);

    await waitFor(() => {
      expect(screen.getByText(/1 installed/i)).toBeTruthy();
    });
    expect(screen.getByText("memory-postgres")).toBeTruthy();
    // No skeleton lingering after resolve.
    expect(screen.queryByTestId("plugin-skeleton")).toBeNull();
  });

  it("install dialog shows a skeleton while the registry is in flight, then lists registry plugins", async () => {
    const registryDef = deferred<unknown>();
    apiGet.mockImplementation((path: string) => {
      if (path === "/plugins") return registryDef.promise;
      // installed resolves empty immediately so the compact pill renders
      // and we can click "+ Install Plugin".
      if (path === `/workspaces/ws-3/plugins`) return Promise.resolve([]);
      return Promise.resolve([]); // /plugins/sources
    });

    render(<SkillsTab workspaceId="ws-3" data={minimalData} />);

    // Open the install dialog from the compact pill.
    await waitFor(() => {
      expect(screen.getByLabelText(/Plugins \(none installed\)/i)).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: /\+ Install Plugin/i }));

    // Dialog open, registry still loading → skeleton, NOT the "Registry
    // returned 0 plugins" empty banner.
    await waitFor(() => {
      expect(screen.getByTestId("plugin-skeleton")).toBeTruthy();
    });
    expect(screen.queryByText(/Registry returned 0 plugins/i)).toBeNull();

    // Registry resolves with entries → the dialog lists them (name +
    // version + description), and the skeleton is gone.
    registryDef.resolve(REGISTRY_PLUGINS);
    await waitFor(() => {
      expect(screen.getByText("browser-automation")).toBeTruthy();
    });
    expect(screen.getByText("image-gen")).toBeTruthy();
    expect(screen.getByText("v0.2.0")).toBeTruthy();
    expect(screen.getByText("Generate images")).toBeTruthy();
    expect(screen.queryByTestId("plugin-skeleton")).toBeNull();
    // The empty banner must NOT show when the registry has entries.
    expect(screen.queryByText(/Registry returned 0 plugins/i)).toBeNull();
  });

  it("shows the empty registry banner only after the registry resolves with zero entries", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === `/workspaces/ws-4/plugins`) return Promise.resolve([]);
      return Promise.resolve([]); // /plugins resolves empty, /plugins/sources
    });

    render(<SkillsTab workspaceId="ws-4" data={minimalData} />);

    await waitFor(() => {
      expect(screen.getByLabelText(/Plugins \(none installed\)/i)).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: /\+ Install Plugin/i }));

    await waitFor(() => {
      expect(screen.getByText(/Registry returned 0 plugins/i)).toBeTruthy();
    });
    expect(screen.queryByTestId("plugin-skeleton")).toBeNull();
  });
});
