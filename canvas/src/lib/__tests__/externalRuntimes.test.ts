/**
 * Tests for `isExternalLikeRuntime` — mirrors the backend's
 * isExternalLikeRuntime() in workspace-server/internal/handlers/runtime_registry.go.
 *
 * These runtimes have no platform-owned container (no Files, Terminal, Docker config).
 * Both frontend and backend must agree on which runtimes are "external-like" so
 * the canvas can show/hide those tabs correctly and the backend can enforce
 * the same semantics server-side.
 */
import { describe, it, expect } from "vitest";
import { isExternalLikeRuntime } from "../externalRuntimes";

describe("isExternalLikeRuntime", () => {
  describe("known external-like runtimes", () => {
    it.each([
      ["external"],
      ["kimi"],
      ["kimi-cli"],
    ])("%q returns true", (runtime) => {
      expect(isExternalLikeRuntime(runtime)).toBe(true);
    });
  });

  describe("non-external runtimes", () => {
    it.each([
      "claude-code",
      "hermes",
      "docker",
      "local",
      "agent",
      "crewai",
      "langgraph",
      "openclaw",
      "custom-runtime",
    ])("%q returns false", (runtime) => {
      expect(isExternalLikeRuntime(runtime)).toBe(false);
    });
  });

  describe("edge cases", () => {
    it("returns false for undefined", () => {
      expect(isExternalLikeRuntime(undefined)).toBe(false);
    });

    it("returns false for null", () => {
      // @ts-expect-error — intentional runtime test, null is not a valid type
      expect(isExternalLikeRuntime(null)).toBe(false);
    });

    it("returns false for empty string", () => {
      expect(isExternalLikeRuntime("")).toBe(false);
    });

    it("is case-sensitive — kimi vs KIMI vs Kimi", () => {
      expect(isExternalLikeRuntime("KIMI")).toBe(false);
      expect(isExternalLikeRuntime("Kimi")).toBe(false);
      expect(isExternalLikeRuntime("kimi")).toBe(true);
    });
  });
});
