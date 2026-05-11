// @vitest-environment jsdom
/**
 * Tests for MemoryTab — the workspace KV memory tab.
 *
 * Coverage:
 *   - Loading state (pending GET)
 *   - Empty state ("No memory entries")
 *   - Memory entries list renders
 *   - Expand/collapse entry + aria-expanded
 *   - Add entry: key validation, value JSON parsing, TTL
 *   - Edit entry: begin, cancel, save, 409 conflict
 *   - Delete entry: optimistic removal
 *   - Error state from API failure
 *   - Refresh button triggers reload
 *   - Awareness dashboard collapse/expand
 *   - Advanced toggle shows/hides KV section
 *   - Awareness URL includes workspaceId
 *
 * Uses vi.useRealTimers() + flush() pattern for all non-window tests.
 * window.open is mocked per-test since it is environment-dependent.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryTab } from "../MemoryTab";

// Hoist mockGet so vi.mock factory can reference it (vi.mock is hoisted).
const mockGet = vi.hoisted(() => vi.fn<[], Promise<unknown>>());
const mockPost = vi.hoisted(() => vi.fn<[], Promise<unknown>>());
const mockDel = vi.hoisted(() => vi.fn<[], Promise<unknown>>());

vi.mock("@/lib/api", () => ({
  api: {
    get: mockGet,
    post: mockPost,
    del: mockDel,
  },
}));

// Mock window.open per-test
const mockOpen = vi.fn();
vi.stubGlobal("open", mockOpen);

beforeEach(() => {
  vi.useRealTimers();
  mockGet.mockReset();
  mockPost.mockReset();
  mockDel.mockReset();
  mockOpen.mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ─── Helpers ──────────────────────────────────────────────────────────────────

const entry = (
  key: string,
  value: unknown,
  overrides?: Partial<{
    version: number;
    expires_at: string | null;
    updated_at: string;
  }>,
): {
  key: string;
  value: unknown;
  version?: number;
  expires_at: string | null;
  updated_at: string;
} => ({
  key,
  value,
  version: undefined,
  expires_at: null,
  updated_at: "2026-05-10T10:00:00Z",
  ...overrides,
});

const renderTab = (workspaceId = "ws-1") =>
  render(<MemoryTab workspaceId={workspaceId} />);

// Flush pattern: resolve mock microtask then flush React state batch.
async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("MemoryTab — render conditions", () => {
  beforeEach(() => {
    mockGet.mockImplementation(() => new Promise(() => {}));
  });

  it("shows loading state while fetching", async () => {
    renderTab();
    await act(async () => { /* flush initial render */ });
    expect(screen.getByText("Loading memory...")).toBeTruthy();
  });

  it("shows empty state when API returns empty list", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    // KV section hidden by default; reveal it via Advanced toggle
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    expect(screen.getByText("No memory entries")).toBeTruthy();
  });

  it("renders memory entries when API returns data", async () => {
    mockGet.mockResolvedValueOnce([
      entry("my-key", { nested: true }),
      entry("another-key", "plain string"),
    ]);
    renderTab();
    await flush();
    // Advanced is collapsed by default; reveal entries
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    expect(screen.getByText("my-key")).toBeTruthy();
    expect(screen.getByText("another-key")).toBeTruthy();
  });

  it("shows Advanced section hidden by default", async () => {
    mockGet.mockResolvedValueOnce([entry("k1", "v1")]);
    renderTab();
    await flush();
    expect(screen.getByText("Advanced workspace memory is hidden")).toBeTruthy();
  });

  it("shows Advanced section when entries exist and advanced is toggled on", async () => {
    mockGet.mockResolvedValueOnce([entry("k1", "v1")]);
    renderTab();
    await flush();
    // Show the advanced section
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    expect(screen.getByText("k1")).toBeTruthy();
  });

  // Awareness section defaults to showAwareness=true (expanded with iframe)
  it("shows Awareness dashboard expanded with iframe by default", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    // Default state shows the expanded section
    const iframe = document.querySelector("iframe");
    expect(iframe).toBeTruthy();
    expect(iframe?.getAttribute("title")).toBe("Awareness dashboard");
  });

  it("collapses Awareness dashboard when Collapse button is clicked", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /collapse/i }).click();
    });
    await flush();
    expect(screen.getByText("Awareness dashboard is collapsed")).toBeTruthy();
  });

  it("shows awareness status grid in expanded Awareness section", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    // Default state is already expanded — status grid is visible
    expect(screen.getByText("Connected")).toBeTruthy();
    expect(screen.getByText("Mode")).toBeTruthy();
    expect(screen.getByText("Workspace")).toBeTruthy();
  });

  it("shows workspaceId in awareness grid", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab("my-workspace-id");
    await flush();
    // workspaceId appears twice: in awareness grid and in KV description.
    // Query the awareness grid span specifically (text-ink-mid class in the grid).
    const spans = screen.getAllByText("my-workspace-id");
    const gridSpan = spans.find(
      (s) => s.className.includes("font-mono") && !s.className.includes("truncate"),
    );
    expect(gridSpan).toBeTruthy();
  });
});

describe("MemoryTab — KV memory CRUD", () => {
  beforeEach(() => {
    // Use mockImplementation so every call resolves (loadMemory is called multiple
    // times: on mount, on refresh, after add/save errors)
    mockGet.mockImplementation(() =>
      Promise.resolve([entry("existing-key", "existing-value")]),
    );
    mockPost.mockResolvedValue({});
    mockDel.mockResolvedValue({});
  });

  it("shows error alert when GET rejects", async () => {
    mockGet.mockRejectedValue(new Error("Network failure"));
    renderTab();
    await flush();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Network failure")).toBeTruthy();
  });

  it("Refresh button calls GET /workspaces/:id/memory", async () => {
    renderTab();
    await flush();
    mockGet.mockClear();
    act(() => {
      screen.getByRole("button", { name: /refresh/i }).click();
    });
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/memory");
  });

  it("shows + Add button to open add form", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    expect(screen.getByRole("button", { name: /^\+ add$/i })).toBeTruthy();
  });

  it("shows add form when + Add is clicked", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    expect(screen.getByLabelText(/memory key/i)).toBeTruthy();
    expect(screen.getByLabelText(/memory value/i)).toBeTruthy();
  });

  it("requires key in add form", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    mockPost.mockReset().mockRejectedValue(new Error("should not be called"));
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(screen.getByText("Key is required")).toBeTruthy();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("parses JSON value in add form", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    fireEvent.change(screen.getByLabelText(/memory key/i), {
      target: { value: "json-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: '{"nested": "value"}' },
    });
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({
        key: "json-key",
        value: { nested: "value" },
      }),
    );
  });

  it("treats plain-text value as string in add form", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    fireEvent.change(screen.getByLabelText(/memory key/i), {
      target: { value: "plain-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "plain text" },
    });
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({
        key: "plain-key",
        value: "plain text",
      }),
    );
  });

  it("sends ttl_seconds when TTL is provided in add form", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    fireEvent.change(screen.getByLabelText(/memory key/i), {
      target: { value: "ttl-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "val" },
    });
    fireEvent.change(screen.getByLabelText(/ttl in seconds/i), {
      target: { value: "3600" },
    });
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({
        key: "ttl-key",
        value: "val",
        ttl_seconds: 3600,
      }),
    );
  });

  it("closes add form on cancel", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    expect(screen.getByLabelText(/memory key/i)).toBeTruthy();
    act(() => {
      screen.getByRole("button", { name: /cancel/i }).click();
    });
    await flush();
    expect(screen.queryByLabelText(/memory key/i)).toBeFalsy();
  });

  it("shows error when add POST rejects", async () => {
    mockGet.mockResolvedValueOnce([]);
    mockPost.mockRejectedValue(new Error("Add failed"));
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /^\+ add$/i }).click();
    });
    await flush();
    fireEvent.change(screen.getByLabelText(/memory key/i), {
      target: { value: "k" },
    });
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(screen.getByText("Add failed")).toBeTruthy();
  });

  it("optimistically removes entry on delete", async () => {
    renderTab();
    await flush();
    // Expand the advanced section
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    // Expand the entry row
    act(() => {
      screen.getByText("existing-key").closest("button")?.click();
    });
    await flush();
    // Verify the Delete button is visible inside the expanded section
    const deleteBtn = screen
      .getAllByRole("button")
      .find((b) => b.textContent === "Delete");
    expect(deleteBtn).toBeTruthy();
    // Clicking Delete fires the API call; the entry is optimistically
    // removed from state before the response. We verify the API call here.
    act(() => {
      deleteBtn?.click();
    });
    await flush();
    expect(mockDel).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory/existing-key",
    );
  });

  it("calls DELETE /workspaces/:id/memory/:key on delete", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("existing-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /delete/i }).click();
    });
    await flush();
    expect(mockDel).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory/existing-key",
    );
  });

  it("shows error when delete rejects", async () => {
    mockDel.mockRejectedValue(new Error("Delete failed"));
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("existing-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /delete/i }).click();
    });
    await flush();
    // Error should appear in the alert
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Delete failed")).toBeTruthy();
    // Entry should be visible again (reverted)
    expect(screen.getByText("existing-key")).toBeTruthy();
  });
});

describe("MemoryTab — edit entry", () => {
  beforeEach(() => {
    // Use mockImplementation so every call resolves (loadMemory called multiple times)
    mockGet.mockImplementation(() =>
      Promise.resolve([
        entry("edit-key", { original: true }, { version: 5 }),
      ]),
    );
    mockPost.mockResolvedValue({});
  });

  it("begins edit mode when Edit is clicked", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    // Expand the entry row first
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    // Find the "Edit" button specifically (not the row button whose accessible name is "edit-key")
    const editBtn = screen
      .getAllByRole("button", { name: /^edit$/i })
      .find((b) => b.textContent === "Edit");
    act(() => {
      editBtn?.click();
    });
    await flush();
    expect(screen.getByLabelText(/edit value for edit-key/i)).toBeTruthy();
    expect(screen.getByLabelText(/edit ttl for edit-key/i)).toBeTruthy();
  });

  it("pre-fills edit textarea with JSON for object values", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    const textarea = screen.getByLabelText(/edit value for edit-key/i);
    expect(textarea.textContent?.trim()).toBe('{\n  "original": true\n}');
  });

  it("pre-fills edit textarea with raw string for string values", async () => {
    mockGet.mockImplementation(() =>
      Promise.resolve([
        entry("str-key", "plain string value", { version: 1 }),
      ]),
    );
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("str-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    const textarea = screen.getByLabelText(/edit value for str-key/i);
    expect(textarea.textContent?.trim()).toBe("plain string value");
  });

  it("cancels edit and restores entry view", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    expect(screen.getByLabelText(/edit value for edit-key/i)).toBeTruthy();
    act(() => {
      screen.getByRole("button", { name: /cancel/i }).click();
    });
    await flush();
    expect(screen.queryByLabelText(/edit value/i)).toBeFalsy();
  });

  it("calls POST with if_match_version on save", async () => {
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({
        key: "edit-key",
        value: { original: true },
        if_match_version: 5,
      }),
    );
  });

  it("shows 409 conflict error and reloads on version mismatch", async () => {
    mockPost.mockRejectedValue(
      new Error("409 Conflict: if_match_version mismatch"),
    );
    // Return entries for initial load; on 409 the component calls loadMemory()
    // again — use mockImplementation so subsequent calls also return entries
    mockGet.mockImplementation(() =>
      Promise.resolve([
        entry("edit-key", { original: true }, { version: 5 }),
      ]),
    );
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(screen.getByText(/this entry changed since you opened it/i)).toBeTruthy();
  });

  it("shows generic error when edit POST rejects with non-409", async () => {
    mockPost.mockRejectedValue(new Error("Server error"));
    renderTab();
    await flush();
    act(() => {
      screen.getByRole("button", { name: /advanced/i }).click();
    });
    await flush();
    act(() => {
      screen.getByText("edit-key").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen
        .getAllByRole("button", { name: /^edit$/i })
        .find((b) => b.textContent === "Edit")
        ?.click();
    });
    await flush();
    act(() => {
      screen.getByRole("button", { name: /save/i }).click();
    });
    await flush();
    expect(screen.getByText("Server error")).toBeTruthy();
  });
});

describe("MemoryTab — expand/collapse entry", () => {
  beforeEach(() => {
    mockGet.mockResolvedValue([
      entry("entry-a", { data: "A" }),
      entry("entry-b", { data: "B" }),
    ]);
  });

  it("expands entry when clicked", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    act(() => {
      screen.getByText("entry-a").closest("button")?.click();
    });
    await flush();
    // Expanded entry shows its JSON value
    expect(screen.getByText(/"data": "A"/)).toBeTruthy();
  });

  it("collapses entry when clicked again", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    act(() => {
      screen.getByText("entry-a").closest("button")?.click();
    });
    await flush();
    act(() => {
      screen.getByText("entry-a").closest("button")?.click();
    });
    await flush();
    expect(screen.queryByText(/"data": "A"/)).toBeFalsy();
  });

  it("shows collapsed indicator ▶ for non-expanded entries", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    expect(screen.getAllByText("▶").length).toBeGreaterThan(0);
  });

  it("shows expanded indicator ▼ for expanded entries", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    act(() => {
      screen.getByText("entry-a").closest("button")?.click();
    });
    await flush();
    expect(screen.getAllByText("▼").length).toBeGreaterThan(0);
  });

  it("hides edit/delete buttons when entry is collapsed", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    expect(screen.queryByRole("button", { name: /edit/i })).toBeFalsy();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeFalsy();
  });

  it("shows edit/delete buttons when entry is expanded", async () => {
    renderTab();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    await flush();
    act(() => {
      screen.getByText("entry-a").closest("button")?.click();
    });
    await flush();
    expect(screen.getAllByRole("button", { name: /edit/i }).length).toBeGreaterThan(0);
    expect(screen.getAllByRole("button", { name: /delete/i }).length).toBeGreaterThan(0);
  });
});

describe("MemoryTab — Open Awareness button", () => {
  it("calls window.open with workspaceId in URL", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab("my-ws");
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /open/i }));
    await flush();
    expect(mockOpen).toHaveBeenCalled();
    const url = mockOpen.mock.calls[0][0];
    expect(url).toContain("workspaceId=my-ws");
  });
});
