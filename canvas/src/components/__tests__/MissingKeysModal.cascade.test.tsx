// @vitest-environment jsdom
/**
 * Provider→model cascade in the deploy modal (sibling of the ConfigTab
 * cascade fix shipped in PR #2516, task #236).
 *
 * The user-reported bug (2026-05-02 hongming Hermes Agent):
 *
 *   1. User opens TemplatePalette → Deploy on a hermes template.
 *   2. Modal shows MODEL field pre-filled with template default
 *      (e.g. "MiniMax-M2.7-highspeed") AND a list of provider radios
 *      (Anthropic, OpenRouter, MiniMax, …).
 *   3. The provider radio defaults to whichever entry was first in
 *      `preflight.providers` (Anthropic in the hermes case).
 *   4. The env-var input below shows ANTHROPIC_API_KEY.
 *   5. User pastes whatever key they have, clicks Deploy.
 *   6. Workspace is created with model=MiniMax-M2.7-highspeed +
 *      ANTHROPIC_API_KEY → hermes adapter tries to call Anthropic
 *      with a MiniMax model id → crashes before /registry/register
 *      → workspace ends in WORKSPACE_PROVISION_FAILED with
 *      "container started but never called /registry/register".
 *
 * Fix: when the model resolves to a known provider via its
 * `required_env`, snap the radio so the env-var fields below match
 * the model the user picked. Free-text models not in `models` (or
 * models without required_env) leave the radio alone — the user can
 * still manually pick a provider.
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

import { MissingKeysModal, providerIdForModel } from "../MissingKeysModal";
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

describe("providerIdForModel", () => {
  it("returns the provider id (sorted+joined required_env) for a known model", () => {
    expect(providerIdForModel("MiniMax-M2.7-highspeed", HERMES_MODELS)).toBe(
      "MINIMAX_API_KEY",
    );
    expect(providerIdForModel("claude-opus-4-7", HERMES_MODELS)).toBe(
      "ANTHROPIC_API_KEY",
    );
  });

  // The id formula sorts envVars before joining. A model that needs
  // two keys together (rare today, but the shape supports it) maps
  // to a deterministic id regardless of the order in required_env.
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

describe("ProviderPickerModal — model→provider cascade", () => {
  afterEach(() => cleanup());

  // The headline bug: opening the modal with the MiniMax default
  // pre-filled should NOT leave the radio on Anthropic just because
  // Anthropic was first in providers[]. The cascade snaps the radio
  // to MINIMAX_API_KEY on first paint.
  it("snaps provider radio to MiniMax when initialModel is a MiniMax model", () => {
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
    const minimaxRadio = screen.getByRole("radio", {
      name: /MiniMax \(2 models\)/i,
    }) as HTMLInputElement;
    expect(minimaxRadio.checked).toBe(true);
    // The env-var input underneath should be for MINIMAX_API_KEY,
    // not ANTHROPIC_API_KEY — that's the load-bearing UX win. The
    // entry uses a password input with a fixed "sk-..." placeholder
    // when the key name contains "API_KEY"; assert exactly ONE such
    // input exists, which proves only the selected provider's envVars
    // were rendered into entries[]. (The provider-radio subtitles
    // also mention each envVar name as Mono text — that's why we
    // can't use getByText("MINIMAX_API_KEY") here, it would match
    // both the radio label and the entry label.)
    const apiKeyInputs = screen.getAllByPlaceholderText("sk-...");
    expect(apiKeyInputs).toHaveLength(1);
  });

  // Mid-flow change: user starts with the pre-filled MiniMax model,
  // edits it to a Claude model, the radio re-snaps to Anthropic. This
  // matches user expectation — picking a different model shouldn't
  // leave the wrong env-var input showing.
  it("re-snaps when the user edits the model field to a different provider's model", () => {
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
    const modelInput = screen.getByLabelText(/Model slug/i) as HTMLInputElement;
    fireEvent.change(modelInput, { target: { value: "claude-opus-4-7" } });
    const anthropicRadio = screen.getByRole("radio", {
      name: /Anthropic \(8 models\)/i,
    }) as HTMLInputElement;
    expect(anthropicRadio.checked).toBe(true);
    // Same shape-pin as the previous test — exactly one
    // password input means only the selected provider's envVars
    // landed in entries[].
    expect(screen.getAllByPlaceholderText("sk-...")).toHaveLength(1);
  });

  // Free-text models (typed slug not in the registry) should NOT
  // change the radio — the user may know about a model the template
  // doesn't list. Falling back to the previously-selected provider
  // keeps the form in a usable state.
  it("leaves the radio alone when the typed model is not in the registry", () => {
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
    // Snapped to MiniMax by initial cascade.
    expect(
      (screen.getByRole("radio", {
        name: /MiniMax \(2 models\)/i,
      }) as HTMLInputElement).checked,
    ).toBe(true);

    // Type something the registry doesn't know — radio stays on MiniMax.
    const modelInput = screen.getByLabelText(/Model slug/i) as HTMLInputElement;
    fireEvent.change(modelInput, {
      target: { value: "some-future-model-not-in-registry" },
    });
    expect(
      (screen.getByRole("radio", {
        name: /MiniMax \(2 models\)/i,
      }) as HTMLInputElement).checked,
    ).toBe(true);
  });

  // Backwards-compat: callers that don't pass `models` (legacy
  // call sites) keep the pre-cascade behavior — radio defaults to
  // providers[0] (or to a satisfied configuredKeys match). The
  // cascade is purely additive.
  it("falls back to providers[0] when models prop is omitted", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        // models intentionally omitted — legacy caller shape.
        initialModel="MiniMax-M2.7-highspeed"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Without `models`, no cascade: radio sits on providers[0]
    // (Anthropic), reproducing the bug the cascade fixes. Pinned
    // here so anyone removing the `models` prop sees the regression.
    expect(
      (screen.getByRole("radio", {
        name: /Anthropic \(8 models\)/i,
      }) as HTMLInputElement).checked,
    ).toBe(true);
  });

  // configuredKeys interaction: when a provider's keys are already
  // saved globally, the picker pre-selects that satisfied provider.
  // The model cascade should still override — the user explicitly
  // picked a model that needs a different provider, that intent
  // wins over "you already have this key".
  it("model cascade beats configuredKeys-satisfied default", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY"]}
        providers={HERMES_PROVIDERS}
        runtime="hermes"
        // User has Anthropic globally. Without the cascade, radio
        // would snap to Anthropic. WITH the cascade, the typed
        // MiniMax model wins.
        configuredKeys={new Set(["ANTHROPIC_API_KEY"])}
        modelSuggestions={HERMES_MODELS.map((m) => m.id)}
        models={HERMES_MODELS}
        initialModel="MiniMax-M2.7-highspeed"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    expect(
      (screen.getByRole("radio", {
        name: /MiniMax \(2 models\)/i,
      }) as HTMLInputElement).checked,
    ).toBe(true);
  });
});
