// @vitest-environment jsdom
//
// Pins the "Files not available" early-return for runtimes whose
// filesystem the platform doesn't own (today: runtime === "external").
//
// Pre-fix: FilesTab issued a GET /workspaces/<id>/files for every
// workspace. The platform's response for an external workspace is
// always [] (no rows in workspace_files), but the canvas rendered
// "0 files / No config files yet" — visually identical to the SaaS
// empty-listing bug fixed in PR-A. The placeholder makes the absence
// intentional.
//
// Pinned branches:
//   1. external runtime → "Files not available" banner renders,
//      runtime name surfaces in the body so user knows WHY.
//   2. external runtime → useFilesApi is NOT invoked. Verified by
//      asserting the mocked api.get was never called.
//   3. claude-code (or any other runtime) → no banner, normal mount
//      proceeds (`/configs` toolbar visible). Pre-fix regression cover.
//   4. data prop omitted (legacy callers) → no early-return, falls
//      through to normal mount.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// Mock the api module so the normal-mount branches don't try to
// fetch against a real backend — and so we can assert the
// external-runtime branch never fires a request.
const apiCalls: string[] = [];
vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn((path: string) => {
      apiCalls.push(path);
      return Promise.resolve([]);
    }),
    put: vi.fn(() => Promise.resolve()),
    del: vi.fn(() => Promise.resolve()),
  },
}));

// useCanvasStore is referenced by useFilesApi for the needsRestart
// flag. The Toaster import inside FilesTab also pulls the store
// indirectly. Stub minimally to satisfy the import chain.
vi.mock("@/store/canvas", async () => {
  const actual = await vi.importActual<typeof import("@/store/canvas")>(
    "@/store/canvas",
  );
  return {
    ...actual,
    useCanvasStore: {
      getState: () => ({
        updateNodeData: vi.fn(),
      }),
    },
  };
});

vi.mock("../Toaster", () => ({
  showToast: vi.fn(),
}));

beforeEach(() => {
  apiCalls.length = 0;
});

import { FilesTab } from "../FilesTab";

const externalData = { runtime: "external", status: "online" } as unknown as Parameters<
  typeof FilesTab
>[0]["data"];

const claudeData = { runtime: "claude-code", status: "online" } as unknown as Parameters<
  typeof FilesTab
>[0]["data"];

describe("FilesTab not-available early-return for runtimes without platform-owned filesystem", () => {
  it("external runtime renders the not-available banner with runtime name", () => {
    render(<FilesTab workspaceId="ws-ext" data={externalData} />);
    expect(screen.getByText(/Files not available/i)).not.toBeNull();
    // Runtime name must surface so the user understands WHY — without
    // it the placeholder reads as a generic error.
    expect(screen.getByText(/external/)).not.toBeNull();
    // Chat tab is the recommended alternative — flagged in copy so the
    // user knows where to go next instead of bouncing tabs.
    expect(screen.getByText(/Chat tab/i)).not.toBeNull();
  });

  it("external runtime does NOT issue any /files API call", async () => {
    render(<FilesTab workspaceId="ws-ext" data={externalData} />);
    // Tolerate one microtask boundary in case useEffect schedules.
    await new Promise((r) => setTimeout(r, 0));
    const filesCalls = apiCalls.filter((p) => p.includes("/files"));
    expect(filesCalls).toEqual([]);
  });

  it("claude-code runtime does NOT render the banner (normal mount)", async () => {
    render(<FilesTab workspaceId="ws-claude" data={claudeData} />);
    // The normal-mount path renders the FilesToolbar with the root
    // selector. Wait for it (useEffect → loadFiles → setLoading false).
    await waitFor(() => {
      expect(screen.queryByText(/Files not available/i)).toBeNull();
    });
    // Toolbar's root selector confirms we're on the platform-owned
    // rendering path, not the placeholder.
    expect(screen.getByLabelText(/File root directory/i)).not.toBeNull();
  });

  it("data prop omitted falls through to normal mount (back-compat)", async () => {
    render(<FilesTab workspaceId="ws-no-data" />);
    await waitFor(() => {
      expect(screen.queryByText(/Files not available/i)).toBeNull();
    });
    // Without data we can't gate on runtime — must mount normally.
    expect(screen.getByLabelText(/File root directory/i)).not.toBeNull();
  });
});
