// @vitest-environment jsdom
/**
 * Tests for MissingKeysModal's providerIdForModel helper.
 *
 * Covers: model match, no match, empty modelId, whitespace-only modelId,
 * model with no required_env, models undefined, single vs multiple env vars,
 * stable sort order for env var ordering.
 */
import { describe, expect, it } from "vitest";
import { providerIdForModel } from "../MissingKeysModal";

describe("providerIdForModel — match behavior", () => {
  it("returns sorted-joined env vars when model is found", () => {
    const models = [
      { id: "claude-3-5-sonnet", name: "Claude 3.5 Sonnet", required_env: ["ANTHROPIC_API_KEY"] },
    ];
    expect(providerIdForModel("claude-3-5-sonnet", models)).toBe("ANTHROPIC_API_KEY");
  });

  it("returns null when model is not found", () => {
    const models = [
      { id: "claude-3-5-sonnet", name: "Claude 3.5 Sonnet", required_env: ["ANTHROPIC_API_KEY"] },
    ];
    expect(providerIdForModel("unknown-model", models)).toBeNull();
  });

  it("returns null when models is undefined", () => {
    expect(providerIdForModel("claude-3-5-sonnet", undefined)).toBeNull();
  });

  it("returns null when modelId is empty string", () => {
    const models = [{ id: "claude", name: "Claude", required_env: ["KEY"] }];
    expect(providerIdForModel("", models)).toBeNull();
  });

  it("returns null when modelId is whitespace-only", () => {
    const models = [{ id: "claude", name: "Claude", required_env: ["KEY"] }];
    expect(providerIdForModel("   ", models)).toBeNull();
  });

  it("trims whitespace from modelId before matching", () => {
    const models = [{ id: "claude", name: "Claude", required_env: ["KEY"] }];
    expect(providerIdForModel("  claude  ", models)).toBe("KEY");
  });
});

describe("providerIdForModel — required_env variations", () => {
  it("returns null when model has no required_env", () => {
    const models = [{ id: "local-model", name: "Local Model", required_env: [] }];
    expect(providerIdForModel("local-model", models)).toBeNull();
  });

  it("returns null when model.required_env is undefined", () => {
    const models = [{ id: "local-model", name: "Local Model" }] as Array<{
      id: string;
      name: string;
      required_env?: string[];
    }>;
    expect(providerIdForModel("local-model", models)).toBeNull();
  });

  it("sorts and joins multiple required_env alphabetically", () => {
    const models = [
      { id: "openrouter", name: "OpenRouter", required_env: ["OPENAI_API_KEY", "ANTHROPIC_API_KEY"] },
    ];
    // Expected: alphabetically sorted = ANTHROPIC_API_KEY|OPENAI_API_KEY
    expect(providerIdForModel("openrouter", models)).toBe("ANTHROPIC_API_KEY|OPENAI_API_KEY");
  });
});
