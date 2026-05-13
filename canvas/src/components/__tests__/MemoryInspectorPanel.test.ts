// @vitest-environment jsdom
/**
 * Unit tests for pure helpers from MemoryInspectorPanel:
 *   isPluginUnavailableError, formatRelativeTime, formatTTL
 *
 * These are the three exported non-component functions. The component
 * itself (MemoryInspectorPanel) requires full API + store mocking and
 * is exercised by the existing MemoryTab.test.tsx.
 */
import { describe, it, expect } from "vitest";
import { isPluginUnavailableError, formatTTL } from "../MemoryInspectorPanel";

// formatRelativeTime is not exported — tested via the component in MemoryTab.test.tsx

describe("isPluginUnavailableError", () => {
  it("returns true when Error message contains MEMORY_PLUGIN_URL", () => {
    const err = new Error("memory: could not resolve MEMORY_PLUGIN_URL — plugin not configured");
    expect(isPluginUnavailableError(err)).toBe(true);
  });

  it("returns true for Error containing MEMORY_PLUGIN_URL", () => {
    expect(isPluginUnavailableError(new Error("MEMORY_PLUGIN_URL is not set"))).toBe(true);
  });

  it("returns false for unrelated error messages", () => {
    expect(isPluginUnavailableError(new Error("workspace not found"))).toBe(false);
  });

  it("returns false for null", () => {
    expect(isPluginUnavailableError(null)).toBe(false);
  });

  it("returns false for undefined", () => {
    expect(isPluginUnavailableError(undefined)).toBe(false);
  });

  it("returns false for plain objects without message", () => {
    expect(isPluginUnavailableError({ code: 503 })).toBe(false);
  });

  it("is case-sensitive (MEMORY_PLUGIN_URL must match exactly)", () => {
    const lowerErr = new Error("memory_plugin_url missing");
    const upperErr = new Error("MEMORY_PLUGIN_URL missing");
    expect(isPluginUnavailableError(lowerErr)).toBe(false);
    expect(isPluginUnavailableError(upperErr)).toBe(true);
  });
});

describe("formatTTL", () => {
  it("returns '' for null", () => {
    expect(formatTTL(null)).toBe("");
  });

  it("returns '' for undefined", () => {
    expect(formatTTL(undefined)).toBe("");
  });

  it('returns "expired" when expiresAt is in the past', () => {
    const past = new Date(Date.now() - 60_000).toISOString();
    expect(formatTTL(past)).toBe("expired");
  });

  it('returns "Xs" for less than a minute', () => {
    const soon = new Date(Date.now() + 30_000).toISOString();
    expect(formatTTL(soon)).toBe("30s");
  });

  it('returns "Xm" for less than an hour', () => {
    const soon = new Date(Date.now() + 5 * 60_000).toISOString();
    expect(formatTTL(soon)).toBe("5m");
  });

  it('returns "Xh" for less than a day', () => {
    const soon = new Date(Date.now() + 3 * 3_600_000).toISOString();
    expect(formatTTL(soon)).toBe("3h");
  });

  it('returns "Xd" for more than a day', () => {
    const soon = new Date(Date.now() + 2 * 86_400_000).toISOString();
    expect(formatTTL(soon)).toBe("2d");
  });

  it("returns '' for invalid date string", () => {
    expect(formatTTL("not-a-date")).toBe("");
  });

  it("returns '' for empty string", () => {
    expect(formatTTL("")).toBe("");
  });
});
