/** Pure helpers for the ConfigTab provider→model cascade.
 *
 *  Lives in its own module so the cascade logic is unit-testable
 *  without rendering the React tree. The ConfigTab consumes these
 *  with the structured `provider_registry` field surfaced by
 *  workspace-server's /templates handler. Falls back to flat
 *  `providers []string` (legacy hermes shape) and finally to
 *  vendor-prefix derivation when neither is present. */

export interface ModelSpec {
  id: string;
  name?: string;
  required_env?: string[];
}

/** Mirrors providerEntry from workspace-server templates.go. The
 *  yaml-null `base_url` becomes "" on the wire (omitempty drops it,
 *  but JSON omits the key). Both ModelPrefixes and ModelAliases are
 *  treated case-insensitively when matching. */
export interface ProviderEntry {
  name: string;
  auth_mode?: string;
  model_prefixes?: string[];
  model_aliases?: string[];
  base_url?: string;
  auth_env?: string[];
}

/** Sentinel string for "let the runtime pick a provider from the
 *  selected model id." When the operator picks this in the dropdown,
 *  the saved provider override is empty (`""`) and the adapter falls
 *  back to its built-in derive logic. */
export const AUTO_PROVIDER = "(auto)";

/** matchProviderForModel — given a model id and the provider
 *  registry, return the FIRST entry whose model_aliases or
 *  model_prefixes claim that id. Case-insensitive. Returns null when
 *  nothing matches.
 *
 *  Order matters in config.yaml: anthropic-oauth precedes
 *  anthropic-api so `sonnet` → oauth (alias hit) before `claude-X`
 *  prefix kicks in. Preserve that order so the canvas auto-selection
 *  matches what the adapter does at boot. */
export function matchProviderForModel(
  modelId: string,
  registry: readonly ProviderEntry[],
): ProviderEntry | null {
  if (!modelId || !registry || registry.length === 0) return null;
  const lower = modelId.toLowerCase();
  for (const p of registry) {
    if (p.model_aliases?.some((a) => a.toLowerCase() === lower)) return p;
    if (p.model_prefixes?.some((pre) => pre && lower.startsWith(pre.toLowerCase()))) return p;
  }
  return null;
}

/** modelsForProvider — filter the runtime's full models list down to
 *  the ones a given provider claims via its model_aliases or
 *  model_prefixes. Returns the ORIGINAL list when providerName is
 *  AUTO_PROVIDER or the provider isn't in the registry — there's no
 *  filter to apply, the operator gets the unfiltered set.
 *
 *  Deduplicates by id while preserving order, since the same model id
 *  could match two providers (sonnet→oauth via alias, sonnet→api via
 *  prefix expansion if config evolves) and the dropdown should show it
 *  once. */
export function modelsForProvider(
  providerName: string,
  models: readonly ModelSpec[],
  registry: readonly ProviderEntry[],
): ModelSpec[] {
  if (!providerName || providerName === AUTO_PROVIDER) return [...models];
  const provider = registry.find((p) => p.name === providerName);
  if (!provider) return [...models];

  const aliases = new Set((provider.model_aliases || []).map((a) => a.toLowerCase()));
  const prefixes = (provider.model_prefixes || [])
    .filter((p) => p)
    .map((p) => p.toLowerCase());

  const seen = new Set<string>();
  const out: ModelSpec[] = [];
  for (const m of models) {
    if (!m.id || seen.has(m.id)) continue;
    const lower = m.id.toLowerCase();
    if (aliases.has(lower) || prefixes.some((pre) => lower.startsWith(pre))) {
      seen.add(m.id);
      out.push(m);
    }
  }
  return out;
}

/** requiredEnvFor — union of (provider.auth_env, model.required_env)
 *  with provider auth_env first since it's the "any-of" auth path
 *  (e.g. ANTHROPIC_API_KEY OR ANTHROPIC_AUTH_TOKEN) and the per-model
 *  required_env is the "must-have" specifier the template wrote. The
 *  caller treats provider.auth_env as semantically "any one of" and
 *  model.required_env as "all of"; this helper just orders them.
 *
 *  Dedupes via Set so when both lists name CLAUDE_CODE_OAUTH_TOKEN we
 *  emit it once. */
export function requiredEnvFor(
  provider: ProviderEntry | null,
  model: ModelSpec | null,
): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const push = (v: string | undefined) => {
    if (v && !seen.has(v)) {
      seen.add(v);
      out.push(v);
    }
  };
  for (const e of provider?.auth_env || []) push(e);
  for (const e of model?.required_env || []) push(e);
  return out;
}

/** deriveProvidersFromModels — fallback for templates that ship neither
 *  a structured registry nor a flat providers list. Take the first
 *  vendor-prefix segment from each model id (split on `:` or `/`) and
 *  dedupe. Hermes-style legacy shape.
 *
 *  Returns [] when nothing can be derived — the form then renders the
 *  Provider field as a free-text input rather than an empty select.
 *  Callers convert these strings into pseudo ProviderEntry rows
 *  (name-only, no aliases/prefixes/auth_env) for the dropdown. */
export function deriveProvidersFromModels(models: readonly ModelSpec[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const m of models) {
    if (!m.id) continue;
    const sep = m.id.match(/[:/]/)?.index ?? -1;
    if (sep <= 0) continue;
    const vendor = m.id.slice(0, sep);
    if (!seen.has(vendor)) {
      seen.add(vendor);
      out.push(vendor);
    }
  }
  return out;
}

/** providerListForRuntime — single source of truth for the provider
 *  dropdown options. Prefers the structured registry, falls back to
 *  the flat `providers []string` list (synthesizing minimal
 *  ProviderEntry rows so the rest of the cascade has a uniform shape),
 *  finally falls back to vendor-prefix derivation. Returns the list
 *  the form should render directly. */
export function providerListForRuntime(
  registry: readonly ProviderEntry[] | undefined,
  flatProviders: readonly string[] | undefined,
  models: readonly ModelSpec[],
): ProviderEntry[] {
  if (registry && registry.length > 0) return [...registry];
  const flat = (flatProviders && flatProviders.length > 0)
    ? flatProviders
    : deriveProvidersFromModels(models);
  return flat.map((name) => ({ name }));
}
