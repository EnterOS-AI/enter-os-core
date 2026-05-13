// @vitest-environment jsdom
/**
 * Unit tests for extractSkills — pure helper from SkillsTab.
 *
 * Covers: null card, non-array skills, empty skills, full skill entries
 * (id, name, description, tags, examples), id-only fallback, name-only
 * fallback, string coercion, array coercion for tags/examples,
 * filtering entries with no id after coercion, empty string id (filtered).
 */
import { describe, it, expect } from "vitest";
import { extractSkills } from "../SkillsTab";

describe("extractSkills", () => {
  it("returns [] for null card", () => {
    expect(extractSkills(null)).toEqual([]);
  });

  it("returns [] when card.skills is not an array", () => {
    expect(extractSkills({ skills: undefined })).toEqual([]);
    expect(extractSkills({ skills: "not-an-array" })).toEqual([]);
    expect(extractSkills({ skills: { id: "x" } })).toEqual([]);
  });

  it("returns [] for empty skills array", () => {
    expect(extractSkills({ skills: [] })).toEqual([]);
  });

  it("maps a fully-populated skill entry", () => {
    const card = {
      skills: [
        {
          id: "code_search",
          name: "Code Search",
          description: "Semantic code search",
          tags: ["search", "code"],
          examples: ["Find unused exports", "Search by AST pattern"],
        },
      ],
    };
    expect(extractSkills(card)).toEqual([
      {
        id: "code_search",
        name: "Code Search",
        description: "Semantic code search",
        tags: ["search", "code"],
        examples: ["Find unused exports", "Search by AST pattern"],
      },
    ]);
  });

  it("uses name as id when id is absent", () => {
    const card = { skills: [{ name: "web_scraper" }] };
    expect(extractSkills(card)).toEqual([
      { id: "web_scraper", name: "web_scraper", description: "", tags: [], examples: [] },
    ]);
  });

  it("uses id as name when name is absent", () => {
    const card = { skills: [{ id: "legacy_skill" }] };
    expect(extractSkills(card)).toEqual([
      { id: "legacy_skill", name: "legacy_skill", description: "", tags: [], examples: [] },
    ]);
  });

  it("filters out entries with neither id nor name", () => {
    // id: String(undefined || undefined || "") → "" → filtered (id.length = 0)
    const card = { skills: [{ description: "orphan entry" }] };
    expect(extractSkills(card)).toEqual([]);
  });

  it("filters out entries with no id after string coercion", () => {
    // id resolves to "" after String(undefined || null || {})
    const card = { skills: [{ id: null, name: null }] };
    expect(extractSkills(card)).toEqual([]);
  });

  it("filters out entries with empty-string id", () => {
    const card = { skills: [{ id: "", name: "" }] };
    expect(extractSkills(card)).toEqual([]);
  });

  it("coerces numeric tags to strings", () => {
    const card = { skills: [{ id: "x", tags: [1, "two", 3] }] };
    expect(extractSkills(card)).toEqual([
      { id: "x", name: "x", description: "", tags: ["1", "two", "3"], examples: [] },
    ]);
  });

  it("coerces non-array tags to empty array", () => {
    const card = { skills: [{ id: "x", tags: "not-an-array" }] };
    expect(extractSkills(card)).toEqual([
      { id: "x", name: "x", description: "", tags: [], examples: [] },
    ]);
  });

  it("coerces non-array examples to empty array", () => {
    const card = { skills: [{ id: "x", examples: 42 }] };
    expect(extractSkills(card)).toEqual([
      { id: "x", name: "x", description: "", tags: [], examples: [] },
    ]);
  });

  // NOTE: extractSkills uses `String(skill.description || "")` — falsy values
  // (0, null, false) fall through to "", NOT to their string form.
  it("returns '' for falsy description values (0, null, false)", () => {
    const card = { skills: [{ id: "x", description: 0 }] };
    expect(extractSkills(card)).toEqual([
      { id: "x", name: "x", description: "", tags: [], examples: [] },
    ]);
  });

  it("handles mixed valid/invalid entries", () => {
    const card = {
      skills: [
        { id: "valid_one", name: "One" },
        { name: "named_only" },
        { description: "orphan" },               // filtered — id becomes ""
        { id: "valid_two", examples: ["a", "b"] },
      ],
    };
    expect(extractSkills(card)).toEqual([
      { id: "valid_one", name: "One", description: "", tags: [], examples: [] },
      { id: "named_only", name: "named_only", description: "", tags: [], examples: [] },
      { id: "valid_two", name: "valid_two", description: "", tags: [], examples: ["a", "b"] },
    ]);
  });

  it("handles a realistic agent card with multiple skills", () => {
    const card = {
      skills: [
        { id: "web_search", name: "Web Search", description: "Search the web", tags: ["search"], examples: ["Latest news"] },
        { id: "file_read", name: "Read Files", description: "Read from disk", tags: ["io"], examples: [] },
      ],
    };
    const result = extractSkills(card);
    expect(result).toHaveLength(2);
    expect(result[0].id).toBe("web_search");
    expect(result[1].tags).toEqual(["io"]);
  });
});
