// @vitest-environment jsdom
//
// core#2594: env-resolved workspaces have no stored model/provider (DB MODEL is
// NULL and config.yaml is empty). The Config tab must not show empty required
// dropdowns as if the workspace is broken; it should explain that values are
// derived from the runtime environment and block Save until the user picks a
// persisted model.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: vi.fn(),
    put: vi.fn(),
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
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-env`) {
      return Promise.resolve({ runtime: "claude-code", status: "online" });
    }
    if (path === `/workspaces/ws-env/model`) {
      return Promise.resolve({ model: "", source: "unresolved" });
    }
    if (path === `/workspaces/ws-env/files/config.yaml`) {
      return Promise.resolve({ content: "name: env-resolved\nruntime: claude-code\n" });
    }
    if (path === "/templates") {
      return Promise.resolve([
        {
          id: "claude-code",
          name: "Claude Code",
          runtime: "claude-code",
          models: [
            { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
            { id: "claude-opus-4-7", name: "Claude Opus 4.7", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
          ],
          providers: ["anthropic-oauth"],
        },
      ]);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
});

describe("ConfigTab env-resolved workspace (core#2594)", () => {
  it("renders a 'derived from environment' hint when model source is unresolved", async () => {
    render(<ConfigTab workspaceId="ws-env" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith(`/workspaces/ws-env/model`));
    expect(screen.getByText(/derived from the workspace runtime environment/i)).toBeTruthy();
  });

  it("disables Save buttons while model is unresolved and empty", async () => {
    render(<ConfigTab workspaceId="ws-env" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith(`/workspaces/ws-env/model`));
    const saveBtn = screen.getByRole("button", { name: /Save$/ });
    const saveRestartBtn = screen.getByRole("button", { name: /Save & Restart/i });
    expect(saveBtn.hasAttribute("disabled")).toBe(true);
    expect(saveRestartBtn.hasAttribute("disabled")).toBe(true);
  });

  it("enables Save after the user selects a model", async () => {
    render(<ConfigTab workspaceId="ws-env" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith(`/workspaces/ws-env/model`));

    // Open provider dropdown and pick the only provider.
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(providerSelect, { target: { value: "anthropic|CLAUDE_CODE_OAUTH_TOKEN" } });
    await waitFor(() => expect(providerSelect.value).toBe("anthropic|CLAUDE_CODE_OAUTH_TOKEN"));

    // Open model dropdown and pick a model.
    const modelSelect = screen.getByTestId("model-select") as HTMLSelectElement;
    await waitFor(() => expect(modelSelect.disabled).toBe(false));
    fireEvent.change(modelSelect, { target: { value: "claude-sonnet-4-6" } });
    await waitFor(() => expect(modelSelect.value).toBe("claude-sonnet-4-6"));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Save$/ }).hasAttribute("disabled")).toBe(false);
      expect(screen.getByRole("button", { name: /Save & Restart/i }).hasAttribute("disabled")).toBe(false);
    });
  });
});
