import { describe, it, expect } from "vitest";
import {
  AUTO_PROVIDER,
  matchProviderForModel,
  modelsForProvider,
  requiredEnvFor,
  deriveProvidersFromModels,
  providerListForRuntime,
  type ProviderEntry,
  type ModelSpec,
} from "../provider-cascade";

// Mirrors the production claude-code-default registry shape so the
// tests fail loudly if the wire shape ever drifts away from what the
// workspace-server templates handler emits.
const CLAUDE_REGISTRY: ProviderEntry[] = [
  {
    name: "anthropic-oauth",
    auth_mode: "oauth",
    model_prefixes: [],
    model_aliases: ["sonnet", "opus", "haiku"],
    auth_env: ["CLAUDE_CODE_OAUTH_TOKEN"],
  },
  {
    name: "anthropic-api",
    auth_mode: "anthropic_api",
    model_prefixes: ["claude-"],
    model_aliases: [],
    auth_env: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"],
  },
  {
    name: "xiaomi-mimo",
    auth_mode: "third_party_anthropic_compat",
    model_prefixes: ["mimo-"],
    model_aliases: [],
    base_url: "https://api.xiaomimimo.com/anthropic",
    auth_env: ["ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"],
  },
];

const CLAUDE_MODELS: ModelSpec[] = [
  { id: "sonnet", name: "Claude Sonnet (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "opus", name: "Claude Opus (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "haiku", name: "Claude Haiku (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "claude-opus-4-7", name: "Claude Opus 4.7 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "claude-haiku-4-5", name: "Claude Haiku 4.5 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "mimo-v2-pro", name: "Xiaomi MiMo V2 Pro", required_env: ["ANTHROPIC_API_KEY"] },
];

describe("matchProviderForModel", () => {
  // Aliases beat prefixes in registry order. The OAuth entry comes
  // first in claude-code-default so the bare alias `sonnet` resolves
  // to anthropic-oauth — matching the adapter's boot-time behavior.
  it("matches by alias before prefix", () => {
    const got = matchProviderForModel("sonnet", CLAUDE_REGISTRY);
    expect(got?.name).toBe("anthropic-oauth");
  });

  it("matches versioned ids by prefix", () => {
    expect(matchProviderForModel("claude-opus-4-7", CLAUDE_REGISTRY)?.name).toBe("anthropic-api");
    expect(matchProviderForModel("mimo-v2-pro", CLAUDE_REGISTRY)?.name).toBe("xiaomi-mimo");
  });

  it("is case-insensitive on both sides", () => {
    expect(matchProviderForModel("OPUS", CLAUDE_REGISTRY)?.name).toBe("anthropic-oauth");
    expect(matchProviderForModel("MIMO-V2-PRO", CLAUDE_REGISTRY)?.name).toBe("xiaomi-mimo");
    const upperRegistry: ProviderEntry[] = [
      { name: "vendor", model_prefixes: ["FOO-"] },
    ];
    expect(matchProviderForModel("foo-bar", upperRegistry)?.name).toBe("vendor");
  });

  it("returns null when nothing claims the id", () => {
    expect(matchProviderForModel("unknown-model", CLAUDE_REGISTRY)).toBeNull();
  });

  it("returns null on empty inputs", () => {
    expect(matchProviderForModel("", CLAUDE_REGISTRY)).toBeNull();
    expect(matchProviderForModel("sonnet", [])).toBeNull();
  });

  it("ignores empty-string prefixes (would otherwise match everything)", () => {
    const sketchy: ProviderEntry[] = [{ name: "broken", model_prefixes: [""] }];
    expect(matchProviderForModel("anything", sketchy)).toBeNull();
  });
});

describe("modelsForProvider", () => {
  // The user-reported bug: typing "sonnet" in the datalist hides
  // every option without "sonnet" in id/name, including opus. With a
  // proper provider→model cascade and a select, picking
  // anthropic-oauth shows all three OAuth aliases including opus.
  it("filtering by anthropic-oauth shows opus alongside sonnet+haiku", () => {
    const got = modelsForProvider("anthropic-oauth", CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got.map((m) => m.id)).toEqual(["sonnet", "opus", "haiku"]);
  });

  it("filtering by anthropic-api shows the versioned prefix matches", () => {
    const got = modelsForProvider("anthropic-api", CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got.map((m) => m.id)).toEqual(["claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5"]);
  });

  it("filtering by xiaomi-mimo shows just the mimo-* models", () => {
    const got = modelsForProvider("xiaomi-mimo", CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got.map((m) => m.id)).toEqual(["mimo-v2-pro"]);
  });

  it("AUTO_PROVIDER returns the unfiltered list", () => {
    const got = modelsForProvider(AUTO_PROVIDER, CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got).toHaveLength(CLAUDE_MODELS.length);
  });

  it("empty provider name returns the unfiltered list", () => {
    const got = modelsForProvider("", CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got).toHaveLength(CLAUDE_MODELS.length);
  });

  it("unknown provider name returns the unfiltered list", () => {
    const got = modelsForProvider("not-a-provider", CLAUDE_MODELS, CLAUDE_REGISTRY);
    expect(got).toHaveLength(CLAUDE_MODELS.length);
  });

  // A model id that matches both an alias and a prefix (defensive:
  // unlikely in production but test it once) appears once. Set-based
  // dedup is the load-bearing detail.
  it("dedupes when alias + prefix would both match", () => {
    const overlap: ProviderEntry[] = [{
      name: "weird",
      model_aliases: ["claude-opus-4-7"],
      model_prefixes: ["claude-"],
    }];
    const got = modelsForProvider("weird", [{ id: "claude-opus-4-7" }], overlap);
    expect(got).toHaveLength(1);
  });

  it("handles a provider with neither aliases nor prefixes (filters everything out)", () => {
    const skeletal: ProviderEntry[] = [{ name: "empty" }];
    const got = modelsForProvider("empty", CLAUDE_MODELS, skeletal);
    expect(got).toEqual([]);
  });

  it("skips models with empty id", () => {
    const withEmpty: ModelSpec[] = [{ id: "" }, { id: "sonnet" }];
    const got = modelsForProvider("anthropic-oauth", withEmpty, CLAUDE_REGISTRY);
    expect(got.map((m) => m.id)).toEqual(["sonnet"]);
  });
});

describe("requiredEnvFor", () => {
  // OAuth path: provider says CLAUDE_CODE_OAUTH_TOKEN, model says
  // CLAUDE_CODE_OAUTH_TOKEN. Union dedupes to a single entry — UI
  // should not show the same env var name twice.
  it("dedupes overlap between provider auth_env and model required_env", () => {
    const provider = CLAUDE_REGISTRY[0];
    const model = CLAUDE_MODELS.find((m) => m.id === "sonnet")!;
    expect(requiredEnvFor(provider, model)).toEqual(["CLAUDE_CODE_OAUTH_TOKEN"]);
  });

  // API key path: provider accepts ANTHROPIC_API_KEY OR
  // ANTHROPIC_AUTH_TOKEN, model requires ANTHROPIC_API_KEY. Union is
  // ordered with provider auth_env first.
  it("orders provider auth_env before model required_env", () => {
    const provider = CLAUDE_REGISTRY[1];
    const model = CLAUDE_MODELS.find((m) => m.id === "claude-opus-4-7")!;
    expect(requiredEnvFor(provider, model)).toEqual([
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
    ]);
  });

  it("works when only provider has auth_env (model missing)", () => {
    expect(requiredEnvFor(CLAUDE_REGISTRY[0], null)).toEqual(["CLAUDE_CODE_OAUTH_TOKEN"]);
  });

  it("works when only model has required_env (provider missing)", () => {
    const model = CLAUDE_MODELS.find((m) => m.id === "claude-opus-4-7")!;
    expect(requiredEnvFor(null, model)).toEqual(["ANTHROPIC_API_KEY"]);
  });

  it("returns empty when both are missing", () => {
    expect(requiredEnvFor(null, null)).toEqual([]);
    expect(requiredEnvFor({ name: "x" }, { id: "y" })).toEqual([]);
  });
});

describe("deriveProvidersFromModels", () => {
  it("splits on `:`", () => {
    expect(deriveProvidersFromModels([
      { id: "anthropic:claude-opus-4-7" },
      { id: "openai:gpt-4o" },
    ])).toEqual(["anthropic", "openai"]);
  });

  it("splits on `/`", () => {
    expect(deriveProvidersFromModels([
      { id: "nousresearch/hermes-4-70b" },
    ])).toEqual(["nousresearch"]);
  });

  it("dedupes within run", () => {
    expect(deriveProvidersFromModels([
      { id: "anthropic:opus" },
      { id: "anthropic:sonnet" },
    ])).toEqual(["anthropic"]);
  });

  it("skips ids with no separator", () => {
    expect(deriveProvidersFromModels([
      { id: "sonnet" },
      { id: "opus" },
    ])).toEqual([]);
  });

  it("handles empty/missing ids", () => {
    expect(deriveProvidersFromModels([])).toEqual([]);
    expect(deriveProvidersFromModels([{ id: "" }])).toEqual([]);
  });
});

describe("providerListForRuntime", () => {
  // Structured registry wins. claude-code-default ships seven entries;
  // the form should pick those, not the empty flat list, and not the
  // empty derive (since the model ids don't have `:` separators).
  it("prefers the structured registry over the flat list", () => {
    const got = providerListForRuntime(CLAUDE_REGISTRY, ["should-not-show"], CLAUDE_MODELS);
    expect(got.map((p) => p.name)).toEqual(["anthropic-oauth", "anthropic-api", "xiaomi-mimo"]);
  });

  it("falls back to flat providers when registry empty", () => {
    const got = providerListForRuntime([], ["nous", "openrouter"], []);
    expect(got).toEqual([{ name: "nous" }, { name: "openrouter" }]);
  });

  it("falls back to derive when both registry and flat are empty", () => {
    const got = providerListForRuntime(undefined, [], [
      { id: "anthropic:opus" },
      { id: "openai:gpt-4o" },
    ]);
    expect(got).toEqual([{ name: "anthropic" }, { name: "openai" }]);
  });

  it("returns empty when nothing can be inferred", () => {
    expect(providerListForRuntime(undefined, [], [])).toEqual([]);
    expect(providerListForRuntime(undefined, [], [{ id: "sonnet" }])).toEqual([]);
  });
});
