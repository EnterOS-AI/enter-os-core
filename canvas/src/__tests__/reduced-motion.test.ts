/**
 * WCAG 2.3.3 — prefers-reduced-motion compliance
 * Verifies that all animation classes are guarded by motion-safe: variants
 * and that globals.css contains the @media rule.
 */
import { describe, it, expect } from "vitest";
import { readFileSync } from "fs";
import { join } from "path";

const root = join(__dirname, "../..");

function readSrc(rel: string) {
  return readFileSync(join(root, "src", rel), "utf8");
}

function usesGuardedPulse(src: string): boolean {
  if (src.includes("motion-safe:animate-pulse")) return true;
  if (src.includes("from \"@/lib/design-tokens\"") || src.includes("from '@/lib/design-tokens'")) return true;
  return false;
}

describe("prefers-reduced-motion compliance", () => {
  it("globals.css contains @media (prefers-reduced-motion: reduce) block", () => {
    const css = readSrc("app/globals.css");
    expect(css).toContain("prefers-reduced-motion: reduce");
    expect(css).toContain("animation-duration: 0.01ms");
  });

  it("ChatTab.tsx uses motion-safe:animate-bounce, not bare animate-bounce", () => {
    const src = readSrc("components/tabs/ChatTab.tsx");
    // Must not have bare animate-bounce (not preceded by motion-safe:)
    expect(src.includes("animate-bounce") && !src.includes("motion-safe:animate-bounce")).toBe(false);
    // Must have guarded version
    expect(src).toContain("motion-safe:animate-bounce");
  });

  it("WorkspaceNode.tsx uses motion-safe:animate-pulse, not bare animate-pulse", () => {
    const src = readSrc("components/WorkspaceNode.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("StatusDot.tsx uses motion-safe:animate-pulse (inline or via shared tokens)", () => {
    const src = readSrc("components/StatusDot.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(usesGuardedPulse(src)).toBe(true);
  });

  it("Toolbar.tsx uses motion-safe:animate-pulse", () => {
    const src = readSrc("components/Toolbar.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("WorkspacePanelTabs.tsx uses motion-safe:animate-pulse", () => {
    // The connection-status dot moved out of SidePanel.tsx into the extracted
    // WorkspacePanelTabs.tsx; verify the reduced-motion guard followed it.
    const src = readSrc("components/WorkspacePanelTabs.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("Legend.tsx uses motion-safe:animate-pulse (inline or via shared tokens)", () => {
    const src = readSrc("components/Legend.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(usesGuardedPulse(src)).toBe(true);
  });

  it("SearchDialog.tsx uses motion-safe:animate-pulse (inline or via shared tokens)", () => {
    const src = readSrc("components/SearchDialog.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(usesGuardedPulse(src)).toBe(true);
  });

  it("TerminalTab.tsx uses motion-safe:animate-pulse", () => {
    const src = readSrc("components/tabs/TerminalTab.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("TemplatePalette.tsx uses motion-safe:animate-pulse", () => {
    const src = readSrc("components/TemplatePalette.tsx");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("design-tokens.ts uses motion-safe:animate-pulse, not bare animate-pulse", () => {
    const src = readSrc("lib/design-tokens.ts");
    expect(src.includes("animate-pulse") && !src.includes("motion-safe:animate-pulse")).toBe(false);
    expect(src).toContain("motion-safe:animate-pulse");
  });

  it("globals.css disables animate-in and slide-in classes under reduced-motion", () => {
    const css = readSrc("app/globals.css");
    expect(css).toContain(".animate-in");
    expect(css).toContain(".slide-in-from-bottom");
    expect(css).toContain("animation: none !important");
  });

  it("globals.css disables React Flow animated edges under reduced-motion", () => {
    const css = readSrc("app/globals.css");
    expect(css).toContain(".react-flow__edge.animated");
  });
});
