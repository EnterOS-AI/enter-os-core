// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { SecretsSection } from "../secrets-section";

// Tests for SecretsSection — locks in the fix that the secret-slot
// list is driven by the workspace's `runtime_config.required_env`
// instead of a hardcoded COMMON_KEYS list.
//
// Before the fix the component always rendered Anthropic / OpenAI /
// Google / SERP / Model Override slots regardless of template. For a
// Hermes workspace that declares MINIMAX_API_KEY that meant the user
// saw five irrelevant slots and no slot for the key they actually
// needed.

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn().mockResolvedValue([]),
    put: vi.fn().mockResolvedValue({}),
    post: vi.fn().mockResolvedValue({}),
    del: vi.fn().mockResolvedValue({}),
    patch: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock("@/lib/canvas-actions", () => ({
  markAllWorkspacesNeedRestart: vi.fn(),
}));

// The Section wrapper is collapsible with `defaultOpen={false}`. For
// tests we want the content visible without a click — replace the
// wrapper with a passthrough that always renders children.
vi.mock("../form-inputs", async () => {
  const actual = await vi.importActual<typeof import("../form-inputs")>("../form-inputs");
  return {
    ...actual,
    Section: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  };
});

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

describe("SecretsSection — template-driven slots", () => {
  it("renders exactly the slots the template declares in required_env", async () => {
    render(
      <SecretsSection workspaceId="ws-1" requiredEnv={["MINIMAX_API_KEY"]} />,
    );
    await waitFor(() => {
      expect(screen.getByText("MINIMAX_API_KEY")).toBeTruthy();
    });
    // Hardcoded slots that were there before this fix must NOT appear
    // when the template doesn't ask for them.
    expect(screen.queryByText("ANTHROPIC_API_KEY")).toBeNull();
    expect(screen.queryByText("OPENAI_API_KEY")).toBeNull();
    expect(screen.queryByText("GOOGLE_API_KEY")).toBeNull();
    expect(screen.queryByText("SERP_API_KEY")).toBeNull();
  });

  it("uses the friendly label from KNOWN_LABELS for a well-known name", async () => {
    render(
      <SecretsSection workspaceId="ws-1" requiredEnv={["ANTHROPIC_API_KEY"]} />,
    );
    await waitFor(() => {
      expect(screen.getByText("Anthropic API Key")).toBeTruthy();
    });
  });

  it("humanises an unknown env var name into a readable label", async () => {
    render(
      <SecretsSection workspaceId="ws-1" requiredEnv={["MINIMAX_API_KEY"]} />,
    );
    await waitFor(() => {
      // "Minimax API Key" — "API" acronym preserved, "Minimax" title-cased.
      expect(screen.getByText("Minimax API Key")).toBeTruthy();
    });
  });

  it("preserves API / URL acronyms when humanising", async () => {
    render(
      <SecretsSection
        workspaceId="ws-1"
        requiredEnv={["ZHIPU_API_KEY", "CUSTOM_MODEL_URL"]}
      />,
    );
    await waitFor(() => {
      expect(screen.getByText("Zhipu API Key")).toBeTruthy();
      expect(screen.getByText("Custom Model URL")).toBeTruthy();
    });
  });

  it("deduplicates repeated entries in required_env", async () => {
    render(
      <SecretsSection
        workspaceId="ws-1"
        requiredEnv={["MINIMAX_API_KEY", "MINIMAX_API_KEY", "OPENAI_API_KEY"]}
      />,
    );
    await waitFor(() => {
      // Only one row for the repeated name.
      const matches = screen.getAllByText("MINIMAX_API_KEY");
      expect(matches).toHaveLength(1);
      expect(screen.getByText("OpenAI API Key")).toBeTruthy();
    });
  });

  it("falls back to the legacy common-keys list when required_env is missing", async () => {
    // Backward compat: old workspaces without a template-set
    // required_env still see Anthropic/OpenAI/Google/SERP slots.
    render(<SecretsSection workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("Anthropic API Key")).toBeTruthy();
    });
    expect(screen.getByText("OpenAI API Key")).toBeTruthy();
    expect(screen.getByText("Google AI API Key")).toBeTruthy();
  });

  it("falls back to the legacy common-keys list when required_env is empty", async () => {
    render(<SecretsSection workspaceId="ws-1" requiredEnv={[]} />);
    await waitFor(() => {
      expect(screen.getByText("Anthropic API Key")).toBeTruthy();
    });
  });

  it("does not fall back when required_env has at least one entry", async () => {
    // Single-entry required_env must NOT spill legacy slots into the UI.
    render(<SecretsSection workspaceId="ws-1" requiredEnv={["MINIMAX_API_KEY"]} />);
    await waitFor(() => {
      expect(screen.getByText("MINIMAX_API_KEY")).toBeTruthy();
    });
    expect(screen.queryByText("Anthropic API Key")).toBeNull();
    expect(screen.queryByText("OpenAI API Key")).toBeNull();
  });
});
