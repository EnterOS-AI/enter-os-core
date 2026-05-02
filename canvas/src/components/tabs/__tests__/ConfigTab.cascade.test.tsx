// @vitest-environment jsdom
//
// Regression tests for the ConfigTab Provider→Model→RequiredEnv
// cascade (PR-6, 2026-05-02). These pin the four UX invariants the
// user reported on hongming.moleculesai.app:
//
//   1. Config display matches the workspace's actual state (workspace
//      metadata + override > config.yaml on disk).
//   2. Provider is selected FIRST, then Model is filtered to that
//      provider's claimed model_prefixes/aliases.
//   3. Required Env Var Names is read-only when the template ships a
//      structured provider_registry — the truth lives in the
//      template, not the form.
//   4. Switching providers shows ALL of that provider's models in
//      the dropdown — including opus, which the previous
//      datalist-filter UX would hide if the user had typed "sonnet".

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, within } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPatch = vi.fn();
const apiPut = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: (path: string, body: unknown) => apiPatch(path, body),
    put: (path: string, body: unknown) => apiPut(path, body),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }),
    { getState: () => ({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }) },
  ),
}));

import { ConfigTab } from "../ConfigTab";

// Mirrors the production claude-code-default registry shape so the
// tests fail loudly when the wire-shape contract drifts.
const CLAUDE_REGISTRY = [
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
    base_url: "https://api.xiaomimimo.com/anthropic",
    auth_env: ["ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"],
  },
];

const CLAUDE_MODELS = [
  { id: "sonnet", name: "Claude Sonnet (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "opus", name: "Claude Opus (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "haiku", name: "Claude Haiku (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
  { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "claude-opus-4-7", name: "Claude Opus 4.7 (API)", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "mimo-v2-pro", name: "Xiaomi MiMo V2 Pro", required_env: ["ANTHROPIC_API_KEY"] },
];

function wireClaudeCode(opts: { providerValue?: string; workspaceModel?: string } = {}) {
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-test") {
      return Promise.resolve({ runtime: "claude-code", tier: 2 });
    }
    if (path === "/workspaces/ws-test/model") {
      return Promise.resolve({ model: opts.workspaceModel ?? "" });
    }
    if (path === "/workspaces/ws-test/provider") {
      return Promise.resolve({ provider: opts.providerValue ?? "", source: opts.providerValue ? "workspace_secrets" : "default" });
    }
    if (path === "/workspaces/ws-test/files/config.yaml") {
      return Promise.resolve({
        content: "name: Claude Code Agent\nruntime: claude-code\nruntime_config:\n  model: sonnet\n",
      });
    }
    if (path === "/templates") {
      return Promise.resolve([
        {
          id: "claude-code-default",
          name: "Claude Code Agent",
          runtime: "claude-code",
          models: CLAUDE_MODELS,
          provider_registry: CLAUDE_REGISTRY,
        },
      ]);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
});

describe("ConfigTab — Provider→Model cascade (PR-6)", () => {
  // The headline regression: typing "sonnet" in the old datalist UX
  // hid every other model. With provider→model cascade and a proper
  // <select>, picking anthropic-oauth must show all three OAuth
  // aliases — including opus.
  it("opus appears in the model dropdown when anthropic-oauth is selected", async () => {
    wireClaudeCode();
    render(<ConfigTab workspaceId="ws-test" />);

    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    expect(providerSelect.tagName).toBe("SELECT");

    fireEvent.change(providerSelect, { target: { value: "anthropic-oauth" } });

    await waitFor(() => {
      const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
      expect(modelSelect.tagName).toBe("SELECT");
      const optionValues = Array.from(modelSelect.querySelectorAll("option")).map((o) => o.value);
      expect(optionValues).toEqual(["sonnet", "opus", "haiku"]);
    });
  });

  // Switching from oauth to api must filter the model dropdown to
  // claude-* prefix matches and snap the selected model to a valid
  // one (because "sonnet" doesn't belong to anthropic-api).
  it("switching providers filters the model dropdown and snaps the selection", async () => {
    wireClaudeCode({ workspaceModel: "sonnet" });
    render(<ConfigTab workspaceId="ws-test" />);

    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    fireEvent.change(providerSelect, { target: { value: "anthropic-api" } });

    await waitFor(() => {
      const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
      const optionValues = Array.from(modelSelect.querySelectorAll("option")).map((o) => o.value);
      expect(optionValues).toEqual(["claude-sonnet-4-6", "claude-opus-4-7"]);
      // Snapped to the first valid model rather than holding the now-
      // invalid `sonnet` value.
      expect(modelSelect.value).toBe("claude-sonnet-4-6");
    });
  });

  // Required Env Var Names: read-only display sourced from the union
  // of provider.auth_env + model.required_env. The free-text TagList
  // editor must not appear when the registry can supply the answer.
  it("Required Env Var Names is read-only and shows the union of provider.auth_env + model.required_env", async () => {
    wireClaudeCode();
    render(<ConfigTab workspaceId="ws-test" />);

    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    fireEvent.change(providerSelect, { target: { value: "anthropic-api" } });

    await waitFor(() => {
      const display = screen.getByTestId("required-env-display");
      const tags = within(display).getAllByText(/[A-Z_]+/).map((el) => el.textContent);
      // Provider auth_env first (any-of: KEY OR TOKEN), then
      // model.required_env (must-have). De-duped — the model only
      // requires ANTHROPIC_API_KEY which is already in auth_env.
      expect(tags).toEqual(["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]);
    });
  });

  // Provider auto-derive: when no override is set, the form must
  // surface what the adapter would resolve at boot. The auto-match
  // shows in the label, and the required-env display reflects that
  // provider's auth_env even though provider is "".
  it("auto-derives provider hints from the saved model when no override is set", async () => {
    wireClaudeCode({ workspaceModel: "claude-opus-4-7" });
    render(<ConfigTab workspaceId="ws-test" />);

    // The provider <select> shows AUTO_PROVIDER as the value.
    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    await waitFor(() => expect(providerSelect.value).toBe("(auto)"));

    // Auto-match label includes the resolved provider name.
    expect(screen.getByText(/auto: anthropic-api/i)).toBeTruthy();

    // Required-env display reflects anthropic-api's auth_env even
    // though no explicit override is saved.
    await waitFor(() => {
      const display = screen.getByTestId("required-env-display");
      const tags = within(display).getAllByText(/[A-Z_]+/).map((el) => el.textContent);
      expect(tags).toEqual(["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]);
    });
  });

  // The form's display must reflect WORKSPACE METADATA + override,
  // not stale config.yaml. Run with workspaceModel="opus" and
  // config.yaml.runtime_config.model="sonnet" — the model select
  // must show "opus" (workspace state), not "sonnet" (yaml).
  it("displays the workspace's actual model state, not the config.yaml value", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test") return Promise.resolve({ runtime: "claude-code", tier: 2 });
      // Workspace endpoint says opus.
      if (path === "/workspaces/ws-test/model") return Promise.resolve({ model: "opus" });
      if (path === "/workspaces/ws-test/provider") return Promise.resolve({ provider: "" });
      // config.yaml on disk says sonnet — should LOSE.
      if (path === "/workspaces/ws-test/files/config.yaml") {
        return Promise.resolve({ content: "name: x\nruntime: claude-code\nruntime_config:\n  model: sonnet\n" });
      }
      if (path === "/templates") {
        return Promise.resolve([
          {
            id: "claude-code-default",
            runtime: "claude-code",
            models: CLAUDE_MODELS,
            provider_registry: CLAUDE_REGISTRY,
          },
        ]);
      }
      return Promise.reject(new Error(`unmocked: ${path}`));
    });

    render(<ConfigTab workspaceId="ws-test" />);

    await waitFor(() => {
      const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
      // Workspace metadata wins.
      expect(modelSelect.value).toBe("opus");
    });
  });

  // Saving a provider+model pair must PUT to both endpoints with the
  // correct shape. The provider PUT triggers the auto-restart server-
  // side; the model PUT validates against the runtime template.
  it("PUTs provider and model to their respective endpoints on Save", async () => {
    wireClaudeCode();
    apiPut.mockResolvedValue({ status: "saved" });
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);

    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    fireEvent.change(providerSelect, { target: { value: "xiaomi-mimo" } });

    await waitFor(() => {
      const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
      expect(modelSelect.value).toBe("mimo-v2-pro");
    });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      const providerCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/provider");
      expect(providerCalls.length).toBe(1);
      expect(providerCalls[0][1]).toEqual({ provider: "xiaomi-mimo" });
    });
    await waitFor(() => {
      const modelCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/model");
      expect(modelCalls.length).toBe(1);
      expect(modelCalls[0][1]).toEqual({ model: "mimo-v2-pro" });
    });
  });

  // Picking AUTO_PROVIDER (the dropdown default) must save "" — that
  // clears any previous override and lets the adapter derive at boot.
  it("AUTO_PROVIDER selection saves the empty-string override", async () => {
    wireClaudeCode({ providerValue: "xiaomi-mimo" });
    apiPut.mockResolvedValue({ status: "cleared" });

    render(<ConfigTab workspaceId="ws-test" />);
    const providerSelect = await screen.findByTestId("provider-input") as HTMLSelectElement;
    await waitFor(() => expect(providerSelect.value).toBe("xiaomi-mimo"));

    fireEvent.change(providerSelect, { target: { value: "(auto)" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => {
      const providerCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/provider");
      expect(providerCalls.length).toBe(1);
      expect(providerCalls[0][1]).toEqual({ provider: "" });
    });
  });

  // Legacy template (no provider_registry) must keep the editable
  // TagList for required env names — otherwise users on older
  // templates lose the ability to set anything.
  it("legacy templates without provider_registry keep the editable required-env TagList", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test") return Promise.resolve({ runtime: "hermes", tier: 2 });
      if (path === "/workspaces/ws-test/model") return Promise.resolve({ model: "" });
      if (path === "/workspaces/ws-test/provider") return Promise.resolve({ provider: "" });
      if (path === "/workspaces/ws-test/files/config.yaml") {
        return Promise.resolve({
          content: "name: x\nruntime: hermes\nruntime_config:\n  required_env:\n    - LEGACY_KEY\n",
        });
      }
      if (path === "/templates") {
        // Old hermes shape: flat providers list, no provider_registry.
        return Promise.resolve([
          { id: "hermes", runtime: "hermes", models: [], providers: ["nous"] },
        ]);
      }
      return Promise.reject(new Error(`unmocked: ${path}`));
    });

    render(<ConfigTab workspaceId="ws-test" />);

    await waitFor(() => {
      // The free-text TagList editor's placeholder should still appear.
      expect(screen.getByPlaceholderText(/variable NAME/i)).toBeTruthy();
      // No structured read-only display.
      expect(screen.queryByTestId("required-env-display")).toBeNull();
    });
  });

  // Legacy template with NO models[] surfaces the model field as a
  // free-text <input> so a power user isn't blocked. Setting a value
  // and saving must still PUT /workspaces/:id/model with the typed
  // string, even though there's no select to pick from.
  it("falls back to a free-text model input when the runtime ships no models", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test") return Promise.resolve({ runtime: "hermes", tier: 2 });
      if (path === "/workspaces/ws-test/model") return Promise.resolve({ model: "" });
      if (path === "/workspaces/ws-test/provider") return Promise.resolve({ provider: "" });
      if (path === "/workspaces/ws-test/files/config.yaml") {
        return Promise.resolve({ content: "name: x\nruntime: hermes\n" });
      }
      if (path === "/templates") {
        return Promise.resolve([{ id: "hermes", runtime: "hermes", models: [] }]);
      }
      return Promise.reject(new Error(`unmocked: ${path}`));
    });
    apiPut.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);
    const modelInput = await screen.findByTestId("model-select") as HTMLInputElement;
    expect(modelInput.tagName).toBe("INPUT");

    fireEvent.change(modelInput, { target: { value: "custom/some-other-model" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      const modelCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/model");
      expect(modelCalls.length).toBe(1);
      expect(modelCalls[0][1]).toEqual({ model: "custom/some-other-model" });
    });
  });

  // Defensive: a saved model that doesn't belong to the currently-
  // selected provider's claim list must remain visible as a
  // "stranded" option so the user sees what's actually saved. Without
  // this, the select would silently snap to the first filtered
  // option and the user would lose the configuration without
  // confirmation.
  it("preserves a stranded saved model as a labeled fallback option", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test") return Promise.resolve({ runtime: "claude-code", tier: 2 });
      if (path === "/workspaces/ws-test/model") return Promise.resolve({ model: "some-strange-model-id" });
      if (path === "/workspaces/ws-test/provider") return Promise.resolve({ provider: "anthropic-oauth" });
      if (path === "/workspaces/ws-test/files/config.yaml") {
        return Promise.resolve({ content: "runtime: claude-code\n" });
      }
      if (path === "/templates") {
        return Promise.resolve([
          {
            id: "claude-code-default",
            runtime: "claude-code",
            models: CLAUDE_MODELS,
            provider_registry: CLAUDE_REGISTRY,
          },
        ]);
      }
      return Promise.reject(new Error(`unmocked: ${path}`));
    });

    render(<ConfigTab workspaceId="ws-test" />);

    await waitFor(() => {
      const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
      expect(modelSelect.value).toBe("some-strange-model-id");
      const text = Array.from(modelSelect.querySelectorAll("option")).map((o) => o.textContent || "").join(" ");
      expect(text).toContain("not in this provider");
    });
  });
});
