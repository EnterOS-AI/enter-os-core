// @vitest-environment jsdom
/**
 * Tests for resolveRuntime — the template-id → runtime-name mapper in deploy-preflight.ts.
 *
 * Lives in lib/__tests__/ alongside deploy-preflight.test.ts so the
 * two share the same describe block convention and the fixture types
 * are close at hand. Separate file keeps the deploy-preflight fixture
 * count bounded.
 */
import { describe, it, expect } from "vitest";
import { isUserVisibleWorkspaceTemplate, resolveRuntime } from "../deploy-preflight";

describe("resolveRuntime", () => {
  describe("explicit runtime-map entries", () => {
    it('maps "claude-code-default" to "claude-code"', () => {
      expect(resolveRuntime("claude-code-default")).toBe("claude-code");
    });

    it('maps "codex" to "codex"', () => {
      expect(resolveRuntime("codex")).toBe("codex");
    });

    it('maps "hermes" to "hermes"', () => {
      expect(resolveRuntime("hermes")).toBe("hermes");
    });

    it('maps "openclaw" to "openclaw"', () => {
      expect(resolveRuntime("openclaw")).toBe("openclaw");
    });
  });

  describe("identity fallback for modern template ids", () => {
    it("strips trailing -default suffix as fallback", () => {
      expect(resolveRuntime("hermes-default")).toBe("hermes");
    });

    it("strips -default only when it is the suffix", () => {
      // "default-something" should NOT strip
      expect(resolveRuntime("default-custom")).toBe("default-custom");
    });

    it("returns the id unchanged when id has no -default suffix", () => {
      expect(resolveRuntime("custom-runtime")).toBe("custom-runtime");
    });

    it("handles custom template ids from community templates", () => {
      expect(resolveRuntime("my-custom-template")).toBe("my-custom-template");
    });
  });

  describe("edge cases", () => {
    it("handles empty string", () => {
      // Falls through to the replace branch
      expect(resolveRuntime("")).toBe("");
    });

    it("handles id that is just '-default'", () => {
      expect(resolveRuntime("-default")).toBe("");
    });

    it("multiple -default suffixes only strips the last one", () => {
      // The JS replace only replaces the first match by default
      expect(resolveRuntime("claude-code-default-default")).toBe("claude-code-default");
    });
  });
});

describe("isUserVisibleWorkspaceTemplate", () => {
  it("hides runtime-default templates from product template surfaces", () => {
    for (const id of ["claude-code-default", "codex", "hermes", "openclaw"]) {
      expect(isUserVisibleWorkspaceTemplate({ id })).toBe(false);
    }
  });

  it("keeps product templates visible", () => {
    expect(isUserVisibleWorkspaceTemplate({ id: "seo-agent" })).toBe(true);
  });
});
