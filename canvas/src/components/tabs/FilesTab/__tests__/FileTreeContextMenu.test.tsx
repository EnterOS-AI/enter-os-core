// @vitest-environment jsdom
//
// Pins the right-click context menu added in PR-C of issue #2999.
// VSCode-style affordance: Open / Download / Delete on file rows,
// Delete on directory rows. Delete is gated by `canDelete` (parent
// only enables on /configs root, matching the toolbar's gate).
//
// Pinned branches:
//   1. Right-click on a file row opens the menu at the click coords
//      with Open + Download + Delete items.
//   2. Right-click on a directory row opens the menu with Delete
//      only (no Open/Download — directories don't have one-click
//      semantics in this surface).
//   3. Clicking Download fires the onDownload callback with the
//      row's path.
//   4. Clicking Delete fires onDelete with the row's path (when
//      canDelete=true).
//   5. Delete is disabled in the rendered menu when canDelete=false
//      and clicking it does NOT fire onDelete (gate is real).
//   6. Esc dismisses the menu.
//   7. Click outside the menu dismisses it.

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, act } from "@testing-library/react";
import React from "react";
import { FileTree } from "../FileTree";
import type { TreeNode } from "../tree";

afterEach(cleanup);

const file: TreeNode = { name: "config.yaml", path: "config.yaml", isDir: false, children: [] };
const dir: TreeNode = {
  name: "skills",
  path: "skills",
  isDir: true,
  children: [],
};

function renderTree(props: Partial<React.ComponentProps<typeof FileTree>> = {}) {
  const defaults = {
    nodes: [file, dir],
    selectedPath: null,
    onSelect: vi.fn(),
    onDelete: vi.fn(),
    onDownload: vi.fn(),
    canDelete: true,
    expandedDirs: new Set<string>(),
    onToggleDir: vi.fn(),
    loadingDir: null,
  };
  const merged = { ...defaults, ...props };
  return { ...render(<FileTree {...merged} />), props: merged };
}

describe("FileTree right-click context menu", () => {
  it("right-click on a file row opens menu with Open/Download/Delete", () => {
    renderTree();
    fireEvent.contextMenu(screen.getByText("config.yaml"), {
      clientX: 50,
      clientY: 100,
    });
    expect(screen.getByRole("menu")).not.toBeNull();
    expect(screen.getByRole("menuitem", { name: /Open/i })).not.toBeNull();
    expect(screen.getByRole("menuitem", { name: /Download/i })).not.toBeNull();
    expect(screen.getByRole("menuitem", { name: /Delete/i })).not.toBeNull();
  });

  it("right-click on a directory row opens menu with Delete only (no Open/Download)", () => {
    renderTree();
    fireEvent.contextMenu(screen.getByText("skills"), { clientX: 60, clientY: 120 });
    expect(screen.getByRole("menu")).not.toBeNull();
    expect(screen.queryByRole("menuitem", { name: /Open/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /Download/i })).toBeNull();
    expect(screen.getByRole("menuitem", { name: /Delete/i })).not.toBeNull();
  });

  it("clicking Download fires onDownload with the row's path", () => {
    const { props } = renderTree();
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 0, clientY: 0 });
    fireEvent.click(screen.getByRole("menuitem", { name: /Download/i }));
    expect(props.onDownload).toHaveBeenCalledWith("config.yaml");
    // Menu auto-closes after click.
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("clicking Delete fires onDelete with the row's path when canDelete=true", () => {
    const { props } = renderTree({ canDelete: true });
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 0, clientY: 0 });
    fireEvent.click(screen.getByRole("menuitem", { name: /Delete/i }));
    expect(props.onDelete).toHaveBeenCalledWith("config.yaml");
  });

  it("Delete is disabled when canDelete=false; clicking does not fire onDelete", () => {
    const { props } = renderTree({ canDelete: false });
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 0, clientY: 0 });
    const del = screen.getByRole("menuitem", { name: /Delete/i }) as HTMLButtonElement;
    expect(del.disabled).toBe(true);
    fireEvent.click(del);
    expect(props.onDelete).not.toHaveBeenCalled();
    // Menu stays open on disabled click — same as VSCode (the user
    // can read the disabled-state hint without losing the menu).
    expect(screen.getByRole("menu")).not.toBeNull();
  });

  it("Esc dismisses the menu", () => {
    renderTree();
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 0, clientY: 0 });
    expect(screen.getByRole("menu")).not.toBeNull();
    act(() => {
      fireEvent.keyDown(document, { key: "Escape" });
    });
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("click outside the menu dismisses it", () => {
    renderTree();
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 0, clientY: 0 });
    expect(screen.getByRole("menu")).not.toBeNull();
    // mousedown on document.body — outside the menu.
    act(() => {
      fireEvent.mouseDown(document.body);
    });
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("opening a second context menu replaces the first (only one open at a time)", () => {
    renderTree();
    fireEvent.contextMenu(screen.getByText("config.yaml"), { clientX: 10, clientY: 10 });
    fireEvent.contextMenu(screen.getByText("skills"), { clientX: 20, clientY: 20 });
    // Only one menu in the DOM. The second open replaced the first
    // because the menu state is lifted to the FileTree, not per-row.
    const menus = screen.getAllByRole("menu");
    expect(menus.length).toBe(1);
  });
});
