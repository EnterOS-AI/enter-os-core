// @vitest-environment jsdom
/**
 * Provider→model cascade in the deploy modal.
 *
 * Original bug (2026-05-02 hongming Hermes Agent):
 *   1. Modal pre-fills MODEL with template default (e.g. MiniMax-M2.7-highspeed)
 *   2. Provider radio defaults to providers[0] (Anthropic) — wrong vendor
 *   3. ENV-VAR input shows ANTHROPIC_API_KEY
 *   4. User pastes a key, deploys
 *   5. Workspace boots with model=MiniMax + ANTHROPIC_API_KEY → adapter
 *      crashes before /registry/register → WORKSPACE_PROVISION_FAILED.
 *
 * Fix: pre-deploy modal back-derives provider from initialModel and pins
 * the selector to the matching vendor. The dropdown UI (replacing the
 * old radios in PR shipped 2026-05-02) keeps the same invariant.
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

import { MissingKeysModal, providerIdForModel } from "../MissingKeysModal";
import { buildProviderCatalog } from "../ProviderModelSelector";
import type { ModelSpec, ProviderChoice } from "@/lib/deploy-preflight";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), put: vi.fn() },
}));

vi.mock("@/lib/deploy-preflight", async () => {
  const actual = await vi.importActual<typeof import("@/lib/deploy-preflight")>(
    "@/lib/deploy-preflight",
  );
  return actual;
});

// Hermes-shaped fixture: 3 providers, multiple models per provider, one
// "no required_env" local model that should never block a deploy.
const HERMES_PROVIDERS: ProviderChoice[] = [
  {
    id: "ANTHROPIC_API_KEY",
    label: "Anthropic (8 models)",
    envVars: ["ANTHROPIC_API_KEY"],
  },
  {
    id: "MINIMAX_API_KEY",
    label: "MiniMax (2 models)",
    envVars: ["MINIMAX_API_KEY"],
  },
  {
    id: "OPENROUTER_API_KEY",
    label: "OpenRouter (14 models)",
    envVars: ["OPENROUTER_API_KEY"],
  },
];

const HERMES_MODELS: ModelSpec[] = [
  { id: "claude-sonnet-4-6", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "claude-opus-4-7", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "MiniMax-M2.7-highspeed", required_env: ["MINIMAX_API_KEY"] },
  { id: "MiniMax-M2.7", required_env: ["MINIMAX_API_KEY"] },
  { id: "openrouter/anthropic/claude-3.5-sonnet", required_env: ["OPENROUTER_API_KEY"] },
  // Local/self-hosted endpoint — no required_env. Picker should
  // never snap on this one because there's no provider to snap to.
  { id: "local-llama3", required_env: [] },
];

/** Resolve the selector option-value for a given vendor against the
 *  vendor-aware catalog. Catalog ids are `${vendor}|${sortedEnv}`, so
 *  test code shouldn't hard-code them. */
function providerIdForVendor(vendor: string): string {
  const catalog = buildProviderCatalog(HERMES_MODELS);
  const entry = catalog.find((p) => p.vendor === vendor);
  if (!entry) throw new Error(`vendor "${vendor}" not in catalog`);
  return entry.id;
}

describe("providerIdForModel (legacy helper, still exported for tests)", () => {
  it("returns the provider id (sorted+joined required_env) for a known model", () => {
    expect(providerIdForModel("MiniMax-M2.7-highspeed", HERMES_MODELS)).toBe(
      "MINIMAX_API_KEY",
    );
    expect(providerIdForModel("claude-opus-4-7", HERMES_MODELS)).toBe(
      "ANTHROPIC_API_KEY",
    );
  });

  it("sorts required_env so the id matches providersFromTemplate's formula", () => {
    const models: ModelSpec[] = [
      { id: "weird", required_env: ["Z_KEY", "A_KEY"] },
    ];
    expect(providerIdForModel("weird", models)).toBe("A_KEY|Z_KEY");
  });

  it("trims whitespace before lookup so a stray space doesn't miss a match", () => {
    expect(providerIdForModel("  MiniMax-M2.7  ", HERMES_MODELS)).toBe(
      "MINIMAX_API_KEY",
    );
  });

  it("returns null for empty / undefined / whitespace-only model id", () => {
    expect(providerIdForModel("", HERMES_MODELS)).toBeNull();
    expect(providerIdForModel("   ", HERMES_MODELS)).toBeNull();
  });

  it("returns null when models are not provided (free-text mode)", () => {
    expect(providerIdForModel("anything", undefined)).toBeNull();
  });

  it("returns null when model isn't in the registry (free-text)", () => {
    expect(providerIdForModel("not-a-listed-model", HERMES_MODELS)).toBeNull();
  });

  it("returns null when the model has no required_env (local endpoint)", () => {
    expect(providerIdForModel("local-llama3", HERMES_MODELS)).toBeNull();
  });
});

describe("ProviderPickerModal — model→provider cascade (dropdown UI)", () => {
  afterEach(() => cleanup());

  // The headline bug: opening the modal with the MiniMax default
  // pre-filled should NOT leave the selector on Anthropic just because
  // Anthropic was first in providers[]. Back-derivation snaps it on
  // first paint to the MiniMax vendor entry.
  it("snaps provider selector to MiniMax when initialModel is a MiniMax model", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        models={HERMES_MODELS}
        initialModel="MiniMax-M2.7-highspeed"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    expect(providerSelect.value).toBe(providerIdForVendor("minimax"));
    // The env-var input underneath should be for MINIMAX_API_KEY,
    // not ANTHROPIC_API_KEY — that's the load-bearing UX win. The
    // entry uses a password input with a fixed "sk-..." placeholder
    // when the key name contains "API_KEY"; assert exactly ONE such
    // input exists, which proves only the selected provider's envVars
    // were rendered into entries[].
    const apiKeyInputs = screen.getAllByPlaceholderText("sk-...");
    expect(apiKeyInputs).toHaveLength(1);
  });

  // Mid-flow change: user starts with the pre-filled MiniMax model and
  // switches the provider dropdown to Anthropic. Env-var rows below
  // re-render to show ANTHROPIC_API_KEY only. Same shape-pin as above.
  it("re-renders credential entries when provider is switched", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        models={HERMES_MODELS}
        initialModel="MiniMax-M2.7-highspeed"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(providerSelect, {
      target: { value: providerIdForVendor("anthropic") },
    });
    expect(providerSelect.value).toBe(providerIdForVendor("anthropic"));
    // Exactly one password input means only the selected provider's
    // envVars landed in entries[].
    expect(screen.getAllByPlaceholderText("sk-...")).toHaveLength(1);
  });

  // Backwards-compat: callers that don't pass `models` (legacy
  // call sites) fall back to a synthesized catalog from `providers`
  // — selector still works, but vendor split is degraded to env-tuple
  // grouping (one entry per ProviderChoice).
  it("falls back to providers[] when models prop is omitted", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        // models intentionally omitted — legacy caller shape.
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Without `models`, no back-derivation: selector defaults to
    // providers[0] (Anthropic). Dropdown still populated with all 3
    // entries — synthesized catalog uses `${vendor}|${envTuple}` ids
    // (matching the selector's own catalog shape), so the value is
    // "anthropic|ANTHROPIC_API_KEY", not the raw "ANTHROPIC_API_KEY".
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    expect(providerSelect.value).toBe("anthropic|ANTHROPIC_API_KEY");
    expect(providerSelect.options.length).toBeGreaterThanOrEqual(4); // 3 providers + the disabled placeholder
  });

  // configuredKeys interaction: when a provider's keys are already
  // saved globally, the picker pre-selects that satisfied provider.
  // BUT the model-derived snap still wins — the user explicitly
  // picked a model, that intent overrides "you already have this key".
  it("model-derived selection beats configuredKeys-satisfied default", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        // User has Anthropic globally. Without back-derivation,
        // selector would land on Anthropic. WITH it, the typed
        // MiniMax model wins.
        configuredKeys={new Set(["ANTHROPIC_API_KEY"])}
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        models={HERMES_MODELS}
        initialModel="MiniMax-M2.7-highspeed"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    expect(providerSelect.value).toBe(providerIdForVendor("minimax"));
  });
});
