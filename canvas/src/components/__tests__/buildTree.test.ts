import { describe, it, expect } from "vitest";
import { buildTree } from "../tabs/FilesTab";

describe("buildTree", () => {
  it("returns empty array for empty input", () => {
    expect(buildTree([])).toEqual([]);
  });

  it("handles flat files at root level", () => {
    const files = [
      { path: "config.yaml", size: 100, dir: false },
      { path: "readme.md", size: 50, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(2);
    expect(tree[0].name).toBe("config.yaml");
    expect(tree[1].name).toBe("readme.md");
    expect(tree.every((n) => !n.isDir)).toBe(true);
  });

  it("sorts dirs before files", () => {
    const files = [
      { path: "file.txt", size: 10, dir: false },
      { path: "scripts", size: 0, dir: true },
    ];
    const tree = buildTree(files);
    expect(tree[0].name).toBe("scripts");
    expect(tree[0].isDir).toBe(true);
    expect(tree[1].name).toBe("file.txt");
  });

  it("nests files under parent directories", () => {
    const files = [
      { path: ".claude", size: 0, dir: true },
      { path: ".claude/settings.json", size: 200, dir: false },
      { path: ".claude/hooks", size: 0, dir: true },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    const claude = tree[0];
    expect(claude.name).toBe(".claude");
    expect(claude.isDir).toBe(true);
    expect(claude.children).toHaveLength(2);
    // dirs first in children
    expect(claude.children[0].name).toBe("hooks");
    expect(claude.children[1].name).toBe("settings.json");
  });

  it("does not duplicate dirs when both dir entry and nested children exist", () => {
    // This is the key bug that was fixed — dir entry at root + nested child
    // should NOT create two .claude nodes
    const files = [
      { path: ".agents", size: 0, dir: true },
      { path: ".claude", size: 0, dir: true },
      { path: ".claude/settings.json", size: 767, dir: false },
      { path: ".claude/settings.local.json", size: 278, dir: false },
    ];
    const tree = buildTree(files);
    const claudeNodes = tree.filter((n) => n.name === ".claude");
    expect(claudeNodes).toHaveLength(1);
    expect(claudeNodes[0].children).toHaveLength(2);
  });

  it("creates implicit parent dirs for deeply nested files", () => {
    const files = [
      { path: "src/lib/utils.ts", size: 300, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    expect(tree[0].name).toBe("src");
    expect(tree[0].isDir).toBe(true);
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].name).toBe("lib");
    expect(tree[0].children[0].children).toHaveLength(1);
    expect(tree[0].children[0].children[0].name).toBe("utils.ts");
  });

  it("handles nested dir entries without duplicating", () => {
    // Nested dir entry like ".claude/.claude" scenario from lazy loading
    const files = [
      { path: ".claude", size: 0, dir: true },
      { path: ".claude/.claude", size: 0, dir: true },
      { path: ".claude/.claude/settings.json", size: 100, dir: false },
    ];
    const tree = buildTree(files);
    expect(tree).toHaveLength(1);
    const outer = tree[0];
    expect(outer.name).toBe(".claude");
    const inner = outer.children.find((c) => c.name === ".claude");
    expect(inner).toBeDefined();
    expect(inner!.children).toHaveLength(1);
    expect(inner!.children[0].name).toBe("settings.json");
  });

  it("merges children when dir entry comes after nested files (sort order)", () => {
    // Files arrive in any order — buildTree sorts dirs first
    const files = [
      { path: "scripts/deploy.sh", size: 50, dir: false },
      { path: "scripts", size: 0, dir: true },
    ];
    const tree = buildTree(files);
    const scriptsNodes = tree.filter((n) => n.name === "scripts");
    expect(scriptsNodes).toHaveLength(1);
    expect(scriptsNodes[0].children).toHaveLength(1);
    expect(scriptsNodes[0].children[0].name).toBe("deploy.sh");
  });
});
