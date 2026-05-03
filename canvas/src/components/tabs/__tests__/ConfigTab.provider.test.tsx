// @vitest-environment jsdom
//
// Regression tests for ConfigTab Provider override (Option B PR-5).
//
// What this pins: a free-text Provider combobox in the Runtime section
// that lets the operator override the model→provider derivation hermes-
// agent does internally. Without this UI, a fresh signup whose Hermes
// workspace defaults to a model with no clean vendor prefix (e.g.
// `nousresearch/hermes-4-70b`) hits the runtime's own preflight error:
//   "No LLM provider configured. Run `hermes model` to select a
//    provider, or run `hermes setup` for first-time configuration."
// — even though tasks #195-198 wired the entire downstream pipe so a
// non-empty provider WOULD flow through canvas → workspace-server →
// CP user-data → workspace config.yaml → hermes adapter.
//
// Hongming Wang hit this on hongming.moleculesai.app at signup
// 2026-05-01T17:35Z. Backend PRs were green, the gap was the missing
// UI to set the value.
//
// Each test pins one invariant. If any fails, the bug is back.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
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

// Shared store stub — `updateNodeData` is exposed so a test can assert the
// node-data flush happens after a successful PATCH (regression: previously
// the DB updated but the canvas badge stayed stale until full hydrate).
const storeUpdateNodeData = vi.fn();
const storeRestartWorkspace = vi.fn();
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace: storeRestartWorkspace, updateNodeData: storeUpdateNodeData }),
    { getState: () => ({ restartWorkspace: storeRestartWorkspace, updateNodeData: storeUpdateNodeData }) },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

// wireApi — same shape as ConfigTab.hermes.test.tsx, extended with the
// /provider endpoint. Each test sets `providerValue` to the value the
// GET endpoint returns; "missing" means the endpoint rejects (older
// workspace-server pre-PR-2 — must not crash the tab).
function wireApi(opts: {
  workspaceRuntime?: string;
  workspaceModel?: string;
  configYamlContent?: string | null;
  templates?: Array<{ id: string; name?: string; runtime?: string; models?: unknown[]; providers?: string[] }>;
  providerValue?: string | "missing";
}) {
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-test`) {
      return Promise.resolve({ runtime: opts.workspaceRuntime ?? "" });
    }
    if (path === `/workspaces/ws-test/model`) {
      return Promise.resolve({ model: opts.workspaceModel ?? "" });
    }
    if (path === `/workspaces/ws-test/provider`) {
      if (opts.providerValue === "missing") {
        return Promise.reject(new Error("404"));
      }
      return Promise.resolve({ provider: opts.providerValue ?? "", source: opts.providerValue ? "workspace_secrets" : "default" });
    }
    if (path === `/workspaces/ws-test/files/config.yaml`) {
      if (opts.configYamlContent === null) return Promise.reject(new Error("not found"));
      return Promise.resolve({ content: opts.configYamlContent ?? "" });
    }
    if (path === "/templates") {
      return Promise.resolve(opts.templates ?? []);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
  storeUpdateNodeData.mockReset();
  storeRestartWorkspace.mockReset();
});

describe("ConfigTab — Provider override (Option B PR-5)", () => {
  // Empty provider on load is the legitimate default ("auto-derive
  // from model slug prefix"), NOT an error. The endpoint returning
  // {provider: "", source: "default"} is the documented happy-path
  // shape — if the form treated that as "load failed" we'd lose the
  // ability to render the input at all on fresh workspaces.
  it("renders an empty Provider input when no override is set", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "",
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    expect((input as HTMLInputElement).value).toBe("");
  });

  // Pre-existing override loads back into the field on mount. Without
  // this, an operator who set provider=openrouter yesterday would see
  // the field blank today, conclude the value didn't stick, and
  // re-save — the resulting PUT-with-same-value would auto-restart
  // the workspace for nothing.
  it("loads an existing provider override from the server", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "openrouter",
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    await waitFor(() => expect((input as HTMLInputElement).value).toBe("openrouter"));
  });

  // Old workspace-server (pre-PR-2) returns a 404 on /provider. The
  // tab must keep loading — the fallback is "" (auto-derive), same as
  // a fresh workspace.
  it("falls back to empty provider when the endpoint is missing", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "missing",
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    expect((input as HTMLInputElement).value).toBe("");
    // Tab should be fully rendered, not stuck in loading or error state.
    expect(screen.queryByText(/Loading config/i)).toBeNull();
  });

  // Setting a value + Save must PUT to the right endpoint with the
  // right body shape. Server-side handler (workspace-server
  // handlers/secrets.go:SetProvider) reads body.provider — any other
  // key gets silently ignored and the workspace_secrets row stays
  // unset. This regression would manifest as "Save → Restart →
  // workspace still says No LLM provider configured."
  it("PUTs the new provider to /workspaces/:id/provider on Save", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "",
    });
    apiPut.mockResolvedValue({ status: "saved", provider: "anthropic" });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");

    fireEvent.change(input, { target: { value: "anthropic" } });
    expect((input as HTMLInputElement).value).toBe("anthropic");

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      const providerCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/provider");
      expect(providerCalls.length).toBe(1);
      expect(providerCalls[0][1]).toEqual({ provider: "anthropic" });
    });
  });

  // No-change Save must NOT PUT /provider. The server-side SetProvider
  // auto-restarts the workspace on every successful PUT — re-writing
  // an unchanged value would cost the user a ~30s reboot every time
  // they tweak some other field.
  it("does not PUT /provider when the value is unchanged", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\ntier: 2\n",
      providerValue: "openrouter",
    });
    apiPut.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);
    await screen.findByTestId("provider-input");

    // Click Save without touching the provider field. Trigger another
    // dirty-marker (tier change) so Save is enabled — the test is
    // about NOT touching /provider, not about Save being disabled.
    const tierSelect = screen.getByLabelText(/tier/i) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      // Some PUT(s) may fire (e.g. /model). Just assert /provider is NOT among them.
      const providerCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/provider");
      expect(providerCalls.length).toBe(0);
    });
  });

  // The dropdown's suggestion list MUST come from the runtime's own
  // template (via /templates → runtime_config.providers), not a
  // hardcoded canvas-side enum. This is the "Native + pluggable
  // runtime" invariant: a new runtime declaring its own provider
  // taxonomy in its config.yaml gets a working dropdown without ANY
  // canvas-side change.
  //
  // Pinned by checking that suggestions surfaced in the datalist
  // exactly mirror what the templates endpoint returned for the
  // matching runtime. If a future contributor reintroduces a
  // PROVIDER_SUGGESTIONS-style hardcoded list and the datalist
  // contents don't follow the template, this test fails.
  it("populates the provider datalist from the matched runtime's templates entry", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "nousresearch/hermes-4-70b",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "",
      templates: [
        {
          id: "hermes",
          name: "Hermes",
          runtime: "hermes",
          models: [],
          // The provider list every runtime adapter ships in its own
          // config.yaml. Canvas must surface THIS, not its own list.
          providers: ["nous", "openrouter", "anthropic", "minimax-cn"],
        },
      ],
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    const listId = (input as HTMLInputElement).getAttribute("list");
    expect(listId).toBeTruthy();
    await waitFor(() => {
      const datalist = document.getElementById(listId!);
      expect(datalist).not.toBeNull();
      const optionValues = Array.from(datalist!.querySelectorAll("option")).map(
        (o) => (o as HTMLOptionElement).value,
      );
      // Order matters — most-common-first is part of the contract so
      // the demo flow lands on a working choice without scrolling.
      expect(optionValues).toEqual(["nous", "openrouter", "anthropic", "minimax-cn"]);
    });
  });

  // Fallback path: when a template hasn't migrated to the explicit
  // `providers:` field yet, suggestions are derived from model slug
  // prefixes. Still adapter-driven (the slugs come from the template's
  // `models:` list), just inferred. This keeps existing templates
  // working while the platform team migrates them one at a time.
  it("renders vendor-grouped provider dropdown when template ships models", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "anthropic/claude-opus-4-7",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "",
      templates: [
        {
          id: "hermes",
          name: "Hermes",
          runtime: "hermes",
          models: [
            { id: "anthropic/claude-opus-4-7", required_env: ["ANTHROPIC_API_KEY"] },
            { id: "openai/gpt-4o", required_env: ["OPENROUTER_API_KEY"] },
            { id: "anthropic/claude-sonnet-4-5", required_env: ["ANTHROPIC_API_KEY"] }, // dup vendor — must dedupe
            { id: "nousresearch/hermes-4-70b", required_env: ["HERMES_API_KEY"] },
          ],
          // No `providers:` field → ProviderModelSelector derives vendors
          // from model id prefixes via its own buildProviderCatalog.
        },
      ],
    });

    render(<ConfigTab workspaceId="ws-test" />);
    // With models present, the new vendor-aware dropdown renders.
    // Provider entries dedupe by vendor → 3 unique vendors here
    // (anthropic, openai, nousresearch).
    const select = await screen.findByTestId("provider-select") as HTMLSelectElement;
    await waitFor(() => {
      const optionTexts = Array.from(select.options)
        .map((o) => o.text)
        .filter((t) => !t.startsWith("—")); // strip placeholder
      // Labels are vendor display names, but vendor identity is what
      // matters for dedupe. Assert each expected vendor surfaces once.
      expect(optionTexts.some((t) => t.startsWith("Anthropic API"))).toBe(true);
      expect(optionTexts.some((t) => t.startsWith("OpenAI"))).toBe(true);
      expect(optionTexts.some((t) => t.startsWith("Nous Research"))).toBe(true);
      expect(optionTexts.length).toBe(3); // dedupe pin
    });
  });

  // Empty string is a legitimate save target — it clears the override
  // (the server-side endpoint deletes the workspace_secrets row).
  // Operators who picked "anthropic" yesterday and want to revert to
  // auto-derive today should be able to do so by clearing the field
  // and clicking Save. Without this PUT path, the only way to clear
  // would be a direct DB edit.
  it("PUTs an empty string when the operator clears a previously-set provider", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "anthropic:claude-opus-4-7",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "openrouter",
    });
    apiPut.mockResolvedValue({ status: "cleared" });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    await waitFor(() => expect((input as HTMLInputElement).value).toBe("openrouter"));

    fireEvent.change(input, { target: { value: "" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      const providerCalls = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/provider");
      expect(providerCalls.length).toBe(1);
      expect(providerCalls[0][1]).toEqual({ provider: "" });
    });
  });

  // Display-vs-storage drift regression (2026-05-03 incident, workspace
  // e13aebd8…). User deployed claude-code with MiniMax-M2 stored in
  // MODEL_PROVIDER. The container env (MODEL=MiniMax-M2) and chat
  // worked correctly, but the Config tab showed "Claude Code
  // subscription / Claude Sonnet (OAuth)" — i.e. the template's
  // runtime_config.model: sonnet default — because currentModelId
  // reads runtime_config.model first and loadConfig was overriding
  // only the top-level config.model field. The merged shape was:
  //   { model: "MiniMax-M2", runtime_config: { model: "sonnet" } }
  // and currentModelId picked "sonnet". Fix: loadConfig propagates
  // wsMetadataModel into BOTH places so the form is a single source
  // of truth (DB-backed MODEL_PROVIDER). Pinning the merged-path
  // branch with the exact reproducing shape: claude-code template
  // YAML has runtime_config.model: sonnet; live workspace's
  // MODEL_PROVIDER is MiniMax-M2; tab must show the latter.
  it("prefers MODEL_PROVIDER over the template's runtime_config.model on load", async () => {
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "MiniMax-M2",
      configYamlContent: "name: ws\nruntime: claude-code\nruntime_config:\n  model: sonnet\n",
      providerValue: "",
      templates: [
        {
          id: "claude-code-default",
          name: "Claude Code",
          runtime: "claude-code",
          models: [
            { id: "sonnet", name: "Claude Sonnet (OAuth)", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
            { id: "MiniMax-M2", name: "MiniMax M2", required_env: ["MINIMAX_API_KEY"] },
            { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
          ],
        },
      ],
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const modelSelect = (await screen.findByTestId("model-select")) as HTMLSelectElement;
    await waitFor(() => expect(modelSelect.value).toBe("MiniMax-M2"));

    // Provider dropdown should also reflect MiniMax (back-derived from
    // the model slug since LLM_PROVIDER is unset). Without the fix,
    // the selector falls back to the first catalog entry whose first
    // model matches "sonnet" → anthropic-oauth bucket → "Claude Code
    // subscription".
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    const selectedOption = providerSelect.options[providerSelect.selectedIndex];
    expect(selectedOption.textContent ?? "").toMatch(/MiniMax/);
  });

  // Sibling pin to the display-fix above. The display fix mirrors
  // wsMetadataModel into runtime_config.model so the selector renders
  // the live value; that mirror means handleSave's old YAML-vs-form
  // diff would always be non-zero on a no-op save (YAML default
  // "sonnet" vs. mirrored "MiniMax-M2") and PUT /model — which
  // server-side SetModel chains into an auto-restart. handleSave now
  // diffs against the loaded MODEL_PROVIDER instead. Pin: an
  // unrelated edit (tier change) must NOT touch /model when the
  // model itself didn't change.
  it("does not PUT /model on a no-op save when only an unrelated field changed", async () => {
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "MiniMax-M2",
      configYamlContent: "name: ws\nruntime: claude-code\ntier: 2\nruntime_config:\n  model: sonnet\n",
      providerValue: "",
      templates: [
        {
          id: "claude-code-default",
          name: "Claude Code",
          runtime: "claude-code",
          models: [
            { id: "sonnet", name: "Claude Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
            { id: "MiniMax-M2", name: "MiniMax M2", required_env: ["MINIMAX_API_KEY"] },
          ],
        },
      ],
    });
    apiPut.mockResolvedValue({});
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);
    const tierSelect = (await screen.findByLabelText(/tier/i)) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      const tierPatches = apiPatch.mock.calls.filter(([path, body]) =>
        path === "/workspaces/ws-test" && (body as { tier?: number }).tier === 3,
      );
      expect(tierPatches.length).toBe(1);
    });
    // Spurious /model PUT would fire here without the originalModel
    // diff baseline. The model itself didn't change, so /model must
    // stay untouched (otherwise SetModel auto-restarts).
    const modelPuts = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/model");
    expect(modelPuts.length).toBe(0);
  });

  // Save-then-stale-badge regression (2026-05-03 incident). User
  // selected T3 in the Tier dropdown, hit Save & Restart, the workspace
  // PATCH succeeded (`tier: 3` in DB), but the canvas header pill kept
  // showing "TIER T2" until a full hydrate. Root cause: handleSave
  // sent the PATCH to workspace-server but never pushed the same
  // change into useCanvasStore.updateNodeData, so every UI surface
  // reading from the store kept its stale value. Pin: a successful
  // tier PATCH must mirror into the store so the badge updates
  // synchronously with the response.
  it("flushes the dbPatch into useCanvasStore.updateNodeData after a successful PATCH", async () => {
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "MiniMax-M2",
      configYamlContent: "name: ws\nruntime: claude-code\ntier: 2\nruntime_config:\n  model: sonnet\n",
      providerValue: "",
      templates: [
        {
          id: "claude-code-default",
          name: "Claude Code",
          runtime: "claude-code",
          models: [{ id: "sonnet", name: "Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] }],
        },
      ],
    });
    apiPatch.mockResolvedValue({ status: "updated" });

    render(<ConfigTab workspaceId="ws-test" />);
    const tierSelect = (await screen.findByLabelText(/tier/i)) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      expect(apiPatch.mock.calls.some(([p]) => p === "/workspaces/ws-test")).toBe(true);
    });
    // Without the store flush, the badge would keep reading tier=2
    // from useCanvasStore.nodes until a full hydrate. Pin: handleSave
    // pushes the same fields it PATCHed.
    expect(storeUpdateNodeData).toHaveBeenCalledWith(
      "ws-test",
      expect.objectContaining({ tier: 3 }),
    );
  });

  // Failure-gating sibling pin to the store-flush test above. The
  // production code places `updateNodeData` AFTER `await api.patch(...)`
  // inside the same `if (Object.keys(dbPatch).length > 0)` block, so a
  // PATCH rejection should throw before the store call. Without this
  // pin, a future refactor that wraps the PATCH in try/catch and
  // unconditionally calls updateNodeData would ship green — and then
  // the badge would lie when the server actually rejected the change.
  // Codified review feedback from PR #2545 (Agent 2).
  it("does NOT flush into useCanvasStore.updateNodeData when the PATCH rejects", async () => {
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "MiniMax-M2",
      configYamlContent: "name: ws\nruntime: claude-code\ntier: 2\nruntime_config:\n  model: sonnet\n",
      providerValue: "",
      templates: [
        {
          id: "claude-code-default",
          name: "Claude Code",
          runtime: "claude-code",
          models: [{ id: "sonnet", name: "Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] }],
        },
      ],
    });
    apiPatch.mockRejectedValue(new Error("500 from workspace-server"));

    render(<ConfigTab workspaceId="ws-test" />);
    const tierSelect = (await screen.findByLabelText(/tier/i)) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    // Wait for handleSave to settle (succeeds-or-fails). PATCH must
    // have been attempted; the error swallow inside handleSave keeps
    // saving=false in finally.
    await waitFor(() => {
      expect(apiPatch.mock.calls.some(([p]) => p === "/workspaces/ws-test")).toBe(true);
    });
    // Critically: the store must NOT have been told about the failed
    // change. Otherwise the badge would lie about a write the server
    // rejected.
    const tierFlushes = storeUpdateNodeData.mock.calls.filter(([, body]) =>
      typeof (body as { tier?: number }).tier === "number",
    );
    expect(tierFlushes.length).toBe(0);
  });

  // Pin the hermes/pre-#240 edge case: workspace where MODEL_PROVIDER
  // was never written but YAML has runtime_config.model: "something".
  // originalModel must reflect the rendered baseline (the YAML value),
  // not the empty MODEL_PROVIDER, so an unrelated save (tier change)
  // doesn't fire a /model PUT and trigger an auto-restart. Codified
  // review feedback from PR #2545 (Agent 1, "Important").
  it("does not PUT /model when MODEL_PROVIDER is empty and the user only edited an unrelated field", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "", // legacy workspace — never went through the picker
      configYamlContent:
        "name: ws\nruntime: hermes\ntier: 2\nruntime_config:\n  model: nousresearch/hermes-4-70b\n",
      providerValue: "",
      templates: [
        {
          id: "hermes",
          name: "Hermes",
          runtime: "hermes",
          models: [{ id: "nousresearch/hermes-4-70b", name: "Hermes 4 70B", required_env: ["HERMES_API_KEY"] }],
          providers: ["nous"],
        },
      ],
    });
    apiPut.mockResolvedValue({});
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);
    const tierSelect = (await screen.findByLabelText(/tier/i)) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    const saveBtn = screen.getByRole("button", { name: /^save$/i });
    fireEvent.click(saveBtn);

    await waitFor(() => {
      expect(apiPatch.mock.calls.some(([p]) => p === "/workspaces/ws-test")).toBe(true);
    });
    const modelPuts = apiPut.mock.calls.filter(([path]) => path === "/workspaces/ws-test/model");
    expect(modelPuts.length).toBe(0);
  });
});
