// @vitest-environment jsdom
/**
 * Tests for getIcon — the pure icon-selector from FilesTab/tree.ts.
 */
import { describe, it, expect } from "vitest";
import { getIcon } from "../tabs/FilesTab/tree";

describe("getIcon", () => {
  // ─── Directories ──────────────────────────────────────────────────────────

  it("returns 📁 for directories regardless of extension", () => {
    expect(getIcon("src", true)).toBe("📁");
    expect(getIcon("node_modules", true)).toBe("📁");
    expect(getIcon(".claude", true)).toBe("📁");
    expect(getIcon("foo/bar/baz", true)).toBe("📁");
  });

  it("returns 📁 even for paths that look like files", () => {
    expect(getIcon("foo.txt", true)).toBe("📁");
    expect(getIcon("script.sh", true)).toBe("📁");
  });

  // ─── Files by extension ────────────────────────────────────────────────────

  it("returns 📄 for .md files", () => {
    expect(getIcon("README.md", false)).toBe("📄");
    expect(getIcon("CHANGELOG.md", false)).toBe("📄");
    expect(getIcon("docs/guide.md", false)).toBe("📄");
  });

  it("returns ⚙ for .yaml and .yml files", () => {
    expect(getIcon("config.yaml", false)).toBe("⚙");
    expect(getIcon("values.yml", false)).toBe("⚙");
    expect(getIcon("deploy.yaml", false)).toBe("⚙");
  });

  it("returns 🐍 for .py files", () => {
    expect(getIcon("main.py", false)).toBe("🐍");
    expect(getIcon("utils/helpers.py", false)).toBe("🐍");
  });

  it("returns 💠 for .ts and .tsx files", () => {
    expect(getIcon("index.ts", false)).toBe("💠");
    expect(getIcon("Component.tsx", false)).toBe("💠");
    expect(getIcon("types.d.ts", false)).toBe("💠");
  });

  it("returns 📜 for .js files", () => {
    expect(getIcon("bundle.js", false)).toBe("📜");
    expect(getIcon("src/index.js", false)).toBe("📜");
  });

  it("returns {} for .json files", () => {
    expect(getIcon("package.json", false)).toBe("{}");
    expect(getIcon("config.json", false)).toBe("{}");
  });

  it("returns 🌐 for .html files", () => {
    expect(getIcon("index.html", false)).toBe("🌐");
    expect(getIcon("templates/page.html", false)).toBe("🌐");
  });

  it("returns 🎨 for .css files", () => {
    expect(getIcon("style.css", false)).toBe("🎨");
    expect(getIcon("src/app.css", false)).toBe("🎨");
  });

  it("returns ▸ for .sh files", () => {
    expect(getIcon("deploy.sh", false)).toBe("▸");
    expect(getIcon("scripts/setup.sh", false)).toBe("▸");
  });

  // ─── Fallback ─────────────────────────────────────────────────────────────

  it("returns 📄 for unknown extensions", () => {
    expect(getIcon("README", false)).toBe("📄");
    expect(getIcon("Dockerfile", false)).toBe("📄");
    expect(getIcon("Makefile", false)).toBe("📄");
    expect(getIcon("notes.txt", false)).toBe("📄");
    expect(getIcon("archive.tar.gz", false)).toBe("📄");
  });

  it("returns 📄 for paths with no extension", () => {
    expect(getIcon("Makefile", false)).toBe("📄");
    expect(getIcon("README", false)).toBe("📄");
    expect(getIcon("Dockerfile", false)).toBe("📄");
  });

  // ─── Case sensitivity ──────────────────────────────────────────────────────

  it("is case-insensitive for extension lookup", () => {
    expect(getIcon("image.PNG", false)).toBe("📄");
    expect(getIcon("data.JSON", false)).toBe("{}");
    expect(getIcon("script.SH", false)).toBe("▸");
  });

  // ─── Nested paths ─────────────────────────────────────────────────────────

  it("uses the leaf extension for nested paths", () => {
    expect(getIcon("src/utils/helpers.ts", false)).toBe("💠");
    expect(getIcon("docs/api.yaml", false)).toBe("⚙");
    expect(getIcon(".github/workflows/ci.yml", false)).toBe("⚙");
  });
});
