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

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }),
    { getState: () => ({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }) },
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
  it("falls back to model-slug prefixes when the runtime ships no providers list", async () => {
    wireApi({
      workspaceRuntime: "hermes",
      workspaceModel: "anthropic:claude-opus-4-7",
      configYamlContent: "name: ws\nruntime: hermes\n",
      providerValue: "",
      templates: [
        {
          id: "hermes",
          name: "Hermes",
          runtime: "hermes",
          models: [
            { id: "anthropic:claude-opus-4-7" },
            { id: "openai:gpt-4o" },
            { id: "anthropic:claude-sonnet-4-5" }, // dup vendor — must dedupe
            { id: "nousresearch/hermes-4-70b" },   // "/" separator
          ],
          // No `providers:` field → fallback derivation kicks in.
        },
      ],
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    const listId = (input as HTMLInputElement).getAttribute("list");
    expect(listId).toBeTruthy();
    await waitFor(() => {
      const datalist = document.getElementById(listId!);
      const optionValues = Array.from(datalist!.querySelectorAll("option")).map(
        (o) => (o as HTMLOptionElement).value,
      );
      // Order = first-appearance from models[]; dedup keeps anthropic
      // once even though two model slugs use it.
      expect(optionValues).toEqual(["anthropic", "openai", "nousresearch"]);
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
});
