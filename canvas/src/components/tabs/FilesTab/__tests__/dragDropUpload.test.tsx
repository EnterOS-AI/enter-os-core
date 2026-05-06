// @vitest-environment jsdom
//
// Pins the drag-drop upload added in PR-D of issue #2999.
// Two layers of coverage:
//
//  1. The pure walker (collectFileEntries / walkEntry) — pins the
//     recursion shape against silent folder truncation. Browsers
//     return up to ~100 entries per readEntries() call; if the loop
//     stops early, large folder uploads silently drop files. We
//     simulate a multi-batch reader to discriminate.
//
//  2. FileTree directory-row drop handlers — pins that dragover/drop
//     events fire onDropToTarget with the directory's path + the
//     drop's DataTransferItemList.

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import React from "react";
import { FileTree } from "../FileTree";
import type { TreeNode } from "../tree";
import { __testables } from "../useFilesApi";

afterEach(cleanup);

// ---- Walker tests ----

/**
 * Build a fake FileSystemEntry tree we can hand to walkEntry. The
 * shape mimics what webkitGetAsEntry returns from a real OS drag —
 * directory entries expose createReader, file entries expose file().
 */
function fakeFileEntry(name: string, content = "x"): {
  isFile: true;
  isDirectory: false;
  name: string;
  fullPath: string;
  file: (cb: (f: File) => void) => void;
} {
  return {
    isFile: true,
    isDirectory: false,
    name,
    fullPath: "/" + name,
    file: (cb) => cb(new File([content], name, { type: "text/plain" })),
  };
}

function fakeDirEntry(
  name: string,
  childBatches: ReturnType<typeof fakeFileEntry>[][],
): {
  isFile: false;
  isDirectory: true;
  name: string;
  fullPath: string;
  createReader: () => { readEntries: (cb: (entries: unknown[]) => void) => void };
} {
  let i = 0;
  return {
    isFile: false,
    isDirectory: true,
    name,
    fullPath: "/" + name,
    createReader: () => ({
      readEntries: (cb) => {
        // Mimic browser semantics: emit one batch per call, then
        // an empty array to signal end-of-stream. A walker that
        // calls readEntries only once would silently truncate at
        // the first batch.
        if (i < childBatches.length) {
          cb(childBatches[i++]);
        } else {
          cb([]);
        }
      },
    }),
  };
}

describe("walkEntry — folder-recursion drop walker", () => {
  it("collects a single dropped file", async () => {
    const out: { file: File; relativePath: string }[] = [];
    await __testables.walkEntry(fakeFileEntry("README.md") as never, "", out);
    expect(out.length).toBe(1);
    expect(out[0].relativePath).toBe("README.md");
    expect(out[0].file.name).toBe("README.md");
  });

  it("walks a folder and preserves the relative path under the folder name", async () => {
    const out: { file: File; relativePath: string }[] = [];
    const folder = fakeDirEntry("skills", [
      [fakeFileEntry("a.md"), fakeFileEntry("b.md")],
    ]);
    await __testables.walkEntry(folder as never, "", out);
    expect(out.map((e) => e.relativePath).sort()).toEqual([
      "skills/a.md",
      "skills/b.md",
    ]);
  });

  it("loops readEntries until empty so a multi-batch folder isn't truncated", async () => {
    // Browsers limit each readEntries() call to ~100 entries. Our
    // walker MUST call it again until an empty batch is returned.
    // Fake reader emits two batches of 2 + an implicit empty → 4
    // total. A buggy walker that only takes the first batch would
    // see only 2.
    const out: { file: File; relativePath: string }[] = [];
    const folder = fakeDirEntry("big", [
      [fakeFileEntry("1.txt"), fakeFileEntry("2.txt")],
      [fakeFileEntry("3.txt"), fakeFileEntry("4.txt")],
    ]);
    await __testables.walkEntry(folder as never, "", out);
    expect(out.length).toBe(4);
  });

  it("walks nested directories and accumulates the full path", async () => {
    const out: { file: File; relativePath: string }[] = [];
    const inner = fakeDirEntry("web-search", [[fakeFileEntry("SKILL.md")]]);
    // Outer dir whose first batch contains a sub-dir entry.
    const outer = {
      isFile: false,
      isDirectory: true,
      name: "skills",
      fullPath: "/skills",
      createReader: () => {
        let i = 0;
        return {
          readEntries: (cb: (entries: unknown[]) => void) => {
            if (i++ === 0) cb([inner]);
            else cb([]);
          },
        };
      },
    };
    await __testables.walkEntry(outer as never, "", out);
    expect(out.length).toBe(1);
    expect(out[0].relativePath).toBe("skills/web-search/SKILL.md");
  });
});

// ---- FileTree drag-drop wiring ----

const file: TreeNode = { name: "config.yaml", path: "config.yaml", isDir: false, children: [], size: 0 };
const skillsDir: TreeNode = { name: "skills", path: "skills", isDir: true, children: [], size: 0 };

function renderTree(props: Partial<React.ComponentProps<typeof FileTree>> = {}) {
  // PR-D test defaults must include PR-C's onDownload + canDelete now
  // that they're required on the TreeCallbacks shape (the rebase
  // surfaced this — the merged tree depends on both feature sets).
  const defaults: React.ComponentProps<typeof FileTree> = {
    nodes: [file, skillsDir],
    selectedPath: null,
    onSelect: vi.fn(),
    onDelete: vi.fn(),
    onDownload: vi.fn(),
    canDelete: true,
    onDropToTarget: vi.fn(),
    expandedDirs: new Set<string>(),
    onToggleDir: vi.fn(),
    loadingDir: null,
  };
  const merged = { ...defaults, ...props };
  return { ...render(<FileTree {...merged} />), props: merged };
}

describe("FileTree directory-row drag-drop", () => {
  it("dragover on a directory row preventDefault's so the drop will fire", () => {
    renderTree();
    const row = screen.getByText("skills");
    const dragOver = new Event("dragover", { bubbles: true, cancelable: true });
    Object.defineProperty(dragOver, "dataTransfer", {
      value: { dropEffect: "" },
    });
    row.parentElement!.dispatchEvent(dragOver);
    // preventDefault registers via the React handler — without it
    // the drop event would never fire, so this assertion is the
    // load-bearing one.
    expect(dragOver.defaultPrevented).toBe(true);
  });

  it("drop on a directory row fires onDropToTarget with that path + the items list", () => {
    const { props } = renderTree();
    const row = screen.getByText("skills").parentElement!;
    const fakeItems = { length: 1, 0: { kind: "file" } } as unknown as DataTransferItemList;
    fireEvent.drop(row, { dataTransfer: { items: fakeItems } });
    expect(props.onDropToTarget).toHaveBeenCalledWith("skills", fakeItems);
  });

  it("drop on a FILE row does NOT fire onDropToTarget (only directories are valid targets)", () => {
    const { props } = renderTree();
    const fileRow = screen.getByText("config.yaml").parentElement!;
    const fakeItems = { length: 1, 0: { kind: "file" } } as unknown as DataTransferItemList;
    fireEvent.drop(fileRow, { dataTransfer: { items: fakeItems } });
    expect(props.onDropToTarget).not.toHaveBeenCalled();
  });

  it("drop with no DataTransferItems does NOT fire onDropToTarget", () => {
    const { props } = renderTree();
    const row = screen.getByText("skills").parentElement!;
    fireEvent.drop(row, { dataTransfer: { items: { length: 0 } } });
    expect(props.onDropToTarget).not.toHaveBeenCalled();
  });

  it("dragenter sets the drop-target highlight on the directory row", () => {
    renderTree();
    const row = screen.getByText("skills").parentElement!;
    fireEvent.dragEnter(row, { dataTransfer: {} });
    // Highlight class is the discriminator — without dragenter
    // wiring the row stays in its hover-only style.
    expect(row.className).toMatch(/bg-accent|outline-accent/);
  });
});
