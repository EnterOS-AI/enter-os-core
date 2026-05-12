// @vitest-environment jsdom
/**
 * MemoryTab — 42 test cases covering awareness dashboard, KV memory CRUD,
 * and error states.
 *
 * Issue #519: Add 42 test cases for MemoryTab (42 cases).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  fireEvent,
  cleanup,
  act,
} from "@testing-library/react";
import React from "react";

// ── Module-level mocks ────────────────────────────────────────────────────────
// Mock @/lib/env before MemoryTab loads so it sees the stub values.
vi.mock("@/lib/env", () => ({
  NEXT_PUBLIC_AWARENESS_URL: "http://localhost:37800",
}));

// Mock @/lib/api at module level. vi.hoisted() captures the mock function
// references so they are accessible in the test scope after hoisting.
const _mockGet = vi.hoisted(() => vi.fn<() => Promise<unknown[]>>());
const _mockPost = vi.hoisted(() => vi.fn<() => Promise<unknown>>());
const _mockDel = vi.hoisted(() => vi.fn<() => Promise<unknown>>());
vi.mock("@/lib/api", () => ({
  api: {
    get: _mockGet,
    post: _mockPost,
    del: _mockDel,
  },
}));

// Stub window.open so tests don't actually open a window.
const _windowOpen = vi.fn();
vi.stubGlobal("window", {
  ...window,
  open: _windowOpen,
});

import { MemoryTab } from "../MemoryTab";
import { api } from "@/lib/api";

const WS_ID = "ws-test-123";

const MEMORY_ENTRY: Record<string, unknown> = {
  key: "user-preference",
  value: { theme: "dark", language: "en" },
  version: 1,
  expires_at: null,
  updated_at: "2026-04-15T10:00:00Z",
};

const MEMORY_ENTRY_WITH_TTL: Record<string, unknown> = {
  key: "session-token",
  value: "abc123",
  version: 3,
  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  updated_at: "2026-04-15T11:00:00Z",
};

const MEMORY_ENTRY_RAW_STRING: Record<string, unknown> = {
  key: "plain-text",
  value: "hello world",
  version: 1,
  expires_at: null,
  updated_at: "2026-04-15T12:00:00Z",
};

// ── Setup / teardown ────────────────────────────────────────────────────────

beforeEach(() => {
  // Reset all api mock functions to a clean default state between tests.
  _mockGet.mockReset();
  _mockGet.mockResolvedValue([] as unknown[]);
  _mockPost.mockReset();
  _mockPost.mockResolvedValue({} as unknown);
  _mockDel.mockReset();
  _mockDel.mockResolvedValue({} as unknown);
  _windowOpen.mockClear();
});

afterEach(cleanup);

// ── Shared helpers ──────────────────────────────────────────────────────────

/**
 * Render MemoryTab and reveal the entries list by clicking "Show".
 * The component starts with showAdvanced=false (hidden mode); most entry-list
 * tests need to click Show before entries appear.
 *
 * Uses fireEvent.click directly on the button element (not the text span) to
 * ensure React's onClick fires correctly.
 */
async function renderAndShowEntries() {
  render(<MemoryTab workspaceId={WS_ID} />);
  // Wait for the api.get mock to resolve and React to render with entries.
  // 500ms gives enough time for useEffect → setEntries → re-render.
  await new Promise((r) => setTimeout(r, 500));
  fireEvent.click(screen.getByRole("button", { name: /show/i }));
}

/** Configure api.get to resolve with the given entries.
 * Must be called BEFORE render() so the useEffect sees the mock. */
function stubMemoryFetch(entries: unknown[]) {
  _mockGet.mockReset();
  _mockGet.mockResolvedValue(entries as unknown[]);
}

/**
 * Click the memory entry button to expand it.
 * Uses filter-on-all-buttons to avoid getByRole's strict accessible-name
 * matching (which can silently find the wrong element in dense DOM trees).
 */
function expandEntry(key: string) {
  const allBtns = screen.getAllByRole("button");
  const entryBtn = allBtns.find((b) => b.textContent?.includes(key));
  if (!entryBtn) throw new Error(`expandEntry: no button found containing "${key}"`);
  act(() => { fireEvent.click(entryBtn); });
}

// =============================================================================
// Awareness dashboard
// =============================================================================

describe("MemoryTab — awareness dashboard", () => {
  it("shows awareness section on load", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByText("Awareness dashboard")).toBeTruthy();
  });

  it("renders iframe with correct src containing workspaceId", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    const iframe = (await screen.findByTitle(
      "Awareness dashboard",
    )) as HTMLIFrameElement;
    expect(iframe.src).toContain("workspaceId=" + WS_ID);
  });

  it("collapse button hides iframe and shows collapsed state", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByTitle("Awareness dashboard")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /collapse/i }));
    expect(
      await screen.findByText(/awareness dashboard is collapsed/i),
    ).toBeTruthy();
    expect(screen.queryByTitle("Awareness dashboard")).toBeNull();
  });

  it("collapsed state has expand button that re-shows iframe", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /collapse/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /collapse/i }));
    // After collapse there are two "Expand" buttons (header + collapsed banner).
    // Click the one inside the collapsed banner (last in DOM order).
    const expandBtns = await screen.findAllByRole("button", { name: /^expand$/i });
    fireEvent.click(expandBtns[expandBtns.length - 1]);
    expect(await screen.findByTitle("Awareness dashboard")).toBeTruthy();
  });

  it("open button calls window.open with awarenessUrl", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /open/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /open/i }));
    expect(_windowOpen).toHaveBeenCalledWith(
      expect.stringContaining("workspaceId=" + WS_ID),
      "_blank",
      "noopener,noreferrer",
    );
  });

  it("renders awareness status grid with Connected / Mode / Workspace", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByText("Connected")).toBeTruthy();
    expect(await screen.findByText("Workspace")).toBeTruthy();
  });
});

// =============================================================================
// Loading state
// =============================================================================

describe("MemoryTab — loading state", () => {
  it("shows 'Loading memory...' while initial fetch is pending", () => {
    _mockGet.mockReturnValue(new Promise(() => {}) as unknown as Promise<unknown[]>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(screen.getByText("Loading memory...")).toBeTruthy();
  });

  it("does not render memory section while loading", () => {
    _mockGet.mockReturnValue(new Promise(() => {}) as unknown as Promise<unknown[]>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(screen.queryByText("Workspace KV memory")).toBeNull();
  });
});

// =============================================================================
// KV memory — initial load
// =============================================================================

describe("MemoryTab — initial load", () => {
  it("fetches memory entries on mount", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    // Reveal the entries list
    expect(await screen.findByRole("button", { name: /show/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    expect(await screen.findByText("Workspace KV memory")).toBeTruthy();
    expect(api.get).toHaveBeenCalledWith(`/workspaces/${WS_ID}/memory`);
  });

  it("renders workspace KV memory section heading", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    // Heading is visible in hidden mode (above the hidden banner)
    expect(await screen.findByText("Workspace KV memory")).toBeTruthy();
  });

  it("shows advanced mode by default hidden; Refresh / Advanced / + Add buttons visible", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    // Hidden-mode banner is visible with a Show button
    expect(
      await screen.findByText("Advanced workspace memory is hidden"),
    ).toBeTruthy();
    expect(await screen.findByRole("button", { name: /show/i })).toBeTruthy();
    // Action buttons are still visible in the header
    expect(await screen.findByRole("button", { name: /refresh/i })).toBeTruthy();
    expect(await screen.findByRole("button", { name: /advanced/i })).toBeTruthy();
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
  });
});

// =============================================================================
// KV memory — empty state
// =============================================================================

describe("MemoryTab — empty state", () => {
  it("shows 'No memory entries' when entries array is empty (after Show)", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    // Click Show to reveal entries list (advanced mode is hidden by default)
    fireEvent.click(await screen.findByRole("button", { name: /show/i }));
    expect(await screen.findByText("No memory entries")).toBeTruthy();
  });

  it("hidden mode shows 'Advanced workspace memory is hidden' message", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(
      await screen.findByText("Advanced workspace memory is hidden"),
    ).toBeTruthy();
  });
});

// =============================================================================
// KV memory — list rendering
// =============================================================================

describe("MemoryTab — list rendering", () => {
  it("renders a memory entry key in accent/mono text", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
  });

  it("expands an entry on click showing the value as pretty JSON", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(
      await screen.findByText(/"theme":\s*"dark".*?"language":\s*"en"/),
    ).toBeTruthy();
  });

  it("shows raw string value without extra quotes when value is plain string", async () => {
    stubMemoryFetch([MEMORY_ENTRY_RAW_STRING]);
    await renderAndShowEntries();
    expect(await screen.findByText("plain-text")).toBeTruthy();
    expandEntry("plain-text");
    expect(await screen.findByText(/"hello world"/)).toBeTruthy();
  });

  it("renders updated_at timestamp when entry is expanded", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
  });

  it("shows TTL badge when entry has expires_at", async () => {
    stubMemoryFetch([MEMORY_ENTRY_WITH_TTL]);
    await renderAndShowEntries();
    expect(await screen.findByText("session-token")).toBeTruthy();
    expandEntry("session-token");
    expect(await screen.findByText(/ttl/i)).toBeTruthy();
  });

  it("collapse toggle hides the expanded content", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/Updated:/i)).toBeTruthy();
    expandEntry("user-preference");
    expect(screen.queryByText(/Updated:/i)).toBeNull();
  });
});

// =============================================================================
// KV memory — advanced mode toggle
// =============================================================================

describe("MemoryTab — advanced mode toggle", () => {
  it("clicking Advanced hides the list and shows 'hidden' placeholder", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    expect(
      await screen.findByText("Advanced workspace memory is hidden"),
    ).toBeTruthy();
    expect(screen.queryByText("user-preference")).toBeNull();
  });

  it("clicking Show from hidden mode re-displays the list", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    // Hide via Advanced button
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    expect(await screen.findByText("Advanced workspace memory is hidden")).toBeTruthy();
    // Reveal again
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    expect(await screen.findByText("user-preference")).toBeTruthy();
  });

  it("Hide Advanced button appears when in hidden mode", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    // renderAndShowEntries sets showAdvanced=true, so button says "Hide Advanced".
    // Click "Hide Advanced" to toggle back to hidden mode.
    fireEvent.click(screen.getByRole("button", { name: /hide advanced/i }));
    expect(
      await screen.findByText("Advanced workspace memory is hidden"),
    ).toBeTruthy();
  });
});

// =============================================================================
// KV memory — Add entry
// =============================================================================

describe("MemoryTab — add entry", () => {
  it("clicking + Add shows the add form", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    expect(await screen.findByLabelText(/memory value/i)).toBeTruthy();
  });

  it("add form requires a non-empty key", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(await screen.findByText("Key is required")).toBeTruthy();
    expect(api.post).not.toHaveBeenCalled();
  });

  it("add form parses plain text value as-is (not JSON)", async () => {
    stubMemoryFetch([]);
    _mockPost.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "my-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "plain text value" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(api.post).toHaveBeenCalledWith(
      `/workspaces/${WS_ID}/memory`,
      expect.objectContaining({ key: "my-key", value: "plain text value" }),
    );
  });

  it("add form parses JSON value when valid JSON is entered", async () => {
    stubMemoryFetch([]);
    _mockPost.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "json-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: '{"foo": 123}' },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(api.post).toHaveBeenCalledWith(
      `/workspaces/${WS_ID}/memory`,
      expect.objectContaining({ key: "json-key", value: { foo: 123 } }),
    );
  });

  it("add form accepts optional TTL", async () => {
    stubMemoryFetch([]);
    _mockPost.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    // aria-label is "TTL in seconds (optional)"
    expect(await screen.findByLabelText("TTL in seconds (optional)")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "ttl-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "val" },
    });
    fireEvent.change(screen.getByLabelText("TTL in seconds (optional)"), {
      target: { value: "3600" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(api.post).toHaveBeenCalledWith(
      `/workspaces/${WS_ID}/memory`,
      expect.objectContaining({
        key: "ttl-key",
        value: "val",
        ttl_seconds: 3600,
      }),
    );
  });

  it("successful add clears the form and closes it", async () => {
    stubMemoryFetch([]);
    _mockPost.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "new-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "new-val" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    // Form should close
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    expect(screen.queryByLabelText("Memory key")).toBeNull();
  });

  it("add failure shows error in the add form", async () => {
    stubMemoryFetch([]);
    _mockPost.mockRejectedValueOnce(new Error("server error"));
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "bad-key" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "val" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(await screen.findByText("server error")).toBeTruthy();
  });

  it("cancel button closes the add form without posting", async () => {
    stubMemoryFetch([]);
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(screen.queryByLabelText("Memory key")).toBeNull();
    expect(api.post).not.toHaveBeenCalled();
  });
});

// =============================================================================
// KV memory — Edit entry
// =============================================================================

describe("MemoryTab — edit entry", () => {
  // TEMP inline debug
  it("DEBUG check expandEntry via expandEntry function", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();

    const btns = screen.getAllByRole("button");
    console.log("All button texts:", btns.map(b => b.textContent));
    const match = btns.find(b => b.textContent?.includes("user-preference"));
    console.log("Found button:", match?.textContent, "aria-expanded:", match?.getAttribute("aria-expanded"));
    expandEntry("user-preference");
    console.log("After expandEntry aria-expanded:", match?.getAttribute("aria-expanded"));
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
  });

  it("clicking Edit on an expanded entry switches to edit mode", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    // Expand shows "Updated:" + Edit/Delete buttons; click Edit to enter edit mode.
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    expect(await screen.findByLabelText(/edit ttl/i)).toBeTruthy();
  });

  it("edit form pre-populates with current value (pretty JSON for objects)", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    const textarea = screen.getByLabelText(/edit value/i) as HTMLTextAreaElement;
    expect(textarea.value).toContain("theme");
    expect(textarea.value).toContain("dark");
  });

  it("edit form pre-populates raw string value without surrounding quotes", async () => {
    stubMemoryFetch([MEMORY_ENTRY_RAW_STRING]);
    await renderAndShowEntries();
    expect(await screen.findByText("plain-text")).toBeTruthy();
    expandEntry("plain-text");
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    const textarea = screen.getByLabelText(/edit value/i) as HTMLTextAreaElement;
    expect(textarea.value).toBe("hello world");
  });

  it("Save calls POST with the new value and if_match_version", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockPost.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/edit value/i), {
      target: { value: '{"theme": "light"}' },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(api.post).toHaveBeenCalledWith(
      `/workspaces/${WS_ID}/memory`,
      expect.objectContaining({
        key: "user-preference",
        value: { theme: "light" },
        if_match_version: 1,
      }),
    );
  });

  it("409 conflict shows retry hint and reloads entry", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockPost.mockRejectedValueOnce(
      Object.assign(new Error("409 Conflict"), { status: 409 }),
    );
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(
      await screen.findByText(/this entry changed since you opened it/i),
    ).toBeTruthy();
  });

  it("cancel button exits edit mode without posting", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(await screen.findByLabelText(/edit value/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(await screen.findByText(/"theme":/)).toBeTruthy();
    expect(api.post).not.toHaveBeenCalled();
  });
});

// =============================================================================
// KV memory — Delete entry
// =============================================================================

describe("MemoryTab — delete entry", () => {
  it("clicking Delete optimistically removes entry from list", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockDel.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
    act(() => {
      const deleteBtn = Array.from(document.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Delete",
      );
      if (deleteBtn) fireEvent.click(deleteBtn);
    });
    await new Promise(r => setTimeout(r, 300));
    expect(screen.queryByText("user-preference")).toBeNull();
  });

  it("Delete calls DEL with correct path", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockDel.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    expect(api.del).toHaveBeenCalledWith(
      `/workspaces/${WS_ID}/memory/${encodeURIComponent("user-preference")}`,
    );
  });

  it("Delete failure does NOT remove entry from list", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockDel.mockRejectedValueOnce(new Error("forbidden"));
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    expect(await screen.findByText("user-preference")).toBeTruthy();
  });

  it("Delete clears expanded state when deleting the expanded entry", async () => {
    stubMemoryFetch([MEMORY_ENTRY]);
    _mockDel.mockResolvedValueOnce({} as unknown as Promise<unknown>);
    await renderAndShowEntries();
    expect(await screen.findByText("user-preference")).toBeTruthy();
    expandEntry("user-preference");
    expect(await screen.findByText(/updated:/i)).toBeTruthy();
    act(() => {
      // Re-query inside flush so we get post-expansion buttons
      const deleteBtn = Array.from(document.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Delete",
      );
      if (deleteBtn) fireEvent.click(deleteBtn);
    });
    await new Promise(r => setTimeout(r, 300));
    expect(screen.queryByText("user-preference")).toBeNull();
  });
});

// =============================================================================
// KV memory — Refresh
// =============================================================================

describe("MemoryTab — refresh", () => {
  it("Refresh button re-fetches memory entries", async () => {
    const first = [{ key: "a", value: "1", updated_at: "2026-01-01T00:00:00Z" }];
    const second = [
      ...first,
      { key: "b", value: "2", updated_at: "2026-01-01T00:00:00Z" },
    ];
    // Chain two resolved values: first for initial mount, second for Refresh click.
    // Do NOT call renderAndShowEntries (which calls stubMemoryFetch and resets the chain).
    _mockGet
      .mockResolvedValueOnce(first as unknown[])
      .mockResolvedValueOnce(second as unknown[]);
    render(<MemoryTab workspaceId={WS_ID} />);
    await new Promise((r) => setTimeout(r, 500));
    fireEvent.click(screen.getByRole("button", { name: /show/i }));
    expect(await screen.findByText("a")).toBeTruthy();
    expect(screen.queryByText("b")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    expect(await screen.findByText("b")).toBeTruthy();
  });
});

// =============================================================================
// Error states
// =============================================================================

describe("MemoryTab — error states", () => {
  it("shows error banner when initial fetch fails", async () => {
    _mockGet.mockRejectedValueOnce(new Error("internal server error"));
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByText("internal server error")).toBeTruthy();
  });

  it("error is shown in the form when add fails, not as a top-level banner", async () => {
    stubMemoryFetch([]);
    _mockPost.mockRejectedValueOnce(new Error("add failed"));
    render(<MemoryTab workspaceId={WS_ID} />);
    expect(await screen.findByRole("button", { name: /\+ add/i })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /\+ add/i }));
    expect(await screen.findByLabelText("Memory key")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Memory key"), {
      target: { value: "k" },
    });
    fireEvent.change(screen.getByLabelText(/memory value/i), {
      target: { value: "v" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(await screen.findByText("add failed")).toBeTruthy();
  });
});
