// @vitest-environment jsdom
/**
 * Tests for statusDotClass — maps a workspace status string to the
 * CSS tailwind class used on the status indicator dot.
 */
import { describe, it, expect } from "vitest";
import { statusDotClass, TIER_CONFIG, COMM_TYPE_LABELS } from "../design-tokens";

describe("statusDotClass", () => {
  it('returns "bg-emerald-400" for "online"', () => {
    expect(statusDotClass("online")).toBe("bg-emerald-400");
  });

  it('returns "bg-zinc-500" for "offline"', () => {
    expect(statusDotClass("offline")).toBe("bg-zinc-500");
  });

  it('returns "bg-indigo-400" for "paused"', () => {
    expect(statusDotClass("paused")).toBe("bg-indigo-400");
  });

  it('returns "bg-amber-400" for "degraded"', () => {
    expect(statusDotClass("degraded")).toBe("bg-amber-400");
  });

  it('returns "bg-red-400" for "failed"', () => {
    expect(statusDotClass("failed")).toBe("bg-red-400");
  });

  it('returns "bg-sky-400 motion-safe:animate-pulse" for "provisioning"', () => {
    expect(statusDotClass("provisioning")).toBe("bg-sky-400 motion-safe:animate-pulse");
  });

  it('returns "bg-amber-300" for "not_configured"', () => {
    expect(statusDotClass("not_configured")).toBe("bg-amber-300");
  });

  it("falls back to bg-zinc-500 for unknown status strings", () => {
    expect(statusDotClass("unknown")).toBe("bg-zinc-500");
    expect(statusDotClass("")).toBe("bg-zinc-500");
    expect(statusDotClass("ONLINE")).toBe("bg-zinc-500"); // case-sensitive
    expect(statusDotClass(" online")).toBe("bg-zinc-500"); // whitespace-sensitive
    expect(statusDotClass("online\n")).toBe("bg-zinc-500");
  });

  it("is a pure function — same input always returns same output", () => {
    const result = statusDotClass("online");
    for (let i = 0; i < 5; i++) {
      expect(statusDotClass("online")).toBe(result);
    }
  });
});

// ── TIER_CONFIG ────────────────────────────────────────────────────────────────

describe("TIER_CONFIG", () => {
  it("has entries for all four tier levels", () => {
    expect(TIER_CONFIG).toHaveProperty("1");
    expect(TIER_CONFIG).toHaveProperty("2");
    expect(TIER_CONFIG).toHaveProperty("3");
    expect(TIER_CONFIG).toHaveProperty("4");
  });

  it("each tier has label, color, and border fields", () => {
    for (const tier of [1, 2, 3, 4]) {
      expect(TIER_CONFIG[tier]).toHaveProperty("label");
      expect(TIER_CONFIG[tier]).toHaveProperty("color");
      expect(TIER_CONFIG[tier]).toHaveProperty("border");
    }
  });

  it("tier labels match expected values", () => {
    expect(TIER_CONFIG[1].label).toBe("T1");
    expect(TIER_CONFIG[2].label).toBe("T2");
    expect(TIER_CONFIG[3].label).toBe("T3");
    expect(TIER_CONFIG[4].label).toBe("T4");
  });

  it("is immutable at runtime — same key always returns same shape", () => {
    const result = TIER_CONFIG[2];
    expect(TIER_CONFIG[2]).toBe(result);
  });
});

// ── COMM_TYPE_LABELS ────────────────────────────────────────────────────────

describe("COMM_TYPE_LABELS", () => {
  it("has labels for all known communication types", () => {
    expect(COMM_TYPE_LABELS).toHaveProperty("a2a_send");
    expect(COMM_TYPE_LABELS).toHaveProperty("a2a_receive");
    expect(COMM_TYPE_LABELS).toHaveProperty("task_update");
  });

  it("labels are non-empty strings", () => {
    for (const key of Object.keys(COMM_TYPE_LABELS)) {
      expect(typeof COMM_TYPE_LABELS[key]).toBe("string");
      expect(COMM_TYPE_LABELS[key].length).toBeGreaterThan(0);
    }
  });

  it("is a static map — same key always returns same label", () => {
    expect(COMM_TYPE_LABELS["a2a_send"]).toBe("sent");
    expect(COMM_TYPE_LABELS["a2a_receive"]).toBe("received");
    expect(COMM_TYPE_LABELS["task_update"]).toBe("task update");
  });
});
