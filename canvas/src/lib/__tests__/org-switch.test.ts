import { describe, it, expect } from "vitest";
import { switchOrgUrl } from "../org-switch";

describe("switchOrgUrl", () => {
  it("builds the target org's subdomain URL from the current host", () => {
    expect(
      switchOrgUrl("agents-team.moleculesai.app", "https:", "agents-team", "acme-demo"),
    ).toBe("https://acme-demo.moleculesai.app");
  });

  it("returns null for a no-op (switching to the current org)", () => {
    expect(
      switchOrgUrl("agents-team.moleculesai.app", "https:", "agents-team", "agents-team"),
    ).toBeNull();
  });

  it("returns null when the target slug is empty", () => {
    expect(switchOrgUrl("a.example.com", "https:", "a", "")).toBeNull();
  });

  it("falls back to dropping the first label when currentSlug doesn't prefix the host", () => {
    expect(switchOrgUrl("foo.example.com", "https:", "", "bar")).toBe(
      "https://bar.example.com",
    );
  });

  it("returns null when there is no apex to derive (single-label host)", () => {
    expect(switchOrgUrl("localhost", "http:", "", "bar")).toBeNull();
  });
});
