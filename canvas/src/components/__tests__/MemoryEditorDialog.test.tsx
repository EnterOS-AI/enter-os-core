// @vitest-environment jsdom
/**
 * MemoryEditorDialog tests — covers Add (POST /memories) and Edit
 * (PATCH /memories/:id) flows. Pins:
 *   - Add posts {content, scope, namespace} with the trimmed defaults
 *   - Edit only sends fields that changed (no-op edit short-circuits, no PATCH fires)
 *   - Empty content blocks save
 *   - Save error surfaces in the dialog and keeps the modal open
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
    patch: vi.fn(),
    del: vi.fn(),
  },
}));

import { api } from "@/lib/api";
import { MemoryEditorDialog } from "../MemoryEditorDialog";
import type { MemoryEntry } from "../MemoryInspectorPanel";

const mockPost = vi.mocked(api.post);
const mockPatch = vi.mocked(api.patch);

const SAMPLE: MemoryEntry = {
  id: "mem-x",
  workspace_id: "ws-1",
  content: "original content",
  scope: "TEAM",
  namespace: "procedures",
  created_at: "2026-04-17T12:00:00.000Z",
};

beforeEach(() => {
  vi.clearAllMocks();
  mockPost.mockResolvedValue({} as never);
  mockPatch.mockResolvedValue({} as never);
});

afterEach(() => {
  cleanup();
});

describe("Add mode", () => {
  it("POSTs scope+namespace+trimmed-content and calls onSaved+onClose", async () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(
      <MemoryEditorDialog
        open
        mode="add"
        workspaceId="ws-1"
        defaultScope="GLOBAL"
        defaultNamespace="facts"
        onClose={onClose}
        onSaved={onSaved}
      />,
    );

    const textarea = screen.getByLabelText(/Content/i) as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: "  new fact  " } });

    fireEvent.click(screen.getByRole("button", { name: /Add memory$/i }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-1/memories", {
      content: "new fact",
      scope: "GLOBAL",
      namespace: "facts",
    });
    expect(onSaved).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("blocks save when content is empty (whitespace-only)", () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(
      <MemoryEditorDialog
        open
        mode="add"
        workspaceId="ws-1"
        defaultScope="LOCAL"
        onClose={onClose}
        onSaved={onSaved}
      />,
    );
    const textarea = screen.getByLabelText(/Content/i) as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: "   " } });
    fireEvent.click(screen.getByRole("button", { name: /Add memory$/i }));
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toMatch(/empty/i);
    expect(onSaved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe("Edit mode", () => {
  it("PATCHes only changed fields", async () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(
      <MemoryEditorDialog
        open
        mode="edit"
        workspaceId="ws-1"
        entry={SAMPLE}
        onClose={onClose}
        onSaved={onSaved}
      />,
    );

    const textarea = screen.getByLabelText(/Content/i) as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: "rewritten content" } });
    // namespace untouched

    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));

    await waitFor(() => expect(mockPatch).toHaveBeenCalledTimes(1));
    expect(mockPatch).toHaveBeenCalledWith(
      "/workspaces/ws-1/memories/mem-x",
      { content: "rewritten content" },
    );
    expect(onSaved).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("no-op edit short-circuits (no PATCH fires) and still closes", async () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(
      <MemoryEditorDialog
        open
        mode="edit"
        workspaceId="ws-1"
        entry={SAMPLE}
        onClose={onClose}
        onSaved={onSaved}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(mockPatch).not.toHaveBeenCalled();
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("sends namespace too when both content and namespace changed", async () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(
      <MemoryEditorDialog
        open
        mode="edit"
        workspaceId="ws-1"
        entry={SAMPLE}
        onClose={onClose}
        onSaved={onSaved}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Content/i), {
      target: { value: "newer content" },
    });
    fireEvent.change(screen.getByLabelText(/Namespace/i), {
      target: { value: "blockers" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() => expect(mockPatch).toHaveBeenCalledTimes(1));
    expect(mockPatch).toHaveBeenCalledWith(
      "/workspaces/ws-1/memories/mem-x",
      { content: "newer content", namespace: "blockers" },
    );
  });

  it("surfaces save error and keeps the modal open", async () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    mockPatch.mockRejectedValueOnce(new Error("boom"));
    render(
      <MemoryEditorDialog
        open
        mode="edit"
        workspaceId="ws-1"
        entry={SAMPLE}
        onClose={onClose}
        onSaved={onSaved}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Content/i), {
      target: { value: "rewritten content" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/boom/),
    );
    expect(onClose).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
  });
});
