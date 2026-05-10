// @vitest-environment jsdom
/**
 * Tests for runtimeProfiles.ts — getRuntimeProfile and provisionTimeoutForRuntime.
 */
import { describe, expect, it } from "vitest";
import {
  getRuntimeProfile,
  provisionTimeoutForRuntime,
  DEFAULT_RUNTIME_PROFILE,
  RUNTIME_PROFILES,
} from "../runtimeProfiles";

describe("getRuntimeProfile", () => {
  it("returns DEFAULT_RUNTIME_PROFILE when runtime is undefined and no overrides", () => {
    const result = getRuntimeProfile(undefined);
    expect(result.provisionTimeoutMs).toBe(DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs);
  });

  it("returns DEFAULT_RUNTIME_PROFILE when runtime is empty string", () => {
    const result = getRuntimeProfile("");
    expect(result.provisionTimeoutMs).toBe(DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs);
  });

  it("falls back to DEFAULT_RUNTIME_PROFILE for an unknown runtime", () => {
    const result = getRuntimeProfile("unknown-lang");
    expect(result.provisionTimeoutMs).toBe(DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs);
  });

  it("returns DEFAULT_RUNTIME_PROFILE when RUNTIME_PROFILES is empty (current state)", () => {
    // RUNTIME_PROFILES is currently {} — verify the empty-map path works
    expect(RUNTIME_PROFILES).toEqual({});
    const result = getRuntimeProfile("claude-code");
    expect(result.provisionTimeoutMs).toBe(120_000);
  });

  it("uses overrides.provisionTimeoutMs when provided (highest priority)", () => {
    const result = getRuntimeProfile("claude-code", { provisionTimeoutMs: 300_000 });
    expect(result.provisionTimeoutMs).toBe(300_000);
  });

  it("overrides wins over RUNTIME_PROFILES entry", () => {
    // Even if RUNTIME_PROFILES had an entry, overrides take priority
    const result = getRuntimeProfile("claude-code", { provisionTimeoutMs: 999_000 });
    expect(result.provisionTimeoutMs).toBe(999_000);
  });

  it("uses overrides even when runtime is undefined", () => {
    const result = getRuntimeProfile(undefined, { provisionTimeoutMs: 60_000 });
    expect(result.provisionTimeoutMs).toBe(60_000);
  });

  it("returns Required<Pick> — always has provisionTimeoutMs", () => {
    // The return type is guaranteed non-nullable
    const result = getRuntimeProfile(undefined);
    expect(typeof result.provisionTimeoutMs).toBe("number");
    expect(result.provisionTimeoutMs).toBeGreaterThan(0);
  });
});

describe("provisionTimeoutForRuntime", () => {
  it("returns DEFAULT_RUNTIME_PROFILE value when no runtime or overrides", () => {
    expect(provisionTimeoutForRuntime(undefined)).toBe(120_000);
    expect(provisionTimeoutForRuntime("")).toBe(120_000);
  });

  it("returns overrides value when overrides provided", () => {
    expect(provisionTimeoutForRuntime("claude-code", { provisionTimeoutMs: 90_000 })).toBe(90_000);
  });

  it("returns 120_000 for any unknown runtime", () => {
    expect(provisionTimeoutForRuntime("langgraph")).toBe(120_000);
    expect(provisionTimeoutForRuntime("crewai")).toBe(120_000);
    expect(provisionTimeoutForRuntime("some-new-runtime")).toBe(120_000);
  });

  it("convenience: same as getRuntimeProfile().provisionTimeoutMs", () => {
    const cases: Array<[string | undefined, { provisionTimeoutMs?: number } | undefined]> = [
      [undefined, undefined],
      ["claude-code", undefined],
      ["langgraph", { provisionTimeoutMs: 500_000 }],
      [undefined, { provisionTimeoutMs: 45_000 }],
    ];
    for (const [runtime, overrides] of cases) {
      const profile = getRuntimeProfile(runtime, overrides);
      const direct = provisionTimeoutForRuntime(runtime, overrides);
      expect(direct).toBe(profile.provisionTimeoutMs);
    }
  });
});
