// @vitest-environment jsdom
//
// #2248 — Platform-managed providers must NOT inject their auth_env
// (e.g. MOLECULE_LLM_USAGE_TOKEN) into runtime_config.required_env.
// The tenant supplies no key for these; the CP injects the usage
// credential at provision time.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPut = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: vi.fn(),
    put: (path: string, body?: unknown) => apiPut(path, body),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

const storeUpdateNodeData = vi.fn();
const storeRestartWorkspace = vi.fn();
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) =>
      selector({ restartWorkspace: storeRestartWorkspace, updateNodeData: storeUpdateNodeData }),
    {
      getState: () => ({
        restartWorkspace: storeRestartWorkspace,
        updateNodeData: storeUpdateNodeData,
      }),
    },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

beforeEach(() => {
  apiGet.mockReset();
  apiPut.mockReset();
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-test`) {
      return Promise.resolve({ runtime: "claude-code" });
    }
    if (path === `/workspaces/ws-test/model`) {
      return Promise.resolve({ model: "sonnet" });
    }
    if (path === `/workspaces/ws-test/files/config.yaml`) {
      // Start with a BYOK required_env already persisted — simulates a
      // workspace that was previously on the anthropic-oauth provider and
      // saved CLAUDE_CODE_OAUTH_TOKEN into config.yaml.
      return Promise.resolve({ content: "name: test\nruntime: claude-code\nruntime_config:\n  model: sonnet\n  required_env:\n    - CLAUDE_CODE_OAUTH_TOKEN\n" });
    }
    if (path === "/templates") {
      return Promise.resolve([
        {
          id: "claude-code",
          name: "Claude Code",
          runtime: "claude-code",
          registry_backed: true,
          registry_providers: [
            { name: "anthropic-oauth", display_name: "Claude Code subscription", auth_env: ["CLAUDE_CODE_OAUTH_TOKEN"], billing_mode: "byok" },
            { name: "platform", display_name: "Platform", auth_env: ["MOLECULE_LLM_USAGE_TOKEN"], billing_mode: "platform_managed" },
          ],
          registry_models: [
            { id: "sonnet", provider: "anthropic-oauth", billing_mode: "byok" },
            { id: "moonshot/kimi-k2.6", provider: "platform", billing_mode: "platform_managed" },
          ],
        },
      ]);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
});

describe("ConfigTab — platform-managed provider gating (#2248)", () => {
  it("does NOT inject platform-managed auth_env into required_env when selected", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Open the provider selector and pick the platform-managed model.
    const modelSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(modelSelect, { target: { value: "registry|platform" } });

    // Expand Secrets section so we can inspect its content.
    const secretsBtn = screen.getByRole("button", { name: /secrets & api keys/i });
    fireEvent.click(secretsBtn);

    // The platform token should NOT appear in the secrets section.
    await waitFor(() => {
      expect(screen.queryByText("MOLECULE_LLM_USAGE_TOKEN", { exact: true })).toBeNull();
    });
  });

  it("DOES render BYOK provider env vars in required_env when selected", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    const modelSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(modelSelect, { target: { value: "registry|anthropic-oauth" } });

    // Expand Secrets section so we can inspect its content.
    const secretsBtn = screen.getByRole("button", { name: /secrets & api keys/i });
    fireEvent.click(secretsBtn);

    // The BYOK env var should still appear.
    await waitFor(() => {
      expect(screen.getByText("CLAUDE_CODE_OAUTH_TOKEN", { exact: true })).toBeTruthy();
    });
  });

  it("clears stale BYOK required_env when switching to platform-managed", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Workspace starts with BYOK model (sonnet). Expand secrets and confirm
    // the BYOK env var is present.
    const secretsBtn = screen.getByRole("button", { name: /secrets & api keys/i });
    fireEvent.click(secretsBtn);
    await waitFor(() => {
      expect(screen.getByText("CLAUDE_CODE_OAUTH_TOKEN", { exact: true })).toBeTruthy();
    });

    // Switch to platform-managed provider.
    const modelSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(modelSelect, { target: { value: "registry|platform" } });

    // The stale BYOK env var must be removed; the platform token must also
    // NOT appear (platform-managed credentials are injected by CP, not tenant).
    await waitFor(() => {
      expect(screen.queryByText("CLAUDE_CODE_OAUTH_TOKEN", { exact: true })).toBeNull();
      expect(screen.queryByText("MOLECULE_LLM_USAGE_TOKEN", { exact: true })).toBeNull();
    });
  });
});
