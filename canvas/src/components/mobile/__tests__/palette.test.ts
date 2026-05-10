import { describe, expect, it } from "vitest";

import { MOL_DARK, MOL_LIGHT, getPalette, normalizeStatus, tierCode } from "../palette";

describe("normalizeStatus", () => {
  it("passes design-known statuses through verbatim", () => {
    expect(normalizeStatus("online")).toBe("online");
    expect(normalizeStatus("degraded")).toBe("degraded");
    expect(normalizeStatus("failed")).toBe("failed");
    expect(normalizeStatus("paused")).toBe("paused");
    expect(normalizeStatus("offline")).toBe("offline");
  });

  it("maps platform 'provisioning' to design 'starting'", () => {
    // The platform's 14-state machine collapses to the design's 6 keys.
    // 'provisioning' (post-spawn boot) is the same UX bucket as 'starting'.
    expect(normalizeStatus("provisioning")).toBe("starting");
    expect(normalizeStatus("starting")).toBe("starting");
  });

  it("maps unknown / null / empty to offline", () => {
    expect(normalizeStatus(undefined)).toBe("offline");
    expect(normalizeStatus(null)).toBe("offline");
    expect(normalizeStatus("")).toBe("offline");
    expect(normalizeStatus("garbage-status")).toBe("offline");
  });
});

describe("tierCode", () => {
  it("maps numeric tiers to T-codes", () => {
    expect(tierCode(1)).toBe("T1");
    expect(tierCode(2)).toBe("T2");
    expect(tierCode(3)).toBe("T3");
    expect(tierCode(4)).toBe("T4");
  });

  it("clamps below-1 to T1 (never below sandboxed)", () => {
    expect(tierCode(0)).toBe("T1");
    expect(tierCode(-5)).toBe("T1");
  });

  it("clamps above-4 to T4 (never above full-access)", () => {
    expect(tierCode(5)).toBe("T4");
    expect(tierCode(99)).toBe("T4");
  });

  it("falls back to T2 (Standard) on null/undefined", () => {
    // T2 is the platform default for fresh agents — matches the
    // CreateWorkspaceDialog default. Keeps the mobile spawn UX
    // consistent with the desktop when tier metadata is missing.
    expect(tierCode(undefined)).toBe("T2");
    expect(tierCode(null)).toBe("T2");
  });
});

describe("getPalette", () => {
  it("returns the light palette when dark is false", () => {
    expect(getPalette(false)).toBe(MOL_LIGHT);
  });

  it("returns the dark palette when dark is true", () => {
    expect(getPalette(true)).toBe(MOL_DARK);
  });

  it("light + dark palettes have the same key set (no drift)", () => {
    expect(Object.keys(MOL_LIGHT).sort()).toEqual(Object.keys(MOL_DARK).sort());
  });
});
