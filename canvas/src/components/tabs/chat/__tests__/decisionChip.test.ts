import { describe, it, expect } from "vitest";
import { decisionForChip, decisionChipText } from "../decisionChip";

const base = { status: "approved", responderType: "user", responderId: "u-me", title: "Test approval", kind: "approval" };

describe("decisionForChip gate (core#2636, CR2: only the user's OWN response)", () => {
  it("renders for the current user's own approval", () => {
    expect(decisionForChip(base, "u-me")).toBe("approved");
  });

  it("IGNORES a different user's response (no 'You' chip for someone else)", () => {
    expect(decisionForChip({ ...base, responderId: "u-other" }, "u-me")).toBeNull();
  });

  it("ignores agent-side responses", () => {
    expect(decisionForChip({ ...base, responderType: "agent", responderId: "u-me" }, "u-me")).toBeNull();
  });

  it("ignores when responderId is empty (fail closed — never mis-attribute)", () => {
    expect(decisionForChip({ ...base, responderId: "" }, "u-me")).toBeNull();
  });

  it("maps rejected and done; ignores unknown status", () => {
    expect(decisionForChip({ ...base, status: "rejected" }, "u-me")).toBe("rejected");
    expect(decisionForChip({ ...base, status: "done" }, "u-me")).toBe("done");
    expect(decisionForChip({ ...base, status: "cancelled" }, "u-me")).toBeNull();
  });

  it("single-user 'admin' placeholder path matches on both sides", () => {
    expect(decisionForChip({ ...base, responderId: "admin" }, "admin")).toBe("approved");
  });
});

describe("decisionChipText", () => {
  it("formats with title", () => {
    expect(decisionChipText("approved", "Ship it")).toContain("approved");
    expect(decisionChipText("approved", "Ship it")).toContain("Ship it");
  });
  it("formats rejected/done verbs and the no-title fallback", () => {
    expect(decisionChipText("rejected", "")).toBe("You rejected the request");
    expect(decisionChipText("done", "")).toBe("You completed the request");
  });
});
