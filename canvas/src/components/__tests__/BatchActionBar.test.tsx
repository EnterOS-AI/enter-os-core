// @vitest-environment jsdom
/**
 * BatchActionBar tests — Phase 20.3
 *
 * Covers:
 *   - Not rendered when fewer than 2 nodes selected
 *   - Renders with correct count badge when 2+ selected
 *   - Restart/Pause/Delete buttons exist with correct labels
 *   - Clear selection button exists
 *   - ConfirmDialog appears on destructive action click
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// ── Mocks ────────────────────────────────────────────────────────────────────

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

const mockClearSelection = vi.fn();
const mockBatchRestart = vi.fn(() => Promise.resolve());
const mockBatchPause = vi.fn(() => Promise.resolve());
const mockBatchDelete = vi.fn(() => Promise.resolve());

let mockSelectedNodeIds = new Set<string>();

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      selectedNodeIds: mockSelectedNodeIds,
      clearSelection: mockClearSelection,
      batchRestart: mockBatchRestart,
      batchPause: mockBatchPause,
      batchDelete: mockBatchDelete,
    })
  ),
}));

// Mock ConfirmDialog — renders title + message + buttons so tests can assert
// on dialog copy (singular/plural, retry prompts, etc.). Keeping the message
// accessible in the DOM keeps copy-regression tests cheap. (QA recommendation
// from pr-batch-bar-retry-survivor review, memo qa-batch-bar-retry-survivor-
// approve-2026-04-19.)
vi.mock("@/components/ConfirmDialog", () => ({
  ConfirmDialog: ({
    open,
    title,
    message,
    onConfirm,
    onCancel,
  }: {
    open: boolean;
    title: string;
    confirmLabel: string;
    message: string;
    confirmVariant: string;
    onConfirm: () => void;
    onCancel: () => void;
  }) =>
    open ? (
      <div data-testid="confirm-dialog">
        <span data-testid="confirm-dialog-title">{title}</span>
        <p data-testid="confirm-dialog-message">{message}</p>
        <button onClick={onConfirm}>confirm</button>
        <button onClick={onCancel}>cancel</button>
      </div>
    ) : null,
}));

// Import after mocks
import { BatchActionBar } from "../BatchActionBar";

// ── Tests ────────────────────────────────────────────────────────────────────

describe("BatchActionBar", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSelectedNodeIds = new Set<string>();
  });

  it("does not render when fewer than 2 nodes selected", () => {
    mockSelectedNodeIds = new Set(["ws-1"]);
    const { container } = render(<BatchActionBar />);
    expect(container.innerHTML).toBe("");
  });

  it("renders count badge when 2+ nodes selected", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2", "ws-3"]);
    render(<BatchActionBar />);
    expect(screen.getByText("3 selected")).toBeTruthy();
  });

  it("renders Restart All, Pause All, Delete All buttons", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    expect(screen.getByText("Restart All")).toBeTruthy();
    expect(screen.getByText("Pause All")).toBeTruthy();
    expect(screen.getByText("Delete All")).toBeTruthy();
  });

  it("renders clear selection button with aria-label", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    const clearBtn = screen.getByRole("button", { name: "Clear selection" });
    expect(clearBtn).toBeTruthy();
  });

  it("clicking clear selection calls clearSelection", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(mockClearSelection).toHaveBeenCalled();
  });

  it("clicking Delete All opens ConfirmDialog", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    fireEvent.click(screen.getByText("Delete All"));
    expect(screen.getByTestId("confirm-dialog")).toBeTruthy();
  });

  it("has role=toolbar with aria-label", () => {
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    const toolbar = screen.getByRole("toolbar");
    expect(toolbar.getAttribute("aria-label")).toBe("Batch workspace actions");
  });

  it("Esc clears the selection — matches the deselect button title", () => {
    // The deselect button has been promising "Clear selection (Escape)"
    // since the bar shipped, but no handler was wired. This pins the
    // contract.
    mockSelectedNodeIds = new Set(["ws-1", "ws-2"]);
    render(<BatchActionBar />);
    fireEvent.keyDown(window, { key: "Escape" });
    expect(mockClearSelection).toHaveBeenCalled();
  });

  it("Esc is a no-op when nothing is selected", () => {
    mockSelectedNodeIds = new Set<string>();
    render(<BatchActionBar />);
    fireEvent.keyDown(window, { key: "Escape" });
    // The early-return at count===0 prevents the bar from mounting at all,
    // so the keydown listener never registers. clearSelection must NOT be
    // called.
    expect(mockClearSelection).not.toHaveBeenCalled();
  });
});

/**
 * Retry-survivorship regression tests (QA pr-949 follow-up).
 *
 * When batchRestart / batchPause / batchDelete partial-fail, the store
 * preserves the failed ids in selectedNodeIds and throws. BatchActionBar's
 * catch handler now sets hasFailedBatch=true so the toolbar stays mounted
 * even if only 1 survivor remains, letting the user click the same action
 * button again to retry without re-selecting.
 *
 * Prior behavior: `if (count < 2) return null` unmounted the bar when a
 * single survivor remained, forcing per-node context-menu retry. These
 * tests pin the new behavior.
 */
describe("BatchActionBar — partial-failure retry survivorship", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSelectedNodeIds = new Set<string>();
  });

  it("keeps bar mounted with '1 selected' when partial failure leaves one survivor", async () => {
    // User starts with 2 selected — bar renders.
    mockSelectedNodeIds = new Set(["ws-ok", "ws-fail"]);
    // Simulate store's partial-failure behavior: throws after the fulfilled-branch mutations.
    mockBatchDelete.mockImplementationOnce(() =>
      Promise.reject(new Error("1/2 delete(s) failed"))
    );

    const { rerender } = render(<BatchActionBar />);
    expect(screen.getByText("2 selected")).toBeTruthy();

    // Open confirm dialog → click confirm → execute() runs, rejects, catch sets hasFailedBatch.
    fireEvent.click(screen.getByText("Delete All"));
    fireEvent.click(screen.getByText("confirm"));
    // Let the microtask for the rejection and the subsequent setState run.
    await new Promise((r) => setTimeout(r, 0));

    // Store would have removed ws-ok and kept ws-fail — simulate the store's
    // `selectedNodeIds` mutation by swapping the mock and re-rendering.
    mockSelectedNodeIds = new Set(["ws-fail"]);
    rerender(<BatchActionBar />);

    // Bar MUST still render (hasFailedBatch=true from the catch), and the
    // count badge MUST show the survivor count so the user can retry.
    expect(screen.getByText("1 selected")).toBeTruthy();
    expect(screen.getByText("Delete All")).toBeTruthy();
  });

  it("confirm dialog uses singular 'workspace' copy when only one survivor remains", async () => {
    mockSelectedNodeIds = new Set(["ws-ok", "ws-fail"]);
    mockBatchDelete.mockImplementationOnce(() =>
      Promise.reject(new Error("1/2 delete(s) failed"))
    );
    const { rerender } = render(<BatchActionBar />);
    fireEvent.click(screen.getByText("Delete All"));
    fireEvent.click(screen.getByText("confirm"));
    await new Promise((r) => setTimeout(r, 0));

    // After failure: 1 survivor remains. Open the confirm dialog again for retry.
    mockSelectedNodeIds = new Set(["ws-fail"]);
    rerender(<BatchActionBar />);
    // Dialog is closed after the prior execute() — re-open via click.
    fireEvent.click(screen.getByText("Delete All"));

    // Count badge should show the survivor count.
    expect(screen.getByText("1 selected")).toBeTruthy();
    // Dialog copy MUST be singular — plural(1) → "workspace" (not "workspaces").
    // This is the primary user-facing signal that the retry is scoped to one item.
    const msg = screen.getByTestId("confirm-dialog-message");
    expect(msg.textContent).toBe(
      "Permanently delete 1 workspace? This cannot be undone."
    );
  });

  it("bar unmounts once a single-survivor selection is cleared (hasFailedBatch resets)", async () => {
    // Setup: simulate post-failure state with 1 survivor + hasFailedBatch=true.
    mockSelectedNodeIds = new Set(["ws-ok", "ws-fail"]);
    mockBatchDelete.mockImplementationOnce(() =>
      Promise.reject(new Error("1/2 delete(s) failed"))
    );
    const { rerender, container } = render(<BatchActionBar />);
    fireEvent.click(screen.getByText("Delete All"));
    fireEvent.click(screen.getByText("confirm"));
    await new Promise((r) => setTimeout(r, 0));

    mockSelectedNodeIds = new Set(["ws-fail"]);
    rerender(<BatchActionBar />);
    // Bar mounted with survivor visible.
    expect(screen.getByText("1 selected")).toBeTruthy();

    // User clears selection (Escape / ✕ button) — selection empties.
    mockSelectedNodeIds = new Set<string>();
    rerender(<BatchActionBar />);

    // Bar unmounts. The count===0 early return hides it; the useEffect then
    // resets hasFailedBatch so a future single-node selection won't re-show
    // the bar by mistake.
    expect(container.innerHTML).toBe("");
  });

  it("hasFailedBatch resets after a successful retry (success clears before clearSelection)", async () => {
    // Setup: partial-fail with 1 survivor → hasFailedBatch=true.
    mockSelectedNodeIds = new Set(["ws-ok", "ws-fail"]);
    mockBatchDelete.mockImplementationOnce(() =>
      Promise.reject(new Error("1/2 delete(s) failed"))
    );
    const { rerender, container } = render(<BatchActionBar />);
    fireEvent.click(screen.getByText("Delete All"));
    fireEvent.click(screen.getByText("confirm"));
    await new Promise((r) => setTimeout(r, 0));

    // Survivor remains → bar still mounted, hasFailedBatch=true.
    mockSelectedNodeIds = new Set(["ws-fail"]);
    rerender(<BatchActionBar />);
    expect(screen.getByText("1 selected")).toBeTruthy();

    // Successful retry: resolve without error → hasFailedBatch clears
    // before clearSelection() is called. The survivor is then removed
    // (deleted), leaving count=0.
    mockBatchDelete.mockImplementationOnce(() => Promise.resolve());
    fireEvent.click(screen.getByText("Delete All"));
    fireEvent.click(screen.getByText("confirm"));
    await new Promise((r) => setTimeout(r, 0));

    // After success + deletion: 0 remaining, hasFailedBatch=false.
    // Clearing selection must unmount the bar. If hasFailedBatch had NOT
    // been cleared, the bar would re-mount as a single-node toolbar
    // (because it would still be in the survivor state from the prior
    // catch block).
    mockSelectedNodeIds = new Set<string>();
    rerender(<BatchActionBar />);
    expect(container.innerHTML).toBe("");
  });
});
