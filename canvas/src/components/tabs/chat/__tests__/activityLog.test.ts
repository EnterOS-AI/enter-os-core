import { describe, it, expect } from "vitest";
import { ACTIVITY_LOG_WINDOW, appendActivityLine } from "../activityLog";

describe("appendActivityLine", () => {
  it("appends a fresh line", () => {
    expect(appendActivityLine([], "📄 Read /a")).toEqual(["📄 Read /a"]);
  });

  it("collapses an immediate duplicate", () => {
    const prev = ["📄 Read /a"];
    // Same exact string twice in a row is noise — the helper should
    // return the original array reference, not a new one.
    expect(appendActivityLine(prev, "📄 Read /a")).toBe(prev);
  });

  it("keeps non-adjacent duplicates", () => {
    const prev = ["📄 Read /a", "⚡ Bash: ls"];
    expect(appendActivityLine(prev, "📄 Read /a")).toEqual([
      "📄 Read /a",
      "⚡ Bash: ls",
      "📄 Read /a",
    ]);
  });

  it("rolls off the oldest line when the window fills", () => {
    const seed = Array.from({ length: ACTIVITY_LOG_WINDOW }, (_, i) => `line-${i}`);
    const next = appendActivityLine(seed, "newest");
    expect(next.length).toBe(ACTIVITY_LOG_WINDOW);
    expect(next[next.length - 1]).toBe("newest");
    // Oldest entry is dropped — line-0 is gone.
    expect(next[0]).toBe("line-1");
  });

  it("keeps the original array reference when below the window cap", () => {
    const prev = ["a", "b"];
    const next = appendActivityLine(prev, "c");
    // Returned a new array (we appended); must NOT mutate prev.
    expect(prev).toEqual(["a", "b"]);
    expect(next).toEqual(["a", "b", "c"]);
  });
});
