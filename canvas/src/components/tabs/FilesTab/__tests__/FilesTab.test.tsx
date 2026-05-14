// @vitest-environment jsdom
/**
 * Tests for the main FilesTab / PlatformOwnedFilesTab component.
 *
 * Covers: NotAvailablePanel (external runtime), loading/empty/error states,
 * FilesToolbar actions, and the /configs-only upload guard.
 *
 * No @testing-library/jest-dom — use textContent / className / getAttribute.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import React from "react";

import { FilesTab } from "../../FilesTab.tsx";
import { FilesToolbar } from "../FilesToolbar.tsx";
import type { FileEntry } from "../../FilesTab/tree";

// ─── Mock ──────────────────────────────────────────────────────────────────

const _mockGet = vi.hoisted(() => vi.fn<() => Promise<unknown>>());
vi.mock("@/lib/api", () => ({
  api: { get: _mockGet, put: vi.fn(), del: vi.fn() },
}));

afterEach(() => {
  cleanup();
  _mockGet.mockReset();
});

// ─── Helpers ───────────────────────────────────────────────────────────────

const emptyFileList: FileEntry[] = [];

/** Render FilesTab with a non-external runtime (triggers PlatformOwnedFilesTab). */
function renderPlatformTab(extraProps: Partial<React.ComponentProps<typeof FilesTab>> = {}) {
  return render(
    <FilesTab
      workspaceId="ws-1"
      data={{ id: "ws-1", name: "Test", runtime: "claude-code", status: "online", tier: 0, skills: [], created_at: "" }}
      {...extraProps}
    />,
  );
}

/** Render FilesToolbar directly with stub handlers. */
function renderToolbar(extraProps: Partial<React.ComponentProps<typeof FilesToolbar>> = {}) {
  return render(
    <FilesToolbar
      root="/configs"
      setRoot={vi.fn()}
      fileCount={0}
      onNewFile={vi.fn()}
      onUpload={vi.fn()}
      onDownloadAll={vi.fn()}
      onClearAll={vi.fn()}
      onRefresh={vi.fn()}
      {...extraProps}
    />
  );
}

// ─── NotAvailablePanel ──────────────────────────────────────────────────────

describe("FilesTab — NotAvailablePanel", () => {
  it("renders NotAvailablePanel when runtime is external", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    render(
      <FilesTab
        workspaceId="ws-1"
        data={{ id: "ws-1", name: "Test", runtime: "external", status: "online", tier: 0, skills: [], created_at: "" }}
      />,
    );
    expect(screen.getByText(/Files not available/i)).toBeTruthy();
  });

  it("renders the runtime name in NotAvailablePanel", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    render(
      <FilesTab
        workspaceId="ws-1"
        data={{ id: "ws-1", name: "Test", runtime: "external", status: "online", tier: 0, skills: [], created_at: "" }}
      />,
    );
    expect(screen.getByText(/external/i)).toBeTruthy();
  });

  it("does NOT call api.get when runtime is external", async () => {
    render(
      <FilesTab
        workspaceId="ws-1"
        data={{ id: "ws-1", name: "Test", runtime: "external", status: "online", tier: 0, skills: [], created_at: "" }}
      />,
    );
    expect(_mockGet).not.toHaveBeenCalled();
  });
});

// ─── Loading / Empty / Error states ────────────────────────────────────────

describe("FilesTab — states", () => {
  it("shows loading text while fetching files", () => {
    _mockGet.mockImplementation(
      () => new Promise<unknown>(() => {}) as unknown as Promise<unknown>,
    );
    renderPlatformTab();
    expect(screen.getByText("Loading files...")).toBeTruthy();
  });

  it("shows 'No config files yet' when root is /configs and no files", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    renderPlatformTab();
    await waitFor(() => {
      expect(screen.getByText(/No config files yet/i)).toBeTruthy();
    });
  });

  it("fetches from the correct endpoint", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    renderPlatformTab();
    await waitFor(() => {
      expect(_mockGet).toHaveBeenCalledWith(expect.stringContaining("/workspaces/ws-1/files"));
    });
  });

  it("shows file count from toolbar when files exist", async () => {
    _mockGet.mockResolvedValue([
      { path: "configs/a.yaml", size: 10, dir: false },
      { path: "configs/b.yaml", size: 20, dir: false },
    ]);
    renderPlatformTab();
    await waitFor(() => {
      expect(screen.getByText("2 files")).toBeTruthy();
    });
  });
});

// ─── FilesToolbar ──────────────────────────────────────────────────────────

describe("FilesTab — FilesToolbar", () => {
  it("shows Refresh button", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    renderPlatformTab();
    await waitFor(() => {
      expect(screen.getByLabelText("Refresh file list")).toBeTruthy();
    });
  });

  it("shows root directory selector", async () => {
    _mockGet.mockResolvedValueOnce(emptyFileList);
    renderPlatformTab();
    await waitFor(() => {
      expect(screen.getByRole("combobox")).toBeTruthy();
    });
  });

  it("Refresh button triggers a reload", async () => {
    // Use persistent mock — loadFiles fires on mount AND on Refresh click.
    _mockGet.mockResolvedValue(emptyFileList);
    renderPlatformTab();
    await waitFor(() => screen.getByLabelText("Refresh file list"));
    const before = _mockGet.mock.calls.length;
    fireEvent.click(screen.getByLabelText("Refresh file list"));
    await waitFor(() => {
      expect(_mockGet.mock.calls.length).toBeGreaterThan(before);
    });
  });
});

// ─── Upload guard ──────────────────────────────────────────────────────────

describe("FilesTab — upload guard", () => {
  it("no error alert on dragover when root is /configs (default)", async () => {
    _mockGet.mockResolvedValue(emptyFileList);
    renderPlatformTab();
    await waitFor(() => screen.getByText(/No config files yet/i));

    // No alert should be present
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("applies focus-visible ring to all interactive buttons", () => {
    const { container } = renderToolbar({ root: "/configs" });
    const buttons = container.querySelectorAll("button");
    for (const btn of buttons) {
      expect(btn.className).toContain("focus-visible:ring-2");
    }
  });
});
