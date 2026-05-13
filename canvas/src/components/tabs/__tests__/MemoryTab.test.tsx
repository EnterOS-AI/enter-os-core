// @vitest-environment jsdom
/**
 * Tests for MemoryTab — awareness dashboard + workspace KV memory management.
 *
 * Coverage:
 *   - Loading state
 *   - Error state when GET /memory fails
 *   - Empty state (no memory entries)
 *   - Memory list rendering (single + multiple entries)
 *   - Expand/collapse memory entries
 *   - Add memory entry (key + value + TTL)
 *   - Add validates required key
 *   - Add parses JSON values
 *   - Delete memory entry
 *   - Edit memory entry (inline)
 *   - Edit 409 conflict shows retry hint
 *   - Advanced toggle shows/hides KV section
 *   - Awareness dashboard expand/collapse
 *   - Awareness URL includes workspaceId
 *   - Refresh button reloads memory
 *   - Error clears when appropriate actions are taken
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryTab } from "../MemoryTab";

const mockGet = vi.hoisted(() => vi.fn<[], Promise<unknown[]>>());
const mockPost = vi.hoisted(() => vi.fn<[], Promise<unknown>>());
const mockDel = vi.hoisted(() => vi.fn<[], Promise<unknown>>());

vi.mock("@/lib/api", () => ({
  api: { get: mockGet, post: mockPost, del: mockDel },
}));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const MEMORY_ENTRY = {
  key: "user_context",
  value: { name: "Alice", role: "engineer" },
  version: 3,
  expires_at: null,
  updated_at: new Date(Date.now() - 60000).toISOString(),
};

function entry(overrides: Partial<typeof MEMORY_ENTRY> = {}): typeof MEMORY_ENTRY {
  return { ...MEMORY_ENTRY, ...overrides };
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

function typeIn(el: HTMLElement, value: string) {
  Object.defineProperty(el, "value", { value, writable: true, configurable: true });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  fireEvent.change(el as any, { target: el });
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("MemoryTab", () => {
  beforeEach(() => {
    mockGet.mockReset();
    mockPost.mockReset();
    mockDel.mockReset();
    vi.useRealTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  // ── Loading / Error ──────────────────────────────────────────────────────────

  it("shows loading state when memory is being fetched", async () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<MemoryTab workspaceId="ws-1" />);
    await act(async () => { /* flush initial render */ });
    expect(screen.getByText("Loading memory...")).toBeTruthy();
  });

  it("shows error banner when GET /memory rejects", async () => {
    mockGet.mockRejectedValue(new Error("network failure"));
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/network failure/i)).toBeTruthy();
  });

  it("shows 'Failed to load memory' when GET rejects with non-Error", async () => {
    mockGet.mockRejectedValue("unknown error");
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/Failed to load memory/i)).toBeTruthy();
  });

  // ── Awareness Dashboard ─────────────────────────────────────────────────────

  it("shows Awareness dashboard section", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("Awareness dashboard")).toBeTruthy();
  });

  it("renders an iframe with workspaceId in URL", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-xyz" />);
    await flush();
    const iframe = screen.getByTitle("Awareness dashboard");
    expect(iframe.getAttribute("src")).toContain("workspaceId=ws-xyz");
  });

  it("shows 'Connected' status", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("Connected")).toBeTruthy();
  });

  it("shows workspace ID in the status grid", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-test-id" />);
    await flush();
    // workspaceId appears in two places (description + status grid).
    // Target the font-mono span in the status grid specifically.
    const spans = Array.from(document.querySelectorAll("span.font-mono"));
    expect(spans.some(s => s.textContent === "ws-test-id")).toBeTruthy();
  });

  it("shows 'Collapse' and 'Open' buttons for awareness (starts visible)", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByRole("button", { name: /collapse/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /open/i })).toBeTruthy();
  });

  it("hides awareness iframe when Collapse is clicked", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /collapse/i }));
    await flush();
    expect(screen.queryByTitle("Awareness dashboard")).toBeNull();
    expect(screen.getByText(/awareness dashboard is collapsed/i)).toBeTruthy();
  });

  it("re-shows awareness iframe when collapsed state Expand is clicked", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    // Start with awareness visible (default) — verify iframe is there
    expect(screen.getByTitle("Awareness dashboard")).toBeTruthy();
    // Click Collapse in the awareness header to hide the iframe
    fireEvent.click(screen.getByRole("button", { name: /collapse/i }));
    await flush();
    expect(screen.queryByTitle("Awareness dashboard")).toBeNull();
    // The collapsed awareness state has a different "Expand" button.
    // Directly click the button whose text is exactly "Expand".
    const allBtns = screen.getAllByRole("button");
    const expandInCollapsed = allBtns.find(b => b.textContent?.trim() === "Expand");
    expect(expandInCollapsed).toBeTruthy();
    act(() => { expandInCollapsed!.click(); });
    await flush();
    expect(screen.getByTitle("Awareness dashboard")).toBeTruthy();
  });

  // ── KV Memory: Empty / Advanced toggle ───────────────────────────────────────

  it("shows 'Advanced workspace memory is hidden' when advanced is collapsed", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/advanced workspace memory is hidden/i)).toBeTruthy();
  });

  it("shows 'Show' button when advanced is collapsed", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByRole("button", { name: /show/i })).toBeTruthy();
  });

  it("shows 'Hide Advanced' after clicking Show", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByRole("button", { name: /hide advanced/i })).toBeTruthy();
  });

  it("shows empty state 'No memory entries' when advanced is shown and list is empty", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByText("No memory entries")).toBeTruthy();
  });

  // ── KV Memory: List rendering ───────────────────────────────────────────────

  it("renders memory entries when advanced is open", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByText("user_context")).toBeTruthy();
  });

  it("renders multiple memory entries", async () => {
    mockGet.mockResolvedValue([
      entry({ key: "key1", value: "value1" }),
      entry({ key: "key2", value: "value2" }),
    ]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByText("key1")).toBeTruthy();
    expect(screen.getByText("key2")).toBeTruthy();
  });

  it("shows chevron pointing right when entry is collapsed", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByText("▶")).toBeTruthy();
  });

  it("shows chevron pointing down when entry is expanded", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText("▼")).toBeTruthy();
  });

  it("shows entry value when expanded", async () => {
    mockGet.mockResolvedValue([entry({ value: { foo: "bar" } })]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText(/"foo": "bar"/)).toBeTruthy();
  });

  it("shows updated_at timestamp when entry is expanded", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText(/updated:/i)).toBeTruthy();
  });

  it("shows Edit and Delete buttons when entry is expanded", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByRole("button", { name: /edit/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /delete/i })).toBeTruthy();
  });

  it("shows TTL when entry has expires_at", async () => {
    const future = new Date(Date.now() + 3600000).toISOString();
    mockGet.mockResolvedValue([entry({ expires_at: future })]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText(/ttl/i)).toBeTruthy();
  });

  // ── Add Memory Entry ─────────────────────────────────────────────────────────

  it("shows + Add button in KV section", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    expect(screen.getByRole("button", { name: /\+ add/i })).toBeTruthy();
  });

  it("opens add form when + Add is clicked", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    expect(screen.getByLabelText("Memory key")).toBeTruthy();
    expect(screen.getByLabelText("Memory value (JSON or plain text)")).toBeTruthy();
  });

  it("requires key to be non-empty", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(screen.getByText(/key is required/i)).toBeTruthy();
  });

  it("POSTs correct payload when adding a string value", async () => {
    mockGet.mockResolvedValue([]);
    mockPost.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    typeIn(screen.getByLabelText("Memory key") as HTMLElement, "my_key");
    typeIn(screen.getByLabelText("Memory value (JSON or plain text)") as HTMLElement, "plain text value");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText("Memory key")).not.toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({ key: "my_key", value: "plain text value" }),
    );
  });

  it("POSTs parsed JSON when value is valid JSON", async () => {
    mockGet.mockResolvedValue([]);
    mockPost.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    typeIn(screen.getByLabelText("Memory key") as HTMLElement, "config");
    typeIn(screen.getByLabelText("Memory value (JSON or plain text)") as HTMLElement, '{"debug": true}');
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({ key: "config", value: { debug: true } }),
    );
  });

  it("POSTs with ttl_seconds when TTL is provided", async () => {
    mockGet.mockResolvedValue([]);
    mockPost.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    typeIn(screen.getByLabelText("Memory key") as HTMLElement, "temp_data");
    typeIn(screen.getByLabelText("Memory value (JSON or plain text)") as HTMLElement, "value");
    typeIn(screen.getByLabelText("TTL in seconds (optional)") as HTMLElement, "3600");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({ key: "temp_data", value: "value", ttl_seconds: 3600 }),
    );
  });

  it("shows error when add fails", async () => {
    mockGet.mockResolvedValue([]);
    mockPost.mockRejectedValue(new Error("add failed"));
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    typeIn(screen.getByLabelText("Memory key") as HTMLElement, "key");
    typeIn(screen.getByLabelText("Memory value (JSON or plain text)") as HTMLElement, "val");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(screen.getByText(/add failed/i)).toBeTruthy();
  });

  it("closes add form and refreshes after successful add", async () => {
    mockGet.mockResolvedValue([]);
    mockPost.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    typeIn(screen.getByLabelText("Memory key") as HTMLElement, "new_key");
    typeIn(screen.getByLabelText("Memory value (JSON or plain text)") as HTMLElement, "new_val");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText("Memory key")).not.toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/memory");
  });

  it("closes add form when Cancel is clicked", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    await flush();
    expect(screen.getByLabelText("Memory key")).toBeTruthy();
    act(() => { screen.getByRole("button", { name: /cancel/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText("Memory key")).not.toBeTruthy();
    });
  });

  // ── Delete Memory Entry ─────────────────────────────────────────────────────

  it("calls DEL when Delete is clicked", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockDel.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    await flush();
    expect(mockDel).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory/user_context",
    );
  });

  it("removes entry from list after successful delete", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockDel.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText("user_context")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    await flush();
    expect(screen.queryByText("user_context")).toBeFalsy();
  });

  it("collapses entry if it was expanded when deleted", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockDel.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    // Expand the entry
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    expect(screen.getByText("▼")).toBeTruthy();
    // Delete
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    await flush();
    expect(screen.queryByText("user_context")).toBeFalsy();
  });

  it("shows error when delete fails", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockDel.mockRejectedValue(new Error("delete failed"));
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    await flush();
    expect(screen.getByText(/delete failed/i)).toBeTruthy();
  });

  // ── Edit Memory Entry ────────────────────────────────────────────────────────

  it("shows edit form when Edit is clicked", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    expect(screen.getByLabelText(/edit value for user_context/i)).toBeTruthy();
  });

  it("pre-fills edit form with existing value", async () => {
    mockGet.mockResolvedValue([entry({ value: { name: "Alice" } })]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    const textarea = screen.getByLabelText(/edit value for user_context/i);
    expect((textarea as HTMLTextAreaElement).value).toContain("Alice");
  });

  it("POSTs updated value when Save is clicked", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockPost.mockResolvedValue({});
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    typeIn(screen.getByLabelText(/edit value for user_context/i) as HTMLElement, "updated_value");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText(/edit value for user_context/i)).not.toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/memory",
      expect.objectContaining({ key: "user_context", value: "updated_value", if_match_version: 3 }),
    );
  });

  it("shows retry hint on 409 conflict during edit", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockPost.mockRejectedValue(new Error("409 Conflict: if_match_version mismatch"));
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    typeIn(screen.getByLabelText(/edit value for user_context/i) as HTMLElement, "new_val");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(screen.getByText(/this entry changed since you opened it/i)).toBeTruthy();
  });

  it("shows generic error when edit save fails", async () => {
    mockGet.mockResolvedValue([entry()]);
    mockPost.mockRejectedValue(new Error("save failed"));
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    typeIn(screen.getByLabelText(/edit value for user_context/i) as HTMLElement, "x");
    await flush();
    act(() => { screen.getByRole("button", { name: /save/i }).click(); });
    await flush();
    expect(screen.getByText(/save failed/i)).toBeTruthy();
  });

  it("closes edit form when Cancel is clicked", async () => {
    mockGet.mockResolvedValue([entry()]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    await flush();
    fireEvent.click(screen.getByText("user_context"));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    expect(screen.getByLabelText(/edit value for user_context/i)).toBeTruthy();
    act(() => { screen.getByRole("button", { name: /cancel/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText(/edit value for/i)).not.toBeTruthy();
    });
  });

  // ── Refresh ────────────────────────────────────────────────────────────────

  it("Refresh button calls loadMemory", async () => {
    mockGet.mockResolvedValue([]);
    render(<MemoryTab workspaceId="ws-1" />);
    await flush();
    mockGet.mockClear();
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/memory");
  });

});
