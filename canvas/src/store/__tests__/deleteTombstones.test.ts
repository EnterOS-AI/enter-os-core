import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  __resetTombstonesForTest,
  __tombstoneCountForTest,
  markDeleted,
  wasRecentlyDeleted,
} from "../deleteTombstones";

// Tombstone TTL is hardcoded at 10s in the module — these tests freeze
// time so the GC + read-time expiry can be exercised deterministically
// without sleeping.

describe("deleteTombstones (#2069)", () => {
  beforeEach(() => {
    __resetTombstonesForTest();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-04-26T20:00:00Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
    __resetTombstonesForTest();
  });

  it("flags ids as recently deleted immediately after markDeleted", () => {
    markDeleted(["root-1", "child-a"]);
    expect(wasRecentlyDeleted("root-1")).toBe(true);
    expect(wasRecentlyDeleted("child-a")).toBe(true);
  });

  it("returns false for ids that were never marked", () => {
    markDeleted(["root-1"]);
    expect(wasRecentlyDeleted("never-deleted")).toBe(false);
  });

  it("expires tombstones after the 10s TTL", () => {
    markDeleted(["root-1"]);
    expect(wasRecentlyDeleted("root-1")).toBe(true);
    vi.advanceTimersByTime(9_999);
    expect(wasRecentlyDeleted("root-1")).toBe(true);
    vi.advanceTimersByTime(2);
    expect(wasRecentlyDeleted("root-1")).toBe(false);
  });

  it("evicts expired entries on read so the map stays bounded", () => {
    markDeleted(["root-1"]);
    expect(__tombstoneCountForTest()).toBe(1);
    vi.advanceTimersByTime(11_000);
    // The read itself triggers eviction — no separate GC pass needed.
    wasRecentlyDeleted("root-1");
    expect(__tombstoneCountForTest()).toBe(0);
  });

  it("evicts expired entries on write so the map stays bounded across long sessions", () => {
    markDeleted(["root-1"]);
    expect(__tombstoneCountForTest()).toBe(1);
    vi.advanceTimersByTime(11_000);
    // markDeleted GCs before inserting, so the second write should
    // evict root-1 (now stale) AND insert root-2 — net size 1, not 2.
    markDeleted(["root-2"]);
    expect(__tombstoneCountForTest()).toBe(1);
    expect(wasRecentlyDeleted("root-1")).toBe(false);
    expect(wasRecentlyDeleted("root-2")).toBe(true);
  });

  it("resets the deletedAt timestamp when the same id is marked again", () => {
    markDeleted(["root-1"]);
    vi.advanceTimersByTime(8_000);
    // Same id re-deleted (rare, but legal) — TTL restarts from now.
    markDeleted(["root-1"]);
    vi.advanceTimersByTime(8_000);
    // 16s after the FIRST mark; would have expired without the re-mark.
    expect(wasRecentlyDeleted("root-1")).toBe(true);
  });

  it("accepts any iterable, not just arrays", () => {
    const ids = new Set(["root-1", "root-2"]);
    markDeleted(ids);
    expect(wasRecentlyDeleted("root-1")).toBe(true);
    expect(wasRecentlyDeleted("root-2")).toBe(true);
  });
});
