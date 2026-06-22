// @vitest-environment jsdom
//
// Regression tests for #2248 — platform-managed provider credential suppression
// in ConfigTab.
//
// Covers:
//  - required_env is cleared to [] when switching to a platform-managed provider
//    whose only declared env var is MOLECULE_LLM_USAGE_TOKEN (single-token case).
//  - required_env preserves non-internal tokens for BYOK providers.

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
    (selector: (s: unknown) => unknown) =>
      selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }),
    { getState: () => ({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }) },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

function wireApi(opts: {
  workspaceRuntime?: string;
  workspaceModel?: string;
  configYamlContent?: string | null;
  templates?: Array<{
    id: string;
    name?: string;
    runtime?: string;
    models?: unknown[];
    registry_backed?: boolean;
    registry_providers?: unknown[];
    registry_models?: unknown[];
  }>;
}) {
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-test`) {
      return Promise.resolve({ runtime: opts.workspaceRuntime ?? "" });
    }
    if (path === `/workspaces/ws-test/model`) {
      return Promise.resolve({ model: opts.workspaceModel ?? "" });
    }
    if (path === `/workspaces/ws-test/files/config.yaml`) {
      if (opts.configYamlContent === null) {
        return Promise.reject(new Error("not found"));
      }
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

describe("ConfigTab — platform-managed credential suppression (#2248)", () => {
  it("clears required_env to [] when switching to a single-token platform-managed provider", async () => {
    // Setup: workspace currently has a BYOK provider selected with both keys.
    // The user switches to a platform-managed provider whose ONLY auth_env
    // is MOLECULE_LLM_USAGE_TOKEN. After filtering, envVars becomes [];
    // wasTemplateDriven must still overwrite required_env with [] so the
    // old MOLECULE_LLM_USAGE_TOKEN requirement does not linger.
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "byok-sonnet",
      configYamlContent: [
        "runtime: claude-code",
        "runtime_config:",
        "  model: byok-sonnet",
        "  required_env:",
        "    - ANTHROPIC_API_KEY",
        "    - MOLECULE_LLM_USAGE_TOKEN",
      ].join("\n"),
      templates: [
        {
          id: "t-claude-code",
          name: "Claude Code",
          runtime: "claude-code",
          models: [],
          registry_backed: true,
          registry_providers: [
            {
              name: "anthropic",
              display_name: "BYOK Anthropic",
              auth_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
              billing_mode: "byok",
            },
            {
              name: "platform",
              display_name: "Platform Anthropic",
              auth_env: ["MOLECULE_LLM_USAGE_TOKEN"],
              billing_mode: "platform_managed",
            },
          ],
          registry_models: [
            { id: "byok-sonnet", provider: "anthropic", billing_mode: "byok", required_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"] },
            { id: "platform-sonnet", provider: "platform", billing_mode: "platform_managed", required_env: ["MOLECULE_LLM_USAGE_TOKEN"] },
          ],
        },
      ],
    });

    apiPut.mockResolvedValue({});
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);

    // Wait for the provider dropdown to populate.
    const providerSelect = (await waitFor(() =>
      screen.getByTestId("provider-select"),
    )) as HTMLSelectElement;

    // Switch from BYOK to platform-managed provider.
    const platformOption = Array.from(providerSelect.options).find((o) =>
      o.text.includes("Platform"),
    );
    expect(platformOption).toBeTruthy();
    fireEvent.change(providerSelect, { target: { value: platformOption!.value } });

    // Save & Restart.
    fireEvent.click(screen.getByRole("button", { name: /save & restart/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/workspaces/ws-test/files/config.yaml",
        expect.objectContaining({
          content: expect.not.stringContaining("ANTHROPIC_API_KEY"),
        }),
      );
    });

    // Verify the specific put call no longer carries the suppressed token.
    const putCall = apiPut.mock.calls.find(
      ([path]) => path === "/workspaces/ws-test/files/config.yaml",
    );
    expect(putCall?.[1].content).not.toContain("MOLECULE_LLM_USAGE_TOKEN");
  });

  it("preserves non-internal tokens for BYOK providers", async () => {
    wireApi({
      workspaceRuntime: "claude-code",
      workspaceModel: "byok-sonnet",
      configYamlContent: [
        "runtime: claude-code",
        "runtime_config:",
        "  model: byok-sonnet",
        "  required_env:",
        "    - ANTHROPIC_API_KEY",
        "    - MOLECULE_LLM_USAGE_TOKEN",
      ].join("\n"),
      templates: [
        {
          id: "t-claude-code",
          name: "Claude Code",
          runtime: "claude-code",
          models: [],
          registry_backed: true,
          registry_providers: [
            {
              name: "anthropic",
              display_name: "BYOK Anthropic",
              auth_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
              billing_mode: "byok",
            },
          ],
          registry_models: [
            { id: "byok-sonnet", provider: "anthropic", billing_mode: "byok" },
          ],
        },
      ],
    });

    apiPut.mockResolvedValue({});
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-test" />);

    // Wait for load.
    await waitFor(() =>
      screen.getByRole("button", { name: /save & restart/i }),
    );

    // Click Save without changing provider — BYOK should keep both keys.
    fireEvent.click(screen.getByRole("button", { name: /save & restart/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/workspaces/ws-test/files/config.yaml",
        expect.objectContaining({
          content: expect.stringContaining("required_env:"),
        }),
      );
    });

    const putCall = apiPut.mock.calls.find(
      ([path]) => path === "/workspaces/ws-test/files/config.yaml",
    );
    expect(putCall?.[1].content).toContain("ANTHROPIC_API_KEY");
    expect(putCall?.[1].content).toContain("MOLECULE_LLM_USAGE_TOKEN");
  });
});
