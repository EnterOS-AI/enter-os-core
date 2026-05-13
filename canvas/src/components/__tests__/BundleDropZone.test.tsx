// @vitest-environment jsdom
/**
 * Tests for BundleDropZone component.
 *
 * Covers: drag-over/drag-leave state, drop of valid/invalid files,
 * keyboard file input, import success, import error, auto-clear timeout.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { BundleDropZone } from "../BundleDropZone";
import { api } from "@/lib/api";

vi.mock("@/lib/api", () => ({
  api: {
    post: vi.fn(),
  },
}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

// ─── Test helper ──────────────────────────────────────────────────────────────

function makeBundle(name = "test-workspace"): File {
  const content = JSON.stringify({
    name,
    tier: 2,
    skills: [],
    config: {},
  });
  return new File([content], "test.bundle.json", {
    type: "application/json",
  });
}

// jsdom doesn't define DragEvent globally; create a dragover event with
// dataTransfer.types stubbed to include "Files" so handleDragOver triggers.
function createDragOverEvent() {
  return Object.assign(new Event("dragover", { bubbles: true, cancelable: true }), {
    dataTransfer: { types: ["Files"], files: null },
  });
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("BundleDropZone — render", () => {
  it("renders a hidden file input with correct accept and aria-label", () => {
    render(<BundleDropZone />);
    // Use id selector since both input and button share aria-label="Import bundle file"
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;
    expect(input).toBeTruthy();
    expect(input.getAttribute("type")).toBe("file");
    expect(input.getAttribute("accept")).toBe(".bundle.json");
  });

  it("renders the keyboard-accessible import button with aria-label", () => {
    render(<BundleDropZone />);
    const btn = screen.getByRole("button", { name: /import bundle/i });
    expect(btn).toBeTruthy();
    expect(btn.getAttribute("aria-controls")).toBe("bundle-file-input");
  });
});

describe("BundleDropZone — drag state", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows the drop overlay when a file is dragged over", async () => {
    render(<BundleDropZone />);
    expect(screen.queryByText("Drop Bundle to Import")).toBeNull();
    const zone = document.body.querySelector('[class*="z-10"]') as HTMLElement;
    if (zone) {
      const dragOverEvent = createDragOverEvent();
      fireEvent.dragOver(zone, dragOverEvent);
    }
    await act(async () => { vi.runOnlyPendingTimers(); });
    const overlay = screen.getByText("Drop Bundle to Import").closest('[class*="z-20"]');
    expect(overlay).not.toBeNull();
  });

  it("hides the drop overlay when not dragging", () => {
    render(<BundleDropZone />);
    // By default (no drag), the overlay should not be visible
    expect(screen.queryByText("Drop Bundle to Import")).toBeNull();
  });
});

describe("BundleDropZone — keyboard file input (WCAG 2.1.1)", () => {
  it("triggers the hidden file input when the import button is clicked", () => {
    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;    const clickSpy = vi.spyOn(input, "click");
    fireEvent.click(screen.getByRole("button", { name: /import bundle/i }));
    expect(clickSpy).toHaveBeenCalled();
  });

  it("processes a selected file when the file input changes", async () => {
    vi.useFakeTimers();
    const postMock = vi.mocked(api.post).mockResolvedValueOnce({
      workspace_id: "ws-new",
      name: "Imported Workspace",
      status: "online",
    });

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("My Bundle");
    Object.defineProperty(input, "files", {
      value: [file],
      writable: false,
    });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });

    expect(postMock).toHaveBeenCalledWith(
      "/bundles/import",
      expect.objectContaining({ name: "My Bundle" })
    );
    vi.useRealTimers();
  });
});

describe("BundleDropZone — import success", () => {
  it("shows success toast after successful import", async () => {
    vi.useFakeTimers();
    vi.mocked(api.post).mockResolvedValueOnce({
      workspace_id: "ws-new",
      name: "My Workspace",
      status: "online",
    });

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("Success Workspace");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });

    // Success toast should be visible
    expect(screen.getByText(/imported "my workspace" successfully/i)).toBeTruthy();

    // Toast auto-clears after 4000ms
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.queryByRole("status")).toBeNull();
    vi.useRealTimers();
  });

  it("clears the result toast after 4000ms", async () => {
    vi.useFakeTimers();
    vi.mocked(api.post).mockResolvedValueOnce({
      workspace_id: "ws-new",
      name: "Timed Workspace",
      status: "online",
    });

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("Timed Workspace");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByText(/timed workspace/i)).toBeTruthy();

    await act(async () => {
      vi.advanceTimersByTime(4500);
    });
    expect(screen.queryByText(/timed workspace/i)).toBeNull();
    vi.useRealTimers();
  });
});

describe("BundleDropZone — import error", () => {
  it("shows error toast when the API call fails", async () => {
    vi.useFakeTimers();
    vi.mocked(api.post).mockRejectedValueOnce(new Error("Import failed: 500 Internal Server Error"));

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("Failed Workspace");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });

    expect(screen.getByText(/import failed: 500 internal server error/i)).toBeTruthy();
    vi.useRealTimers();
  });

  it("shows error when file is not a .bundle.json", async () => {
    vi.useFakeTimers();
    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = new File(["{}"], "readme.txt", { type: "text/plain" });
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });

    expect(screen.getByText(/only .bundle.json files are accepted/i)).toBeTruthy();
    // Error clears after 3000ms
    await act(async () => {
      vi.advanceTimersByTime(3500);
    });
    expect(screen.queryByText(/only .bundle.json/i)).toBeNull();
    vi.useRealTimers();
  });

  it("clears error after 4000ms", async () => {
    vi.useFakeTimers();
    vi.mocked(api.post).mockRejectedValueOnce(new Error("Network error"));

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("Error Workspace");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByText(/network error/i)).toBeTruthy();

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.queryByText(/network error/i)).toBeNull();
    vi.useRealTimers();
  });
});

describe("BundleDropZone — importing state", () => {
  it("shows 'Importing bundle...' status while API call is in flight", async () => {
    vi.useFakeTimers();
    let resolve: (v: unknown) => void;
    const pending = new Promise((r) => { resolve = r; });
    vi.mocked(api.post).mockReturnValueOnce(pending as unknown as ReturnType<typeof api.post>);

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = makeBundle("Pending Workspace");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    // Advance timer to allow the state update to flush
    await act(async () => {
      vi.advanceTimersByTime(100);
    });

    expect(screen.getByText("Importing bundle...")).toBeTruthy();
    expect(screen.getByRole("status")).toBeTruthy();

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    vi.useRealTimers();
  });
});

describe("BundleDropZone — file input reset", () => {
  it("resets the file input value after processing so the same file can be re-selected", async () => {
    vi.useFakeTimers();
    vi.mocked(api.post).mockResolvedValueOnce({
      workspace_id: "ws-new",
      name: "Reset Workspace",
      status: "online",
    });

    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;
    const file = makeBundle("Reset Test");
    Object.defineProperty(input, "files", { value: [file], writable: false });

    fireEvent.change(input);

    await act(async () => {
      vi.advanceTimersByTime(500);
    });

    // The component calls e.target.value = "" after processing
    expect(input.value).toBe("");
    vi.useRealTimers();
  });
});
