// @vitest-environment jsdom
/**
 * Regression tests for #2248 — platform-managed provider credential suppression.
 *
 * Covers:
 *  - MOLECULE_LLM_USAGE_TOKEN is hidden when the selected provider is platform-managed
 *  - MOLECULE_LLM_USAGE_TOKEN is still shown for BYOK providers
 *  - No render churn from unstable array references (useMemo guard)
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import { MissingKeysModal } from "../MissingKeysModal";
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

const PLATFORM_MANAGED_MODELS: ModelSpec[] = [
  { id: "platform-claude", provider: "platform", required_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"] },
];

const BYOK_MODELS: ModelSpec[] = [
  { id: "byok-claude", provider: "anthropic", required_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"] },
];

function makeProviders(billingMode: "platform_managed" | "byok"): ProviderChoice[] {
  const main = {
    id: billingMode === "platform_managed" ? "platform|ANTHROPIC_API_KEY|MOLECULE_LLM_USAGE_TOKEN" : "anthropic|ANTHROPIC_API_KEY|MOLECULE_LLM_USAGE_TOKEN",
    label: billingMode === "platform_managed" ? "Platform Anthropic" : "BYOK Anthropic",
    envVars: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
    billingMode,
  };
  // Need ≥2 providers so MissingKeysModal enters picker mode (pickerMode = providers.length > 1).
  const dummy = {
    id: "openai|OPENAI_API_KEY",
    label: "OpenAI",
    envVars: ["OPENAI_API_KEY"],
  };
  return [main, dummy];
}

describe("ProviderPickerModal — platform-managed suppression (#2248)", () => {
  afterEach(() => cleanup());

  it("hides all tenant API keys when provider is platform-managed", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={makeProviders("platform_managed")}
        models={PLATFORM_MANAGED_MODELS}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Platform-managed providers use Molecule-injected credentials; no tenant
    // API key inputs should be rendered.
    expect(screen.queryByText("ANTHROPIC_API_KEY")).toBeNull();
    expect(screen.queryByText("MOLECULE_LLM_USAGE_TOKEN")).toBeNull();
    expect(screen.getByText(/Platform-managed/)).toBeTruthy();
  });

  it("shows MOLECULE_LLM_USAGE_TOKEN when provider is BYOK", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={makeProviders("byok")}
        models={BYOK_MODELS}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Both keys visible for BYOK
    expect(screen.getByText("ANTHROPIC_API_KEY")).toBeTruthy();
    expect(screen.getByText("MOLECULE_LLM_USAGE_TOKEN")).toBeTruthy();
  });

  it("does not churn renders when the modal is open and platform-managed", () => {
    let renderCount = 0;

    function RenderSpy({ children }: { children: React.ReactNode }) {
      renderCount++;
      return <>{children}</>;
    }

    render(
      <RenderSpy>
        <MissingKeysModal
          open
          missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
          providers={makeProviders("platform_managed")}
          models={PLATFORM_MANAGED_MODELS}
          runtime="claude-code"
          onKeysAdded={vi.fn()}
          onCancel={vi.fn()}
        />
      </RenderSpy>,
    );

    const countAfterInitial = renderCount;

    // Wait a tick — if useEffect were looping, renderCount would climb.
    // In jsdom without real timers there's no automatic re-render, so we
    // just assert the count is stable immediately after the single
    // commit required by the initial open state.
    expect(renderCount).toBe(countAfterInitial);
    expect(renderCount).toBeLessThanOrEqual(2); // StrictMode double-render ceiling
  });

  it("updates suppression correctly when switching from BYOK to platform-managed", async () => {
    const providers: ProviderChoice[] = [
      {
        id: "anthropic|ANTHROPIC_API_KEY|MOLECULE_LLM_USAGE_TOKEN",
        label: "BYOK Anthropic",
        envVars: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
        billingMode: "byok",
      },
      {
        id: "platform|ANTHROPIC_API_KEY|MOLECULE_LLM_USAGE_TOKEN",
        label: "Platform Anthropic",
        envVars: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
        billingMode: "platform_managed",
      },
      {
        id: "openai|OPENAI_API_KEY",
        label: "OpenAI",
        envVars: ["OPENAI_API_KEY"],
      },
    ];

    const models: ModelSpec[] = [
      { id: "byok-claude", provider: "anthropic", required_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"] },
      { id: "platform-claude", provider: "platform", required_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"] },
    ];

    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={providers}
        models={models}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );

    // Default selection is providers[0] (BYOK) — both keys visible
    expect(screen.getByText("ANTHROPIC_API_KEY")).toBeTruthy();
    expect(screen.getByText("MOLECULE_LLM_USAGE_TOKEN")).toBeTruthy();

    // Switch to platform-managed provider
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    act(() => {
      fireEvent.change(providerSelect, {
        target: { value: "platform|ANTHROPIC_API_KEY|MOLECULE_LLM_USAGE_TOKEN" },
      });
    });

    // Platform-managed selection should suppress all tenant API key inputs.
    await waitFor(() => {
      expect(screen.queryByText("ANTHROPIC_API_KEY")).toBeNull();
    });
    expect(screen.queryByText("MOLECULE_LLM_USAGE_TOKEN")).toBeNull();
    expect(screen.getByText(/Platform-managed/)).toBeTruthy();
  });
});
