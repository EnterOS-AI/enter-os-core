import { describe, it, expect, beforeEach, vi } from "vitest";

global.fetch = vi.fn();

import {
  checkDeploySecrets,
  providersFromTemplate,
  findSatisfiedProvider,
  getKeyLabel,
  getProviderLabel,
  type TemplateLike,
  type ModelSpec,
} from "../deploy-preflight";

beforeEach(() => {
  vi.clearAllMocks();
});

// -----------------------------------------------------------------------------
// Fixtures mirroring what the Go /templates endpoint returns from each
// template repo's config.yaml. Keep these minimal — we only need the
// fields the preflight reads.
// -----------------------------------------------------------------------------

const hermesModels: ModelSpec[] = [
  { id: "nousresearch/hermes-4-70b", name: "Hermes 4 70B", required_env: ["HERMES_API_KEY"] },
  { id: "nousresearch/hermes-3-405b", name: "Hermes 3 405B", required_env: ["OPENROUTER_API_KEY"] },
  { id: "anthropic/claude-opus", name: "Claude Opus", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "openai/gpt-5", name: "GPT-5 via OpenRouter", required_env: ["OPENROUTER_API_KEY"] },
  { id: "custom/local", name: "Local endpoint", required_env: [] },
];

const HERMES: TemplateLike = { runtime: "hermes", models: hermesModels };

const LANGGRAPH: TemplateLike = {
  runtime: "langgraph",
  required_env: ["OPENAI_API_KEY"],
};

const UNKNOWN: TemplateLike = { runtime: "nothing-declared" };

// -----------------------------------------------------------------------------
// providersFromTemplate
// -----------------------------------------------------------------------------

describe("providersFromTemplate", () => {
  it("groups hermes models by unique required_env tuples", () => {
    const providers = providersFromTemplate(HERMES);
    // Three distinct tuples: HERMES_API_KEY, OPENROUTER_API_KEY, ANTHROPIC_API_KEY.
    // The `custom/local` entry has required_env: [] and must be skipped.
    expect(providers.map((p) => p.id)).toEqual([
      "HERMES_API_KEY",
      "OPENROUTER_API_KEY",
      "ANTHROPIC_API_KEY",
    ]);
  });

  it("decorates labels with model counts when a provider serves multiple models", () => {
    const providers = providersFromTemplate(HERMES);
    const openrouter = providers.find((p) => p.id === "OPENROUTER_API_KEY");
    expect(openrouter?.label).toMatch(/\(2 models\)/);
    const hermes = providers.find((p) => p.id === "HERMES_API_KEY");
    expect(hermes?.label).not.toMatch(/\(\d+ models\)/);
  });

  it("preserves insertion order so the template author controls defaults", () => {
    const providers = providersFromTemplate(HERMES);
    expect(providers[0].id).toBe("HERMES_API_KEY");
  });

  it("falls back to top-level required_env when no models[] are declared", () => {
    const providers = providersFromTemplate(LANGGRAPH);
    expect(providers).toHaveLength(1);
    expect(providers[0].envVars).toEqual(["OPENAI_API_KEY"]);
  });

  it("returns [] for templates declaring no env requirements", () => {
    expect(providersFromTemplate(UNKNOWN)).toEqual([]);
  });

  it("supports multi-env providers (AND-semantics inside one option)", () => {
    const tmpl: TemplateLike = {
      runtime: "agent",
      models: [
        { id: "m", required_env: ["OPENAI_API_KEY", "SERPER_API_KEY"] },
      ],
    };
    const providers = providersFromTemplate(tmpl);
    expect(providers).toHaveLength(1);
    expect(providers[0].envVars).toEqual(["OPENAI_API_KEY", "SERPER_API_KEY"]);
  });
});

// -----------------------------------------------------------------------------
// findSatisfiedProvider
// -----------------------------------------------------------------------------

describe("findSatisfiedProvider", () => {
  it("returns the first provider whose envVars are all configured", () => {
    const providers = providersFromTemplate(HERMES);
    const satisfied = findSatisfiedProvider(
      providers,
      new Set(["ANTHROPIC_API_KEY"]),
    );
    expect(satisfied?.id).toBe("ANTHROPIC_API_KEY");
  });

  it("returns null when no provider is fully configured", () => {
    const providers = providersFromTemplate(HERMES);
    expect(findSatisfiedProvider(providers, new Set())).toBeNull();
  });

  it("requires ALL envVars in a multi-env provider", () => {
    const providers: ReturnType<typeof providersFromTemplate> =
      providersFromTemplate({
        runtime: "agent",
        models: [{ id: "m", required_env: ["A", "B"] }],
      });
    expect(findSatisfiedProvider(providers, new Set(["A"]))).toBeNull();
    expect(findSatisfiedProvider(providers, new Set(["A", "B"]))?.id).toBe("A|B");
  });
});

// -----------------------------------------------------------------------------
// Label helpers
// -----------------------------------------------------------------------------

describe("getKeyLabel / getProviderLabel", () => {
  it("uses KEY_LABELS for well-known keys", () => {
    expect(getProviderLabel("OPENAI_API_KEY")).toBe("OpenAI");
    expect(getKeyLabel("OPENAI_API_KEY")).toBe("OpenAI API Key");
  });

  it("humanizes unknown env vars", () => {
    expect(getProviderLabel("MY_CUSTOM_API_KEY")).toBe("My Custom");
    expect(getKeyLabel("MY_CUSTOM_TOKEN")).toBe("My Custom");
  });
});

// -----------------------------------------------------------------------------
// checkDeploySecrets
// -----------------------------------------------------------------------------

describe("checkDeploySecrets", () => {
  it("returns ok=true when a single-provider template's key is configured", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve([
          { key: "OPENAI_API_KEY", has_value: true, created_at: "", updated_at: "" },
        ]),
    } as Response);

    const result = await checkDeploySecrets(LANGGRAPH);
    expect(result.ok).toBe(true);
    expect(result.missingKeys).toEqual([]);
    expect(result.runtime).toBe("langgraph");
  });

  it("returns ok=true on a multi-provider template when ANY provider is configured", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve([
          { key: "ANTHROPIC_API_KEY", has_value: true, created_at: "", updated_at: "" },
        ]),
    } as Response);

    const result = await checkDeploySecrets(HERMES);
    expect(result.ok).toBe(true);
  });

  it("returns ok=false with every candidate env when nothing is configured", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([]),
    } as Response);

    const result = await checkDeploySecrets(HERMES);
    expect(result.ok).toBe(false);
    // De-duplicated flat list across providers.
    expect(new Set(result.missingKeys)).toEqual(
      new Set(["HERMES_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"]),
    );
    // Grouped providers preserved for the picker.
    expect(result.providers).toHaveLength(3);
  });

  it("treats has_value=false as not-configured", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve([
          { key: "OPENAI_API_KEY", has_value: false, created_at: "", updated_at: "" },
        ]),
    } as Response);

    const result = await checkDeploySecrets(LANGGRAPH);
    expect(result.ok).toBe(false);
    expect(result.missingKeys).toEqual(["OPENAI_API_KEY"]);
  });

  it("skips the API call entirely when the template declares no env needs", async () => {
    const result = await checkDeploySecrets(UNKNOWN);
    expect(result.ok).toBe(true);
    expect(result.missingKeys).toEqual([]);
    expect(global.fetch).not.toHaveBeenCalled();
  });

  it("uses the workspace-scoped endpoint when workspaceId is provided", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve([
          { key: "OPENAI_API_KEY", has_value: true, created_at: "", updated_at: "" },
        ]),
    } as Response);

    await checkDeploySecrets(LANGGRAPH, "ws-123");
    expect(global.fetch).toHaveBeenCalledWith(
      expect.stringContaining("/workspaces/ws-123/secrets"),
      expect.any(Object),
    );
  });

  it("uses the global secrets endpoint when no workspaceId", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([]),
    } as Response);

    await checkDeploySecrets(LANGGRAPH);
    expect(global.fetch).toHaveBeenCalledWith(
      expect.stringContaining("/settings/secrets"),
      expect.any(Object),
    );
  });

  it("treats fetch failure as all-missing (safe default prompts the user)", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error("Network error"),
    );

    const result = await checkDeploySecrets(LANGGRAPH);
    expect(result.ok).toBe(false);
    expect(result.missingKeys).toEqual(["OPENAI_API_KEY"]);
    // Empty Set on fetch failure — useTemplateDeploy relies on this
    // so the picker still opens with every entry rendered as input.
    expect(result.configuredKeys).toEqual(new Set());
  });

  it("surfaces configuredKeys (has_value=true entries only) so callers skip a second fetch", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve([
          { key: "ANTHROPIC_API_KEY", has_value: true, created_at: "", updated_at: "" },
          { key: "OPENROUTER_API_KEY", has_value: false, created_at: "", updated_at: "" },
          { key: "RANDOM_OTHER_KEY", has_value: true, created_at: "", updated_at: "" },
        ]),
    } as Response);

    const result = await checkDeploySecrets(HERMES);
    // Only has_value=true entries belong in the set.
    expect(result.configuredKeys).toEqual(
      new Set(["ANTHROPIC_API_KEY", "RANDOM_OTHER_KEY"]),
    );
  });
});
