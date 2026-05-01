/**
 * Pre-deploy secret check driven by the template's config.yaml.
 *
 * The single source of truth for which env vars a workspace needs is
 * each template repo's config.yaml — the `runtime_config.models[].required_env`
 * array names the key(s) required per model, and `runtime_config.required_env`
 * names any AND-required keys at the runtime level. The Go `/templates`
 * handler parses these and exposes them as `models` and `required_env` on
 * each template summary.
 *
 * This module consumes that shape; it does NOT hardcode a per-runtime
 * provider table. When a template declares alternative models (e.g.
 * Hermes supports 35 models across 8 providers), the unique required_env
 * tuples become the provider options shown in the picker modal.
 */

import { api } from "./api";

/* ---------- Types matching the /templates response ---------- */

export interface ModelSpec {
  id: string;
  name?: string;
  required_env?: string[];
}

/** Minimal template shape consumed by the preflight check. Any object
 *  that matches this subset of the `/templates` response works. */
export interface TemplateLike {
  runtime: string;
  models?: ModelSpec[];
  /** AND-required env vars declared at runtime_config level. */
  required_env?: string[];
}

/** Full /templates response shape shared by TemplatePalette (sidebar)
 *  and EmptyState (welcome grid). Was previously re-declared in each
 *  with subtly different fields — EmptyState's narrower shape silently
 *  dropped `runtime`, `models`, and `required_env`, so the preflight
 *  couldn't see provider alternatives the template declared. Keep this
 *  the single source of truth.  */
export interface Template extends TemplateLike {
  id: string;
  name: string;
  description: string;
  tier: number;
  model: string;
  skills: string[];
  skill_count: number;
}

/** Map from a template id to the runtime name the per-workspace
 *  preflight expects. Used only when the server's `/templates`
 *  response predates the `runtime` field on the summary (legacy
 *  installs) — modern responses carry it verbatim. Strip `-default`
 *  for the claude-code template and identity-map everything else
 *  that matches our current runtime registry.
 *
 *  Lives in the preflight module (not TemplatePalette) so EmptyState
 *  uses the SAME fallback table. A previous duplication in both call
 *  sites left EmptyState with only the `-default` suffix strip, which
 *  would silently disagree with TemplatePalette on templates whose
 *  id needs a non-identity mapping. */
export function resolveRuntime(templateId: string): string {
  const runtimeMap: Record<string, string> = {
    langgraph: "langgraph",
    "claude-code-default": "claude-code",
    openclaw: "openclaw",
    deepagents: "deepagents",
    crewai: "crewai",
    autogen: "autogen",
  };
  return runtimeMap[templateId] ?? templateId.replace(/-default$/, "");
}

export interface SecretEntry {
  key: string;
  has_value: boolean;
  created_at: string;
  updated_at: string;
  scope?: "global" | "workspace";
}

export interface PreflightResult {
  ok: boolean;
  /** Flat list of env var names needed — for the legacy modal path and
   *  for callers that want a single display of "what's missing". */
  missingKeys: string[];
  /** Grouped provider options derived from the template. When length ≥ 2
   *  the modal renders a picker; length 1 means exactly one provider is
   *  required (AllKeysModal renders the N envVars inline). */
  providers: ProviderChoice[];
  runtime: string;
  /** Set of env var names already configured (i.e. `has_value: true`) at
   *  the relevant scope (workspace if `workspaceId` was passed, otherwise
   *  global). Surfaced so callers can mark pre-saved entries in the
   *  picker without making a second `/settings/secrets` round trip.
   *  Empty Set on secrets-endpoint failure (treated as "nothing set"). */
  configuredKeys: Set<string>;
}

/* ---------- Provider options ---------- */

/** One row in the provider picker. `envVars` is the set of keys required
 *  TOGETHER to satisfy this option (usually length 1 — e.g. just
 *  OPENROUTER_API_KEY). When length ≥ 2 all must be saved. */
export interface ProviderChoice {
  /** Stable id for React keys + picker value — the sorted envVars joined. */
  id: string;
  /** Human label, e.g. "OpenRouter" or "OpenAI + Serper". */
  label: string;
  /** Env vars required for this provider option. */
  envVars: string[];
  /** Short rationale shown under the option, optional. */
  note?: string;
}

/** Human-readable labels for well-known secret keys. Anything not in
 *  this table falls back to a humanized form of the env var. */
export const KEY_LABELS: Record<string, string> = {
  OPENAI_API_KEY: "OpenAI",
  ANTHROPIC_API_KEY: "Anthropic",
  GOOGLE_API_KEY: "Google AI",
  GEMINI_API_KEY: "Google Gemini",
  SERP_API_KEY: "SERP",
  SERPER_API_KEY: "Serper",
  OPENROUTER_API_KEY: "OpenRouter",
  HERMES_API_KEY: "Nous Research (Hermes native)",
  DEEPSEEK_API_KEY: "DeepSeek",
  GLM_API_KEY: "z.ai GLM",
  KIMI_API_KEY: "Moonshot Kimi",
  MINIMAX_API_KEY: "MiniMax",
  KILOCODE_API_KEY: "Kilo Code",
  CLAUDE_CODE_OAUTH_TOKEN: "Claude Code subscription",
};

/** Full "API Key" label used for input field headers. */
export function getKeyLabel(key: string): string {
  const base = KEY_LABELS[key];
  if (base) return `${base} API Key`;
  return humanizeEnvVar(key);
}

/** Short provider name used in the picker (no trailing "API Key"). */
export function getProviderLabel(key: string): string {
  return KEY_LABELS[key] ?? humanizeEnvVar(key);
}

function humanizeEnvVar(key: string): string {
  return key
    .replace(/_API_KEY$|_TOKEN$|_KEY$/i, "")
    .split(/[_-]/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
    .join(" ");
}

/**
 * Derive the provider options for a template from its declared shape.
 *
 *   1. `models[].required_env` — each unique (sorted) tuple becomes a
 *      provider option. E.g. Hermes exposes 8 options (Nous, OpenRouter,
 *      Anthropic, Gemini, DeepSeek, GLM, Kimi, Kilocode) even though it
 *      lists 35 models. Insertion order is preserved so the template's
 *      author controls which provider is offered first.
 *   2. If `models` is empty or has no required_env, fall back to the
 *      top-level `required_env` as a single all-required option.
 *   3. If neither is declared, return [] — no preflight needed.
 *
 * Models with `required_env: []` (local / self-hosted endpoints) are
 * skipped when computing options; they never block a deploy.
 */
export function providersFromTemplate(template: TemplateLike): ProviderChoice[] {
  const out: ProviderChoice[] = [];
  const seen = new Set<string>();
  const modelCount: Record<string, number> = {};

  for (const m of template.models ?? []) {
    const envs = m.required_env ?? [];
    if (envs.length === 0) continue;
    const id = [...envs].sort().join("|");
    modelCount[id] = (modelCount[id] ?? 0) + 1;
    if (seen.has(id)) continue;
    seen.add(id);
    out.push({
      id,
      envVars: envs,
      label: envs.map(getProviderLabel).join(" + "),
    });
  }

  // Decorate labels with model-count hints when multiple models share
  // the same provider. Gives the user context: "OpenRouter (14 models)".
  for (const p of out) {
    const n = modelCount[p.id];
    if (n && n > 1) p.label = `${p.label} (${n} models)`;
  }

  if (out.length === 0 && template.required_env?.length) {
    const envs = template.required_env;
    out.push({
      id: [...envs].sort().join("|"),
      envVars: envs,
      label: envs.map(getProviderLabel).join(" + "),
    });
  }

  return out;
}

/** Helper: is any single provider option already satisfied by the set of
 *  configured keys? A provider is satisfied when EVERY envVar it requires
 *  is present. Returns the first such option or null. */
export function findSatisfiedProvider(
  providers: ProviderChoice[],
  configured: Set<string>,
): ProviderChoice | null {
  for (const p of providers) {
    if (p.envVars.every((k) => configured.has(k))) return p;
  }
  return null;
}

/* ---------- Preflight ---------- */

/**
 * Fetch configured secrets from the platform and decide whether the
 * workspace can deploy. When `workspaceId` is provided the merged
 * (global + workspace) secrets are checked; otherwise only globals.
 *
 * Returns `ok=true` immediately if any provider option's env vars are
 * already configured. Otherwise returns all candidate env vars flat in
 * `missingKeys` plus the grouped `providers` list for the picker.
 */
export async function checkDeploySecrets(
  template: TemplateLike,
  workspaceId?: string,
): Promise<PreflightResult> {
  const providers = providersFromTemplate(template);
  const runtime = template.runtime;

  if (providers.length === 0) {
    // Template declares no env requirements — nothing to preflight.
    return {
      ok: true,
      missingKeys: [],
      providers: [],
      runtime,
      configuredKeys: new Set(),
    };
  }

  let configured: Set<string>;
  try {
    const secrets = workspaceId
      ? await api.get<SecretEntry[]>(`/workspaces/${workspaceId}/secrets`)
      : await api.get<SecretEntry[]>("/settings/secrets");
    configured = new Set(secrets.filter((s) => s.has_value).map((s) => s.key));
  } catch (error) {
    console.error(
      "[deploy-preflight] Failed to read secrets, assuming all missing:",
      error,
    );
    // Safer to prompt the user than to silently deploy.
    configured = new Set();
  }

  if (findSatisfiedProvider(providers, configured)) {
    return {
      ok: true,
      missingKeys: [],
      providers,
      runtime,
      configuredKeys: configured,
    };
  }

  // Nothing configured — surface every candidate env var so the modal
  // can render the picker or the all-keys fallback.
  const missingKeys = Array.from(
    new Set(providers.flatMap((p) => p.envVars)),
  );
  return {
    ok: false,
    missingKeys,
    providers,
    runtime,
    configuredKeys: configured,
  };
}
