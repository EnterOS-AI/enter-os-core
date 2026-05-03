// @vitest-environment jsdom
/**
 * ProviderModelSelector — vendor detection + dropdown cascade.
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

import {
  ProviderModelSelector,
  buildProviderCatalog,
  inferVendor,
  findProviderForModel,
  type SelectorModel,
  type SelectorValue,
} from "../ProviderModelSelector";

afterEach(() => cleanup());

// Fixture mirrors the real claude-code-default config.yaml — covers
// the env-collision scenario (9 models share ANTHROPIC_AUTH_TOKEN
// but represent 4 distinct vendors).
const CLAUDE_CODE_MODELS: SelectorModel[] = [
  { id: "sonnet", name: "Claude Sonnet (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "opus", name: "Claude Opus (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "haiku", name: "Claude Haiku (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "claude-opus-4-7", name: "Claude Opus 4.7 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "mimo-v2-flash", name: "Xiaomi MiMo Flash", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "mimo-v2-pro", name: "Xiaomi MiMo Pro", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "MiniMax-M2", name: "MiniMax M2", required_env: ["ANTHROPIC_AUTH_TOKEN"] },
  { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["ANTHROPIC_AUTH_TOKEN"] },
  { id: "GLM-4.6", name: "Z.ai GLM-4.6", required_env: ["ANTHROPIC_AUTH_TOKEN"] },
  { id: "kimi-k2", name: "Moonshot Kimi K2", required_env: ["ANTHROPIC_AUTH_TOKEN"] },
  { id: "deepseek-v4-pro", name: "DeepSeek V4 Pro", required_env: ["ANTHROPIC_AUTH_TOKEN"] },
];

const HERMES_MODELS: SelectorModel[] = [
  { id: "nousresearch/hermes-4-70b", name: "Hermes 4 70B", required_env: ["HERMES_API_KEY"] },
  { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet (direct)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "openai/gpt-5", name: "GPT-5 via OR", required_env: ["OPENROUTER_API_KEY"] },
  { id: "huggingface/*", name: "Any HF model", required_env: ["HF_TOKEN"] },
  { id: "openrouter/*", name: "Any OpenRouter model", required_env: ["OPENROUTER_API_KEY"] },
  { id: "custom/*", name: "Self-hosted endpoint", required_env: [] },
];

describe("inferVendor", () => {
  it("uses slash prefix when present", () => {
    expect(inferVendor({ id: "nousresearch/hermes-4-70b", required_env: ["HERMES_API_KEY"] }))
      .toBe("nousresearch");
    expect(inferVendor({ id: "anthropic/claude-sonnet-4-5", required_env: ["ANTHROPIC_API_KEY"] }))
      .toBe("anthropic");
    expect(inferVendor({ id: "openai/gpt-5", required_env: ["OPENROUTER_API_KEY"] }))
      .toBe("openai");
  });

  it("infers vendor from bare-id pattern when no slash", () => {
    expect(inferVendor({ id: "MiniMax-M2.7", required_env: ["ANTHROPIC_AUTH_TOKEN"] })).toBe("minimax");
    expect(inferVendor({ id: "GLM-4.6", required_env: ["ANTHROPIC_AUTH_TOKEN"] })).toBe("zai");
    expect(inferVendor({ id: "kimi-k2", required_env: ["ANTHROPIC_AUTH_TOKEN"] })).toBe("moonshot");
    expect(inferVendor({ id: "deepseek-v4-pro", required_env: ["ANTHROPIC_AUTH_TOKEN"] })).toBe("deepseek");
    expect(inferVendor({ id: "mimo-v2-flash", required_env: ["ANTHROPIC_API_KEY"] })).toBe("xiaomi-mimo");
    expect(inferVendor({ id: "claude-sonnet-4-6", required_env: ["ANTHROPIC_API_KEY"] })).toBe("anthropic");
  });

  it("treats bare sonnet/opus/haiku as anthropic-oauth ONLY when env demands OAuth", () => {
    expect(inferVendor({ id: "sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] }))
      .toBe("anthropic-oauth");
    expect(inferVendor({ id: "opus", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] }))
      .toBe("anthropic-oauth");
    // Hypothetical sonnet alias against API key — must NOT be tagged OAuth.
    expect(inferVendor({ id: "sonnet", required_env: ["ANTHROPIC_API_KEY"] }))
      .toBe("anthropic");
  });

  it("falls back to env namespace for unknown vendors", () => {
    expect(inferVendor({ id: "unknown-id", required_env: ["OPENROUTER_API_KEY"] }))
      .toBe("openrouter");
    expect(inferVendor({ id: "unknown-id", required_env: ["HERMES_API_KEY"] }))
      .toBe("hermes");
  });
});

describe("buildProviderCatalog", () => {
  it("splits ANTHROPIC_AUTH_TOKEN models by vendor (not just env)", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const vendors = catalog.map((p) => p.vendor).sort();
    // The 4 third-party vendors that share ANTHROPIC_AUTH_TOKEN must
    // all appear as separate entries.
    expect(vendors).toContain("minimax");
    expect(vendors).toContain("zai");
    expect(vendors).toContain("moonshot");
    expect(vendors).toContain("deepseek");
    // Plus the OAuth, Anthropic API, and Xiaomi MiMo entries.
    expect(vendors).toContain("anthropic-oauth");
    expect(vendors).toContain("anthropic");
    expect(vendors).toContain("xiaomi-mimo");
  });

  it("buckets models under the correct vendor", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const minimax = catalog.find((p) => p.vendor === "minimax");
    expect(minimax).toBeDefined();
    expect(minimax!.models.map((m) => m.id).sort()).toEqual(["MiniMax-M2", "MiniMax-M2.7"]);
    const oauth = catalog.find((p) => p.vendor === "anthropic-oauth");
    expect(oauth!.models.map((m) => m.id).sort()).toEqual(["haiku", "opus", "sonnet"]);
  });

  it("flags wildcard providers", () => {
    const catalog = buildProviderCatalog(HERMES_MODELS);
    const hf = catalog.find((p) => p.vendor === "huggingface");
    expect(hf?.wildcard).toBe(true);
    const custom = catalog.find((p) => p.vendor === "custom");
    expect(custom?.wildcard).toBe(true);
    const nous = catalog.find((p) => p.vendor === "nousresearch");
    expect(nous?.wildcard).toBe(false);
  });

  it("decorates label with model count when ≥2 concrete models", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const oauth = catalog.find((p) => p.vendor === "anthropic-oauth");
    expect(oauth?.label).toMatch(/3 models/);
    // Wildcard buckets don't get the count suffix.
    const hfCatalog = buildProviderCatalog(HERMES_MODELS);
    const hf = hfCatalog.find((p) => p.vendor === "huggingface");
    expect(hf?.label).not.toMatch(/models\)/);
  });
});

describe("findProviderForModel", () => {
  const catalog = buildProviderCatalog(HERMES_MODELS);

  it("matches concrete model ids directly", () => {
    expect(findProviderForModel(catalog, "nousresearch/hermes-4-70b")?.vendor)
      .toBe("nousresearch");
    expect(findProviderForModel(catalog, "openai/gpt-5")?.vendor).toBe("openai");
  });

  it("matches wildcard providers by prefix", () => {
    expect(findProviderForModel(catalog, "huggingface/meta-llama/Meta-Llama-3-70B")?.vendor)
      .toBe("huggingface");
    expect(findProviderForModel(catalog, "openrouter/anthropic/claude-3.5-sonnet")?.vendor)
      .toBe("openrouter");
    expect(findProviderForModel(catalog, "custom/local-vllm")?.vendor).toBe("custom");
  });

  it("returns null on no match", () => {
    expect(findProviderForModel(catalog, "")).toBeNull();
    expect(findProviderForModel(catalog, "unknown-model-xyz")).toBeNull();
  });
});

// -----------------------------------------------------------------------------
// Component behavior
// -----------------------------------------------------------------------------

function setup(overrides?: Partial<{ value: SelectorValue; models: SelectorModel[]; onChange: (v: SelectorValue) => void }>) {
  const onChange = overrides?.onChange ?? vi.fn();
  const value: SelectorValue = overrides?.value ?? { providerId: "", model: "", envVars: [] };
  render(
    <ProviderModelSelector
      models={overrides?.models ?? CLAUDE_CODE_MODELS}
      value={value}
      onChange={onChange}
    />,
  );
  return { onChange };
}

describe("<ProviderModelSelector>", () => {
  it("renders provider dropdown with all vendor options", () => {
    setup();
    const select = screen.getByTestId("provider-select") as HTMLSelectElement;
    const optionTexts = Array.from(select.options).map((o) => o.text);
    expect(optionTexts).toContain("Claude Code subscription (3 models)");
    expect(optionTexts.some((t) => t.startsWith("MiniMax"))).toBe(true);
    expect(optionTexts.some((t) => t.startsWith("Z.ai"))).toBe(true);
  });

  it("model dropdown is disabled until provider is picked", () => {
    setup();
    const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
    expect(modelSelect.disabled).toBe(true);
  });

  it("picking a multi-model provider emits onChange with empty model (forces explicit pick)", () => {
    const { onChange } = setup();
    const providerSelect = screen.getByTestId("provider-select");
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const minimax = catalog.find((p) => p.vendor === "minimax")!;
    // MiniMax bucket holds 2 models (MiniMax-M2 + MiniMax-M2.7). Auto-
    // picking the first one used to bite a real user (2026-05-03):
    // they wanted M2.7 but the silent default put M2 in the deploy
    // payload. Now the model field must come back empty so the next
    // dropdown is required-empty and Save/Deploy stay disabled until
    // the user picks.
    fireEvent.change(providerSelect, { target: { value: minimax.id } });
    expect(onChange).toHaveBeenCalledWith({
      providerId: minimax.id,
      model: "",
      envVars: ["ANTHROPIC_AUTH_TOKEN"],
    });
  });

  it("picking a single-model provider auto-fills the model (no choice to make)", () => {
    const { onChange } = setup();
    const providerSelect = screen.getByTestId("provider-select");
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    // GLM-4.6 is the only model under the zai vendor in the fixture —
    // a "0 vs many" boundary check. With only one option, forcing the
    // user to re-pick adds friction without preventing any error.
    const zai = catalog.find((p) => p.vendor === "zai")!;
    expect(zai.models.length).toBe(1);
    fireEvent.change(providerSelect, { target: { value: zai.id } });
    expect(onChange).toHaveBeenCalledWith({
      providerId: zai.id,
      model: "GLM-4.6",
      envVars: ["ANTHROPIC_AUTH_TOKEN"],
    });
  });

  it("picking provider then model emits combined value", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const minimax = catalog.find((p) => p.vendor === "minimax")!;
    const onChange = vi.fn();
    setup({
      value: { providerId: minimax.id, model: "MiniMax-M2", envVars: ["ANTHROPIC_AUTH_TOKEN"] },
      onChange,
    });
    const modelSelect = screen.getByTestId("model-select");
    fireEvent.change(modelSelect, { target: { value: "MiniMax-M2.7" } });
    expect(onChange).toHaveBeenCalledWith({
      providerId: minimax.id,
      model: "MiniMax-M2.7",
      envVars: ["ANTHROPIC_AUTH_TOKEN"],
    });
  });

  it("wildcard provider switches model UI to free-text input", () => {
    const catalog = buildProviderCatalog(HERMES_MODELS);
    const hf = catalog.find((p) => p.vendor === "huggingface")!;
    setup({
      models: HERMES_MODELS,
      value: { providerId: hf.id, model: "", envVars: hf.envVars },
    });
    expect(screen.queryByTestId("model-select")).toBeNull();
    expect(screen.queryByTestId("model-input")).not.toBeNull();
  });

  it("wildcard input emits typed value as model", () => {
    const catalog = buildProviderCatalog(HERMES_MODELS);
    const openrouter = catalog.find((p) => p.vendor === "openrouter")!;
    const onChange = vi.fn();
    setup({
      models: HERMES_MODELS,
      value: { providerId: openrouter.id, model: "", envVars: openrouter.envVars },
      onChange,
    });
    const input = screen.getByTestId("model-input");
    fireEvent.change(input, { target: { value: "openrouter/anthropic/claude-3.5-sonnet" } });
    expect(onChange).toHaveBeenCalledWith({
      providerId: openrouter.id,
      model: "openrouter/anthropic/claude-3.5-sonnet",
      envVars: ["OPENROUTER_API_KEY"],
    });
  });

  it("renders required env hint for selected provider", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const oauth = catalog.find((p) => p.vendor === "anthropic-oauth")!;
    setup({
      value: { providerId: oauth.id, model: "sonnet", envVars: oauth.envVars },
    });
    expect(screen.getByText(/requires:/).textContent).toMatch(/CLAUDE_CODE_OAUTH_TOKEN/);
  });

  it("switching to a multi-model provider clears the stale model id", () => {
    const catalog = buildProviderCatalog(CLAUDE_CODE_MODELS);
    const oauth = catalog.find((p) => p.vendor === "anthropic-oauth")!;
    const minimax = catalog.find((p) => p.vendor === "minimax")!;
    const onChange = vi.fn();
    setup({
      value: { providerId: oauth.id, model: "sonnet", envVars: oauth.envVars },
      onChange,
    });
    fireEvent.change(screen.getByTestId("provider-select"), { target: { value: minimax.id } });
    // Empty rather than auto-picked — see "picking a multi-model
    // provider …" test above for the user-facing rationale.
    expect(onChange).toHaveBeenCalledWith({
      providerId: minimax.id,
      model: "",
      envVars: ["ANTHROPIC_AUTH_TOKEN"],
    });
  });
});
