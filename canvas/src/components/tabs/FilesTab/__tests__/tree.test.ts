// @vitest-environment jsdom
/**
 * Tests for tree.ts — buildTree and getIcon pure functions.
 */
import { describe, expect, it } from "vitest";
import type { FileEntry } from "../tree";
import { buildTree, getIcon } from "../tree";

// ─── getIcon ─────────────────────────────────────────────────────────────────

describe("getIcon", () => {
  it("returns folder emoji for directories", () => {
    expect(getIcon("/configs", true)).toBe("📁");
  });

  it("returns correct emoji for .md", () => {
    expect(getIcon("readme.md", false)).toBe("📄");
  });

  it("returns correct emoji for .yaml", () => {
    expect(getIcon("config.yaml", false)).toBe("⚙");
  });

  it("returns correct emoji for .yml", () => {
    expect(getIcon("config.yml", false)).toBe("⚙");
  });

  it("returns correct emoji for .py", () => {
    expect(getIcon("script.py", false)).toBe("🐍");
  });

  it("returns correct emoji for .ts", () => {
    expect(getIcon("index.ts", false)).toBe("💠");
  });

  it("returns correct emoji for .tsx", () => {
    expect(getIcon("App.tsx", false)).toBe("💠");
  });

  it("returns correct emoji for .js", () => {
    expect(getIcon("index.js", false)).toBe("📜");
  });

  it("returns correct emoji for .json", () => {
    expect(getIcon("package.json", false)).toBe("{}");
  });

  it("returns correct emoji for .html", () => {
    expect(getIcon("index.html", false)).toBe("🌐");
  });

  it("returns correct emoji for .css", () => {
    expect(getIcon("style.css", false)).toBe("🎨");
  });

  it("returns correct emoji for .sh", () => {
    expect(getIcon("deploy.sh", false)).toBe("▸");
  });

  it("returns default file emoji for unknown extensions", () => {
    expect(getIcon("Makefile", false)).toBe("📄");
    expect(getIcon("Dockerfile", false)).toBe("📄");
    expect(getIcon("Rakefile", false)).toBe("📄");
  });

  it("extension matching is case-insensitive", () => {
    expect(getIcon("readme.MD", false)).toBe("📄");
    expect(getIcon("script.PY", false)).toBe("🐍");
  });
});

// ─── buildTree ───────────────────────────────────────────────────────────────

describe("buildTree", () => {
  it("returns empty array for empty input", () => {
    expect(buildTree([])).toEqual([]);
  });

  it("adds a single file at root", () => {
    const files: FileEntry[] = [{ path: "config.yaml", size: 128, dir: false }];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0]).toMatchObject({
      name: "config.yaml",
      path: "config.yaml",
      isDir: false,
      children: [],
      size: 128,
    });
  });

  it("adds a single directory at root", () => {
    const files: FileEntry[] = [{ path: "skills", size: 0, dir: true }];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0]).toMatchObject({
      name: "skills",
      path: "skills",
      isDir: true,
      children: [],
      size: 0,
    });
  });

  it("sorts dirs before files at the same level", () => {
    const files: FileEntry[] = [
      { path: "b.txt", size: 10, dir: false },
      { path: "a.txt", size: 10, dir: false },
      { path: "z-dir", size: 0, dir: true },
      { path: "a-dir", size: 0, dir: true },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(4);
    // Dirs first: z-dir, a-dir alphabetically → a before z
    expect(tree[0].name).toBe("a-dir");
    expect(tree[1].name).toBe("z-dir");
    // Then files alphabetically
    expect(tree[2].name).toBe("a.txt");
    expect(tree[3].name).toBe("b.txt");
  });

  it("alphabetically sorts files within the same level", () => {
    const files: FileEntry[] = [
      { path: "z.yaml", size: 10, dir: false },
      { path: "a.yaml", size: 10, dir: false },
      { path: "m.yaml", size: 10, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree.map((n) => n.name)).toEqual(["a.yaml", "m.yaml", "z.yaml"]);
  });

  it("nests a file under its parent directory", () => {
    const files: FileEntry[] = [
      { path: "skills", size: 0, dir: true },
      { path: "skills/readme.md", size: 64, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0].name).toBe("skills");
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0]).toMatchObject({
      name: "readme.md",
      path: "skills/readme.md",
      isDir: false,
      size: 64,
    });
  });

  it("creates intermediate directories automatically", () => {
    const files: FileEntry[] = [
      { path: "a/b/c/deep.txt", size: 32, dir: false },
    ];
    const tree = buildTree(files);
    // Root has one child: "a"
    expect(tree).toHaveLength(1);
    expect(tree[0].name).toBe("a");
    expect(tree[0].isDir).toBe(true);
    // "a" has one child: "b"
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].name).toBe("b");
    // "b" has one child: "c"
    expect(tree[0].children[0].children).toHaveLength(1);
    expect(tree[0].children[0].children[0].name).toBe("c");
    // "c" has the file
    expect(tree[0].children[0].children[0].children[0].name).toBe("deep.txt");
    expect(tree[0].children[0].children[0].children[0].size).toBe(32);
  });

  it("adds multiple files to the same directory", () => {
    const files: FileEntry[] = [
      { path: "configs", size: 0, dir: true },
      { path: "configs/a.yaml", size: 10, dir: false },
      { path: "configs/b.yaml", size: 20, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0].children.map((n) => n.name).sort()).toEqual(["a.yaml", "b.yaml"]);
  });

  it("does not duplicate a directory already created as intermediate", () => {
    const files: FileEntry[] = [
      { path: "a/b.txt", size: 5, dir: false },
      { path: "a", size: 0, dir: true },
    ];
    const tree = buildTree(files);
    // "a" should appear only once
    expect(tree).toHaveLength(1);
    expect(tree[0].name).toBe("a");
    // The dir "a" should still contain "b.txt"
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].name).toBe("b.txt");
  });

  it("intermediate dirs have size 0", () => {
    const files: FileEntry[] = [
      { path: "a/b/c/file.txt", size: 1, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree[0].size).toBe(0);
    expect(tree[0].children[0].size).toBe(0);
  });

  it("handles deeply nested mixed dirs and files", () => {
    const files: FileEntry[] = [
      { path: "a", size: 0, dir: true },
      { path: "a/b", size: 0, dir: true },
      { path: "a/b/c", size: 0, dir: true },
      { path: "a/b/c/d.txt", size: 1, dir: false },
      { path: "a/b/e.txt", size: 2, dir: false },
      { path: "a/f.txt", size: 3, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1); // root: "a"
    expect(tree[0].children.map((n) => n.name).sort()).toEqual(["b", "f.txt"]);
    expect(tree[0].children.find((n) => n.name === "b")!.children.map((n) => n.name).sort())
      .toEqual(["c", "e.txt"]);
  });
});
