// @vitest-environment jsdom
/**
 * Tests for deriveProvidersFromModels — pure vendor-slug extractor from
 * a model list used in ConfigTab.tsx.
 *
 * Takes ModelSpec[] and returns a deduplicated array of vendor strings.
 * Vendor is derived by splitting on ":" (anthropic:claude-opus-4-7) or
 * "/" (nousresearch/hermes-4-70b). Order is preserved from input.
 */
import { describe, expect, it } from "vitest";
import { deriveProvidersFromModels } from "../ConfigTab";

// Local type mirror (not exported from ConfigTab)
interface ModelSpec {
  id?: string;
}

describe("deriveProvidersFromModels", () => {
  it("returns empty array for empty input", () => {
    expect(deriveProvidersFromModels([])).toEqual([]);
  });

  it("extracts vendor from colon-separated id", () => {
    const models: ModelSpec[] = [{ id: "anthropic:claude-sonnet-4-5" }];
    expect(deriveProvidersFromModels(models)).toEqual(["anthropic"]);
  });

  it("extracts vendor from slash-separated id", () => {
    const models: ModelSpec[] = [{ id: "nousresearch/hermes-4-70b" }];
    expect(deriveProvidersFromModels(models)).toEqual(["nousresearch"]);
  });

  it("deduplicates repeated vendors", () => {
    const models: ModelSpec[] = [
      { id: "anthropic:claude-opus-4-7" },
      { id: "anthropic:claude-sonnet-4-5" },
      { id: "openai:gpt-4o" },
    ];
    expect(deriveProvidersFromModels(models)).toEqual(["anthropic", "openai"]);
  });

  it("skips models with no id", () => {
    const models: ModelSpec[] = [
      { id: "anthropic:claude-sonnet-4-5" },
      {},
      { id: undefined },
      { id: "" },
    ];
    expect(deriveProvidersFromModels(models)).toEqual(["anthropic"]);
  });

  it("skips ids with no vendor separator", () => {
    const models: ModelSpec[] = [
      { id: "claude-sonnet-4-5" },
      { id: "unknown/runtime" },
    ];
    expect(deriveProvidersFromModels(models)).toEqual(["unknown"]);
  });

  it("skips empty string id", () => {
    const models: ModelSpec[] = [{ id: "" }];
    expect(deriveProvidersFromModels(models)).toEqual([]);
  });

  it("preserves first-occurrence order", () => {
    const models: ModelSpec[] = [
      { id: "openai:gpt-4o" },
      { id: "anthropic:claude-opus-4-7" },
      { id: "anthropic:claude-sonnet-4-5" },
      { id: "google:gemini-2-5-flash" },
    ];
    expect(deriveProvidersFromModels(models)).toEqual([
      "openai",
      "anthropic",
      "google",
    ]);
  });

  it("handles mix of valid and invalid ids", () => {
    const models: ModelSpec[] = [
      {},
      { id: "openai:gpt-4o-mini" },
      { id: "" },
      { id: "no-separator" },
      { id: "anthropic:claude-opus-4-7" },
    ];
    expect(deriveProvidersFromModels(models)).toEqual(["openai", "anthropic"]);
  });

  it("is pure — same input always returns same output", () => {
    const models: ModelSpec[] = [
      { id: "anthropic:claude-sonnet-4-5" },
      { id: "openai:gpt-4o" },
      { id: "google:gemini-2-5-flash" },
    ];
    for (let i = 0; i < 3; i++) {
      expect(deriveProvidersFromModels(models)).toEqual(["anthropic", "openai", "google"]);
    }
  });
});
