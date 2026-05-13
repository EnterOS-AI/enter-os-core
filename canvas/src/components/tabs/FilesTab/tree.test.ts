// @vitest-environment node
/**
 * FilesTab tree utilities — pure function coverage.
 *
 * Covers:
 *   - getIcon: case-insensitive extension lookup, directory icons, unknown extensions
 *   - buildTree: flat list → nested tree, dirs-first sorting, duplicate dir guard,
 *     nested paths, single-level files
 */
import { describe, expect, it } from "vitest";

import { buildTree, getIcon, type FileEntry } from "./tree";

// ─── getIcon ────────────────────────────────────────────────────────────────────

describe("getIcon — directory", () => {
  it("returns folder icon for directories", () => {
    expect(getIcon("src", true)).toBe("📁");
    expect(getIcon("src/components", true)).toBe("📁");
  });
});

describe("getIcon — extension mapping", () => {
  const cases: [string, string][] = [
    // Known extensions
    ["script.py", "🐍"],
    ["script.PY", "🐍"],           // case-insensitive
    ["script.Py", "🐍"],
    ["main.ts", "💠"],
    ["main.TS", "💠"],
    ["component.tsx", "💠"],
    ["style.css", "🎨"],
    ["index.html", "🌐"],
    ["data.json", "{}"],
    ["app.js", "📜"],
    ["config.yaml", "⚙"],
    ["config.yml", "⚙"],
    ["README.md", "📄"],
    ["build.sh", "▸"],
    // Unknown extension → default
    ["photo.png", "📄"],
    ["archive.zip", "📄"],
    ["document.pdf", "📄"],
    ["data.xml", "📄"],
  ];

  it.each(cases)("getIcon('%s', false) === '%s'", (path, expected) => {
    expect(getIcon(path, false)).toBe(expected);
  });
});

describe("getIcon — edge cases", () => {
  it("no extension (dotfile) falls back to default", () => {
    expect(getIcon(".gitignore", false)).toBe("📄");
    expect(getIcon(".env.local", false)).toBe("📄");
  });

  it("single-component path with no extension falls back to default", () => {
    expect(getIcon("Makefile", false)).toBe("📄");
  });

  it("double extension takes last segment as extension", () => {
    // "file.min.js" → ext = ".js" → 📜 (JS icon)
    expect(getIcon("file.min.js", false)).toBe("📜");
    // "app.d.ts" → ext = ".ts" → 💠 (TS icon)
    expect(getIcon("app.d.ts", false)).toBe("💠");
  });
});

// ─── buildTree ──────────────────────────────────────────────────────────────────

describe("buildTree — empty input", () => {
  it("returns empty array for empty input", () => {
    expect(buildTree([])).toEqual([]);
  });
});

describe("buildTree — flat files", () => {
  it("puts files at root level", () => {
    const files: FileEntry[] = [
      { path: "a.txt", size: 10, dir: false },
      { path: "b.txt", size: 20, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(2);
    expect(tree[0]!.name).toBe("a.txt");
    expect(tree[0]!.path).toBe("a.txt");
    expect(tree[0]!.isDir).toBe(false);
    expect(tree[0]!.size).toBe(10);
  });

  it("directories appear before files (dirs-first)", () => {
    const files: FileEntry[] = [
      { path: "b.txt", size: 10, dir: false },
      { path: "src", size: 0, dir: true },
      { path: "a.txt", size: 10, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree[0]!.isDir).toBe(true);
    expect(tree[0]!.name).toBe("src");
    expect(tree[1]!.name).toBe("a.txt");
    expect(tree[2]!.name).toBe("b.txt");
  });
});

describe("buildTree — nested paths", () => {
  it("builds correct nested structure", () => {
    const files: FileEntry[] = [
      { path: "src", size: 0, dir: true },
      { path: "src/app.tsx", size: 100, dir: false },
      { path: "src/app.css", size: 50, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0]!.name).toBe("src");
    expect(tree[0]!.isDir).toBe(true);
    expect(tree[0]!.children).toHaveLength(2);
    expect(tree[0]!.children[0]!.name).toBe("app.css");
    expect(tree[0]!.children[1]!.name).toBe("app.tsx");
  });

  it("deeply nested paths build correct depth", () => {
    const files: FileEntry[] = [
      { path: "a", size: 0, dir: true },
      { path: "a/b", size: 0, dir: true },
      { path: "a/b/c.txt", size: 30, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree[0]!.name).toBe("a");
    expect(tree[0]!.children[0]!.name).toBe("b");
    expect(tree[0]!.children[0]!.children[0]!.name).toBe("c.txt");
  });
});

describe("buildTree — duplicate dir guard", () => {
  it("ignores duplicate directory entries", () => {
    const files: FileEntry[] = [
      { path: "src", size: 0, dir: true },
      { path: "src", size: 0, dir: true },   // duplicate
      { path: "src/app.ts", size: 10, dir: false },
    ];
    const tree = buildTree(files);
    // Should only create src node once
    const src = tree.find((n) => n.name === "src");
    expect(src).toBeDefined();
    expect(src!.children).toHaveLength(1);
  });
});

describe("buildTree — alphabetical sort within same level", () => {
  it("sorts alphabetically at each level", () => {
    const files: FileEntry[] = [
      { path: "zebra.txt", size: 1, dir: false },
      { path: "apple.txt", size: 1, dir: false },
      { path: "banana.txt", size: 1, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree.map((n) => n.name)).toEqual(["apple.txt", "banana.txt", "zebra.txt"]);
  });
});
