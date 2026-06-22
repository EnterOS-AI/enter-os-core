// @vitest-environment jsdom
//
// internal#718 P3 (retire-list #5) — the billing-mode the Config tab shows /
// sends must reflect the DERIVED provider per the registry, not the hardcoded
// billingModeForProvider("" | "platform" → platform_managed else byok) rule.
// When the runtime is registry-backed, billingModeForSelectedProvider reads the
// registry-served billing_mode off the provider catalog entry. The hardcoded
// rule remains only as the fallback for non-registry runtimes / older backends.

import { describe, it, expect } from "vitest";
import { billingModeForSelectedProvider, billingModeForProvider } from "../ConfigTab";
import {
  buildProviderCatalogFromRegistry,
  type RegistryProvider,
  type RegistryModel,
} from "../../ProviderModelSelector";

const REGISTRY_PROVIDERS: RegistryProvider[] = [
  { name: "anthropic-oauth", display_name: "Claude Code subscription", auth_env: ["CLAUDE_CODE_OAUTH_TOKEN"], billing_mode: "byok" },
  { name: "platform", display_name: "Platform", auth_env: ["ANTHROPIC_API_KEY"], billing_mode: "platform_managed" },
  // DISCRIMINATING fixture (review #7790): a provider whose registry-served
  // billing_mode DISAGREES with the hardcoded name-based rule. Its name is not
  // "platform"/"" so billingModeForProvider() would call it "byok", yet the
  // registry serves "platform_managed" (the federation-ready shape the SSOT is
  // built for — a managed provider that isn't literally named "platform").
  // billingModeForSelectedProvider MUST return the REGISTRY value here; the
  // only way to get "platform_managed" out is to honor the catalog, so this
  // case fails if the impl ever regresses to the hardcoded rule.
  { name: "managed-federated", display_name: "Managed (federated)", auth_env: [], billing_mode: "platform_managed" },
];
const REGISTRY_MODELS: RegistryModel[] = [
  { id: "sonnet", provider: "anthropic-oauth", billing_mode: "byok" },
  { id: "anthropic/claude-opus-4-7", provider: "platform", billing_mode: "platform_managed" },
  // model bucketed under the disagreeing provider so the catalog builds an
  // entry for it (buildProviderCatalogFromRegistry only emits a provider entry
  // for providers that own at least one model).
  { id: "managed/some-model", provider: "managed-federated", billing_mode: "platform_managed" },
];

describe("billingModeForSelectedProvider (registry-driven)", () => {
  const catalog = buildProviderCatalogFromRegistry(REGISTRY_PROVIDERS, REGISTRY_MODELS);

  it("reads platform_managed from the registry for the platform provider", () => {
    expect(billingModeForSelectedProvider("platform", catalog)).toBe("platform_managed");
  });

  it("reads byok from the registry for a BYOK provider", () => {
    // anthropic-oauth derives to byok via the REGISTRY. (Note: the hardcoded
    // rule would ALSO say byok for this non-'platform' name, so on its own this
    // assertion does NOT prove the registry is authoritative — it agrees either
    // way. The registry-WINS proof is the disagreement case below.)
    expect(billingModeForSelectedProvider("anthropic-oauth", catalog)).toBe("byok");
  });

  it("lets the registry billing_mode WIN when it disagrees with the hardcoded rule", () => {
    // 'managed-federated' is not '' / 'platform', so the legacy name-based rule
    // classifies it byok — but the registry serves platform_managed. The
    // registry is the SSOT, so billingModeForSelectedProvider must return
    // platform_managed. This is the discriminating case: it FAILS if the impl
    // regresses to billingModeForProvider (which would return byok here).
    expect(billingModeForProvider("managed-federated")).toBe("byok"); // sanity: the rules genuinely disagree
    expect(billingModeForSelectedProvider("managed-federated", catalog)).toBe("platform_managed");
  });

  it("falls back to the hardcoded rule when no registry catalog is supplied", () => {
    // Non-registry runtime / older backend → catalog empty/undefined → the
    // legacy mapping still applies ('' | 'platform' → platform_managed).
    expect(billingModeForSelectedProvider("", undefined)).toBe("platform_managed");
    expect(billingModeForSelectedProvider("platform", undefined)).toBe("platform_managed");
    expect(billingModeForSelectedProvider("minimax", undefined)).toBe("byok");
  });

  it("falls back to the hardcoded rule when the provider is not in the registry catalog", () => {
    // A provider string the registry catalog doesn't carry (stale saved
    // value) → fall back to the legacy rule rather than guessing.
    expect(billingModeForSelectedProvider("some-byo-vendor", catalog)).toBe("byok");
  });
});
