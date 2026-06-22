// @vitest-environment jsdom
//
// internal#718 P3 (retire-list #4) — when GET /templates serves a
// registry-backed selectable list (registry_providers + registry_models with
// display_name / billing_mode / derived provider), the canvas builds the
// provider catalog FROM that registry data instead of re-inferring vendor
// from model-id prefixes (VENDOR_LABELS / BARE_VENDOR_PATTERNS / inferVendor).
// The heuristic path stays only as the fallback for non-registry runtimes /
// older backends.

import { describe, it, expect } from "vitest";
import {
  buildProviderCatalogFromRegistry,
  type RegistryProvider,
  type RegistryModel,
} from "../ProviderModelSelector";

// Mirrors the registry-served claude-code payload from GET /templates
// (registry_providers / registry_models). display_name + billing_mode come
// from the registry, NOT from the canvas VENDOR_LABELS map.
const CLAUDE_CODE_REGISTRY_PROVIDERS: RegistryProvider[] = [
  {
    name: "anthropic-oauth",
    display_name: "Claude Code subscription",
    auth_env: ["CLAUDE_CODE_OAUTH_TOKEN"],
    billing_mode: "byok",
  },
  {
    name: "anthropic-api",
    display_name: "Anthropic API",
    auth_env: ["ANTHROPIC_API_KEY"],
    billing_mode: "byok",
  },
  {
    name: "platform",
    display_name: "Platform",
    auth_env: ["ANTHROPIC_API_KEY", "MOLECULE_LLM_USAGE_TOKEN"],
    billing_mode: "platform_managed",
  },
];

const CLAUDE_CODE_REGISTRY_MODELS: RegistryModel[] = [
  { id: "sonnet", provider: "anthropic-oauth", billing_mode: "byok" },
  { id: "opus", provider: "anthropic-oauth", billing_mode: "byok" },
  { id: "claude-opus-4-7", provider: "anthropic-api", billing_mode: "byok" },
  { id: "anthropic/claude-opus-4-7", provider: "platform", billing_mode: "platform_managed" },
];

describe("buildProviderCatalogFromRegistry", () => {
  it("buckets models by their DERIVED registry provider, not by inferred vendor", () => {
    const catalog = buildProviderCatalogFromRegistry(
      CLAUDE_CODE_REGISTRY_PROVIDERS,
      CLAUDE_CODE_REGISTRY_MODELS,
    );

    const byVendor = new Map(catalog.map((p) => [p.vendor, p]));
    // anthropic-oauth bucket holds the two OAuth-derived models.
    const oauth = byVendor.get("anthropic-oauth");
    expect(oauth).toBeDefined();
    expect(oauth!.models.map((m) => m.id).sort()).toEqual(["opus", "sonnet"]);
    // platform bucket holds the platform-namespaced model.
    const platform = byVendor.get("platform");
    expect(platform).toBeDefined();
    expect(platform!.models.map((m) => m.id)).toEqual(["anthropic/claude-opus-4-7"]);
  });

  it("labels providers from the registry display_name, not VENDOR_LABELS", () => {
    const catalog = buildProviderCatalogFromRegistry(
      CLAUDE_CODE_REGISTRY_PROVIDERS,
      CLAUDE_CODE_REGISTRY_MODELS,
    );
    const oauth = catalog.find((p) => p.vendor === "anthropic-oauth");
    // Registry display_name "Claude Code subscription" (decorated with the
    // model count by the catalog builder is acceptable; assert it carries the
    // registry label, not an inferred one).
    expect(oauth!.label).toContain("Claude Code subscription");
  });

  it("carries the registry billing_mode per provider", () => {
    const catalog = buildProviderCatalogFromRegistry(
      CLAUDE_CODE_REGISTRY_PROVIDERS,
      CLAUDE_CODE_REGISTRY_MODELS,
    );
    expect(catalog.find((p) => p.vendor === "anthropic-oauth")!.billingMode).toBe("byok");
    expect(catalog.find((p) => p.vendor === "platform")!.billingMode).toBe("platform_managed");
  });

  it("surfaces the registry auth_env on the provider entry", () => {
    const catalog = buildProviderCatalogFromRegistry(
      CLAUDE_CODE_REGISTRY_PROVIDERS,
      CLAUDE_CODE_REGISTRY_MODELS,
    );
    expect(catalog.find((p) => p.vendor === "anthropic-oauth")!.envVars).toEqual([
      "CLAUDE_CODE_OAUTH_TOKEN",
    ]);
  });

  it("only includes providers that actually have at least one served model", () => {
    // anthropic-api is a registry provider but has no model in this slice →
    // it should not appear as an empty bucket.
    const models: RegistryModel[] = [
      { id: "sonnet", provider: "anthropic-oauth", billing_mode: "byok" },
    ];
    const catalog = buildProviderCatalogFromRegistry(
      CLAUDE_CODE_REGISTRY_PROVIDERS,
      models,
    );
    expect(catalog.map((p) => p.vendor)).toEqual(["anthropic-oauth"]);
  });
});
