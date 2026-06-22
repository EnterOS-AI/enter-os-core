// @vitest-environment jsdom
/**
 * Platform-managed provider gating in the deploy modal (#2248).
 *
 * Platform-managed providers (CP LLM proxy, e.g. moonshot/kimi-k2.6) do
 * NOT require a tenant-supplied API key — Molecule injects its own usage
 * credential (MOLECULE_LLM_USAGE_TOKEN). The modal must:
 *   - NOT render credential input fields for these providers
 *   - Treat the provider as already satisfied (Deploy button enabled)
 *   - Show an explanatory message instead of key inputs
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

import { MissingKeysModal } from "../MissingKeysModal";
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

// Fixture: one BYOK provider + one platform-managed provider.
const MIXED_PROVIDERS: ProviderChoice[] = [
  {
    id: "ANTHROPIC_API_KEY",
    label: "Anthropic (8 models)",
    envVars: ["ANTHROPIC_API_KEY"],
  },
  {
    id: "MOLECULE_LLM_USAGE_TOKEN",
    label: "Platform (managed)",
    envVars: ["MOLECULE_LLM_USAGE_TOKEN"],
  },
];

const MIXED_MODELS: ModelSpec[] = [
  { id: "claude-sonnet-4-6", required_env: ["ANTHROPIC_API_KEY"] },
  { id: "moonshot/kimi-k2.6", provider: "platform", required_env: ["MOLECULE_LLM_USAGE_TOKEN"] },
];

/** Catalog id for a vendor — tests shouldn't hard-code `${vendor}|${env}` ids. */
function providerIdForVendor(vendor: string): string {
  const catalog = buildProviderCatalog(MIXED_MODELS);
  const entry = catalog.find((p) => p.vendor === vendor);
  if (!entry) throw new Error(`vendor "${vendor}" not in catalog`);
  return entry.id;
}

describe("ProviderPickerModal — platform-managed gating (#2248)", () => {
  afterEach(() => cleanup());

  it("shows credential input when a BYOK provider is selected", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={MIXED_PROVIDERS}
        runtime="claude-code"
        models={MIXED_MODELS}
        initialModel="claude-sonnet-4-6"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // One password input for the Anthropic key.
    expect(screen.getAllByPlaceholderText("sk-...")).toHaveLength(1);
    // Deploy is disabled until key is saved.
    const deployBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Deploy" || b.textContent?.trim() === "Add Key",
    );
    expect(deployBtn).toBeTruthy();
    expect(deployBtn!.disabled).toBe(true);
  });

  it("hides credential inputs and enables Deploy when platform-managed is selected", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={MIXED_PROVIDERS}
        runtime="claude-code"
        models={MIXED_MODELS}
        initialModel="moonshot/kimi-k2.6"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Selector snapped to platform-managed provider.
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    expect(providerSelect.value).toBe(providerIdForVendor("platform"));

    // No credential inputs rendered.
    expect(screen.queryAllByPlaceholderText("sk-...")).toHaveLength(0);

    // Platform-managed message visible.
    expect(screen.getByText(/Platform-managed — no API key required/i)).toBeTruthy();

    // Deploy button is immediately enabled.
    const deployBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Deploy",
    );
    expect(deployBtn).toBeTruthy();
    expect(deployBtn!.disabled).toBe(false);
  });

  it("switches from credential inputs to platform-managed message when provider changes", () => {
    render(
      <MissingKeysModal
        open
        missingKeys={["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"]}
        providers={MIXED_PROVIDERS}
        runtime="claude-code"
        models={MIXED_MODELS}
        initialModel="claude-sonnet-4-6"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Starts on Anthropic — credential input visible.
    expect(screen.getAllByPlaceholderText("sk-...")).toHaveLength(1);

    // Switch to platform-managed.
    const providerSelect = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(providerSelect, {
      target: { value: providerIdForVendor("platform") },
    });

    // Credential inputs gone, message shown.
    expect(screen.queryAllByPlaceholderText("sk-...")).toHaveLength(0);
    expect(screen.getByText(/Platform-managed — no API key required/i)).toBeTruthy();

    // Deploy enabled.
    const deployBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Deploy",
    );
    expect(deployBtn).toBeTruthy();
    expect(deployBtn!.disabled).toBe(false);
  });
});
