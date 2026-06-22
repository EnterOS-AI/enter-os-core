// @vitest-environment jsdom
/**
 * WCAG 2.1 / Issue M — ConversationTraceModal accessibility
 *
 * Migrated from custom <div> to Radix Dialog, which provides:
 *   - role="dialog" + aria-modal="true" automatically (WCAG 4.1.2)
 *   - aria-labelledby pointing to Dialog.Title (WCAG 1.3.1)
 *   - Focus trap (WCAG 2.1.2 / 2.4.3)
 *   - Escape key closes the dialog (WCAG 2.1.1)
 *   - ✕ close button has aria-label="Close conversation trace"
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ── Mocks must be declared before importing the component ────────────────────

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn().mockResolvedValue([]),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: (selector: (s: { nodes: unknown[] }) => unknown) =>
    selector({ nodes: [] }),
}));

vi.mock("@/hooks/useWorkspaceName", () => ({
  useWorkspaceName: () => () => "Test WS",
}));

import { ConversationTraceModal } from "../ConversationTraceModal";

// Helper: renders the modal in open state with a spy for onClose
function renderOpen() {
  const onClose = vi.fn();
  render(
    <ConversationTraceModal
      open={true}
      workspaceId="ws-1"
      onClose={onClose}
    />
  );
  return { onClose };
}

// ────────────────────────────────────────────────────────────────────────────
// Presence / absence
// ────────────────────────────────────────────────────────────────────────────

describe("ConversationTraceModal — dialog presence (Issue M)", () => {
  it("dialog is absent when open=false", () => {
    render(
      <ConversationTraceModal open={false} workspaceId="ws-1" onClose={vi.fn()} />
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("dialog is present when open=true", () => {
    renderOpen();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });
});

// ────────────────────────────────────────────────────────────────────────────
// ARIA attributes provided by Radix Dialog
// ────────────────────────────────────────────────────────────────────────────

describe("ConversationTraceModal — ARIA attributes (Issue M)", () => {
  it("dialog element is accessible via role='dialog' with a non-empty accessible name", () => {
    renderOpen();
    // Radix Dialog.Content renders role="dialog" with aria-labelledby pointing
    // to Dialog.Title. Verify the role is present and the name is non-empty
    // (testing-library computes the accessible name from aria-labelledby).
    const dialog = screen.getByRole("dialog", { name: /conversation trace/i });
    expect(dialog).toBeTruthy();
  });

  it("dialog has aria-labelledby pointing to 'Conversation Trace' title", () => {
    renderOpen();
    const dialog = screen.getByRole("dialog");
    const labelledBy = dialog.getAttribute("aria-labelledby");
    expect(labelledBy).toBeTruthy();
    const titleEl = document.getElementById(labelledBy!);
    expect(titleEl?.textContent?.trim()).toBe("Conversation Trace");
  });

  it("dialog has data-state='open' (Radix state attribute)", () => {
    renderOpen();
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("data-state")).toBe("open");
  });
});

// ────────────────────────────────────────────────────────────────────────────
// Close button accessible name
// ────────────────────────────────────────────────────────────────────────────

describe("ConversationTraceModal — close button (Issue M)", () => {
  it("✕ close button has aria-label='Close conversation trace'", () => {
    renderOpen();
    const closeBtn = screen.getByRole("button", {
      name: /close conversation trace/i,
    });
    expect(closeBtn).toBeTruthy();
  });

  it("clicking ✕ button calls onClose", async () => {
    const { onClose } = renderOpen();
    const closeBtn = screen.getByRole("button", {
      name: /close conversation trace/i,
    });
    fireEvent.click(closeBtn);
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it("footer 'Close' button also closes the dialog", async () => {
    const { onClose } = renderOpen();
    const closeBtn = screen.getByRole("button", { name: /^Close$/i });
    fireEvent.click(closeBtn);
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });
});

// ────────────────────────────────────────────────────────────────────────────
// Escape key closes the dialog (WCAG 2.1.1 — Keyboard)
// ────────────────────────────────────────────────────────────────────────────

describe("ConversationTraceModal — Escape key (Issue M)", () => {
  it("Escape key triggers onClose via Radix onOpenChange", async () => {
    const { onClose } = renderOpen();
    // Radix Dialog automatically closes on Escape and fires onOpenChange(false)
    // which our handler converts to onClose(). Dispatch on the document so
    // Radix's own keydown listener picks it up.
    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });
});

// ────────────────────────────────────────────────────────────────────────────
// Empty state
// ────────────────────────────────────────────────────────────────────────────

describe("ConversationTraceModal — loading state (Issue M)", () => {
  it("shows loading indicator when dialog opens and fetch is in progress", () => {
    renderOpen();
    // After render + effects (flushed by act inside render), loading=true
    // because useEffect fired setLoading(true). The loading text should
    // be visible at this synchronous point.
    expect(screen.getByText(/loading trace from all workspaces/i)).toBeTruthy();
  });
});
