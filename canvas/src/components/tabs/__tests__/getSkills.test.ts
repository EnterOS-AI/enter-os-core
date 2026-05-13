// @vitest-environment jsdom
/**
 * Unit tests for getSkills — pure helper from DetailsTab.
 *
 * Covers: null card, non-array skills, empty skills, id-only entries,
 * name-only entries (id derives from name), entries with description,
 * entries with neither id nor name (filtered out), mixed entries.
 */
import { describe, it, expect } from "vitest";
import { getSkills } from "../DetailsTab";

describe("getSkills", () => {
  it("returns [] for null card", () => {
    expect(getSkills(null)).toEqual([]);
  });

  it("returns [] when card.skills is not an array", () => {
    expect(getSkills({ skills: undefined })).toEqual([]);
    expect(getSkills({ skills: "not-an-array" })).toEqual([]);
    expect(getSkills({ skills: { id: "x" } })).toEqual([]);
  });

  it("returns [] for empty skills array", () => {
    expect(getSkills({ skills: [] })).toEqual([]);
  });

  it("maps skill with id and description", () => {
    const card = { skills: [{ id: "code_search", description: "Find code patterns" }] };
    expect(getSkills(card)).toEqual([{ id: "code_search", description: "Find code patterns" }]);
  });

  it("maps skill with id only (description absent)", () => {
    const card = { skills: [{ id: "code_search" }] };
    expect(getSkills(card)).toEqual([{ id: "code_search", description: undefined }]);
  });

  it("derives id from name when id is absent", () => {
    const card = { skills: [{ name: "web_scraper" }] };
    expect(getSkills(card)).toEqual([{ id: "web_scraper" }]);
  });

  it("maps description when present", () => {
    const card = { skills: [{ id: "file_write", description: "Writes files to disk" }] };
    expect(getSkills(card)).toEqual([{ id: "file_write", description: "Writes files to disk" }]);
  });

  it("returns description as undefined when skill has no description", () => {
    const card = { skills: [{ id: "noop_skill" }] };
    const result = getSkills(card);
    // The map always includes description; it's undefined when absent
    expect(result).toEqual([{ id: "noop_skill", description: undefined }]);
  });

  it("filters out skills with neither id nor name", () => {
    // id: String(undefined || undefined || "") → "" → filtered
    const card = { skills: [{ description: "loner" }] };
    expect(getSkills(card)).toEqual([]);
  });

  it("handles mixed valid/invalid entries", () => {
    const card = {
      skills: [
        { id: "valid_one" },
        { name: "named_skill" },
        { description: "orphaned" },   // filtered
        { id: "valid_two", description: "Has both" },
      ],
    };
    expect(getSkills(card)).toEqual([
      { id: "valid_one", description: undefined },
      { id: "named_skill", description: undefined },
      { id: "valid_two", description: "Has both" },
    ]);
  });

  it("handles string coercion for numeric ids/names", () => {
    const card = { skills: [{ id: 42, name: "numeric_id" }] };
    expect(getSkills(card)).toEqual([{ id: "42" }]);
  });

  it("uses id over name when both are present", () => {
    const card = { skills: [{ id: "priority_id", name: "fallback_name" }] };
    expect(getSkills(card)).toEqual([{ id: "priority_id", description: undefined }]);
  });

  it("omits description when it is falsy (0 is falsy in JS)", () => {
    // The implementation uses `s.description ?` — 0 is falsy, so it's treated
    // as absent and undefined is returned. Non-zero numbers coerce fine.
    const cardZero = { skills: [{ id: "x", description: 0 }] };
    expect(getSkills(cardZero)).toEqual([{ id: "x", description: undefined }]);

    const cardNum = { skills: [{ id: "x", description: 42 }] };
    expect(getSkills(cardNum)).toEqual([{ id: "x", description: "42" }]);
  });
});
