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
import { resolveRuntime } from "../deploy-preflight";

describe("resolveRuntime", () => {
  describe("explicit runtime-map entries", () => {
    it('maps "langgraph" to "langgraph"', () => {
      expect(resolveRuntime("langgraph")).toBe("langgraph");
    });

    it('maps "claude-code-default" to "claude-code"', () => {
      expect(resolveRuntime("claude-code-default")).toBe("claude-code");
    });

    it('maps "openclaw" to "openclaw"', () => {
      expect(resolveRuntime("openclaw")).toBe("openclaw");
    });

    it('maps "deepagents" to "deepagents"', () => {
      expect(resolveRuntime("deepagents")).toBe("deepagents");
    });

    it('maps "crewai" to "crewai"', () => {
      expect(resolveRuntime("crewai")).toBe("crewai");
    });

    it('maps "autogen" to "autogen"', () => {
      expect(resolveRuntime("autogen")).toBe("autogen");
    });
  });

  describe("identity fallback for modern template ids", () => {
    it("returns the id unchanged when not in the map", () => {
      expect(resolveRuntime("hermes")).toBe("hermes");
    });

    it("strips trailing -default suffix as fallback", () => {
      expect(resolveRuntime("hermes-default")).toBe("hermes");
    });

    it("strips -default only when it is the suffix", () => {
      // "default-something" should NOT strip
      expect(resolveRuntime("default-langgraph")).toBe("default-langgraph");
    });

    it("returns the id unchanged when id has no -default suffix", () => {
      expect(resolveRuntime("gemini-cli")).toBe("gemini-cli");
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
