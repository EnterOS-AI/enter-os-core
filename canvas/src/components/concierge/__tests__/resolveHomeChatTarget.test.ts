// Home chat panel target — selecting an agent in the sidebar switches the
// chat; the root is only the DEFAULT, not a hard-point (the pre-fix bug).
import { describe, it, expect } from "vitest";
import { resolveHomeChatTarget } from "../ConciergeShell";

const root = { id: "root" };
const child = { id: "child" };
const nodes = [root, child];

describe("resolveHomeChatTarget", () => {
  it("returns the selected agent when it exists (the bug: chat stayed on root)", () => {
    expect(resolveHomeChatTarget(nodes, "child", root)).toBe(child);
  });
  it("falls back to the platform root when nothing is selected", () => {
    expect(resolveHomeChatTarget(nodes, null, root)).toBe(root);
  });
  it("degrades to the root when the selection no longer exists (deleted agent)", () => {
    expect(resolveHomeChatTarget(nodes, "gone", root)).toBe(root);
  });
  it("selecting the root itself targets the root", () => {
    expect(resolveHomeChatTarget(nodes, "root", root)).toBe(root);
  });
  it("null when there is neither selection nor root", () => {
    expect(resolveHomeChatTarget([], null, null)).toBeNull();
  });
});
