"use client";

/**
 * ProviderModelSelector — single source of truth for the provider→model
 * dropdown chain shared across:
 *   1. MissingKeysModal (template deploy / first-time onboarding modal)
 *   2. ConfigTab (per-workspace settings — Runtime section)
 *   3. TemplatePalette (template side panel — inherits via MissingKeysModal)
 *
 * The user picks Provider FIRST (Anthropic API, Claude Code subscription,
 * MiniMax, Z.ai GLM, ...). The model dropdown then filters to only that
 * provider's models. Wildcard providers (huggingface/*, openrouter/*,
 * custom/*) reveal a free-text model input with a tooltip explaining the
 * wildcard.
 *
 * Provider taxonomy:
 *   - Multiple models can share the same `required_env` (e.g. all
 *     ANTHROPIC_AUTH_TOKEN-routed third-party providers — MiniMax, GLM,
 *     Kimi, DeepSeek). Grouping ONLY by env-tuple collapses them all into
 *     one bucket. We split further by vendor inferred from the model id
 *     so the user sees "MiniMax" and "Z.ai (GLM)" as separate options.
 *   - Vendor is inferred via prefix rules below. Templates that ship
 *     explicit vendor metadata (future) should override the heuristic.
 */

import { useId, useMemo } from "react";

export interface SelectorModel {
  id: string;
  name?: string;
  required_env?: string[];
}

/** A provider option in the dropdown — one row corresponds to one
 *  vendor + env-tuple combo, holding the models that map to it. */
export interface ProviderEntry {
  /** Stable id used as the <option value>. `${vendor}|${sortedEnv}`. */
  id: string;
  /** Inferred vendor key (e.g. "minimax", "anthropic-oauth"). */
  vendor: string;
  /** Human label shown in the dropdown. */
  label: string;
  /** Env vars required by every model in this provider. */
  envVars: string[];
  /** Models bucketed under this provider. */
  models: SelectorModel[];
  /** True when ANY model id contains "*" — UI shows free-text model input. */
  wildcard: boolean;
  /** Optional tooltip text (rendered as native title=). */
  tooltip?: string;
}

export interface SelectorValue {
  /** ProviderEntry.id of the selected provider. Empty string = nothing
   *  picked yet (parent should treat as invalid for save). */
  providerId: string;
  /** Selected model slug. For wildcard providers this is whatever the
   *  user typed in the free-text input. */
  model: string;
  /** Snapshot of envVars from the selected provider. Re-emitted on every
   *  change so consumers can re-render credential fields without
   *  re-inferring from the model. */
  envVars: string[];
}

interface Props {
  models: SelectorModel[];
  value: SelectorValue;
  onChange: (next: SelectorValue) => void;
  /** Display variant. "grid" = label+control side-by-side (used in ConfigTab
   *  Runtime section). "stack" = vertical (used in MissingKeysModal). */
  variant?: "grid" | "stack";
  /** When true, parent caller is opting in to power-user free-text. Adds a
   *  "Custom (type model id)..." escape-hatch entry as a model option even
   *  when the chosen provider isn't wildcard. ConfigTab uses this; the
   *  deploy modal does not. */
  allowCustomModelEscape?: boolean;
  disabled?: boolean;
  /** Optional id-prefix for label↔control wiring (WCAG 1.3.1). Default
   *  uses useId(). */
  idPrefix?: string;
}

// -----------------------------------------------------------------------------
// Vendor detection — id-prefix heuristic + bare-name patterns.
// -----------------------------------------------------------------------------

/** Vendor keys → human label. Add new vendors here when templates pick
 *  up new model families. */
const VENDOR_LABELS: Record<string, string> = {
  "anthropic-oauth": "Claude Code subscription",
  anthropic: "Anthropic API",
  minimax: "MiniMax",
  zai: "Z.ai (GLM)",
  moonshot: "Moonshot (Kimi)",
  deepseek: "DeepSeek",
  "xiaomi-mimo": "Xiaomi MiMo",
  openai: "OpenAI",
  google: "Google Gemini",
  alibaba: "Alibaba Qwen (DashScope)",
  nousresearch: "Nous Research (Hermes)",
  openrouter: "OpenRouter (any model)",
  huggingface: "Hugging Face Inference",
  "ai-gateway": "Vercel AI Gateway",
  "opencode-zen": "OpenCode Zen",
  "opencode-go": "OpenCode Go",
  kilocode: "Kilo Code",
  "kimi-coding": "Moonshot Kimi (coding-tuned)",
  "minimax-cn": "MiniMax China",
  "ollama-cloud": "Ollama Cloud",
  ollama: "Ollama (self-hosted)",
  nvidia: "NVIDIA NIM",
  arcee: "Arcee",
  xiaomi: "Xiaomi MiMo",
  gemini: "Google Gemini",
  custom: "Custom OpenAI-compat endpoint",
};

/** Optional per-vendor tooltip shown on hover. */
const VENDOR_TOOLTIPS: Record<string, string> = {
  "anthropic-oauth":
    "Use your Claude.ai (Pro/Max/Team) subscription via OAuth. Run `claude login` in the workspace terminal to mint the token, then paste it here. No API spend.",
  anthropic:
    "Pay-per-token via the Anthropic API (Console). Provide an API key starting with sk-ant-…",
  minimax:
    "MiniMax models served through their Anthropic-API-compatible endpoint. Get a key at platform.minimax.io.",
  zai:
    "Zhipu AI / z.ai GLM models through the Anthropic-compatible gateway. Get a key at docs.z.ai.",
  moonshot:
    "Moonshot Kimi K2-series via Anthropic-API-compatible endpoint. Get a key at platform.kimi.ai.",
  deepseek:
    "DeepSeek V4 via Anthropic-API-compatible endpoint. Get a key at api-docs.deepseek.com.",
  openrouter:
    "OpenRouter routes to 200+ models behind one API. Use any openrouter/<model> id. Get a key at openrouter.ai.",
  huggingface:
    "Any model hosted on Hugging Face Inference. Type the full model id (e.g. mistralai/Mistral-7B-Instruct-v0.3).",
  custom:
    "Self-hosted OpenAI-compatible endpoint (LM Studio, Ollama local, vLLM, llama.cpp). Configure base_url in the workspace's runtime config. No API key required.",
};

/** Sentinel value used in the model <select> for the free-text escape hatch
 *  added by `allowCustomModelEscape`. The component swaps to a text input
 *  when this is selected. */
const CUSTOM_MODEL_SENTINEL = "__custom__";

/** Bare-id vendor patterns (no slash separator). Order matters — first
 *  match wins. */
const BARE_VENDOR_PATTERNS: Array<{ test: (id: string) => boolean; vendor: string }> = [
  { test: (id) => /^minimax-/i.test(id) || /^MiniMax-/.test(id), vendor: "minimax" },
  { test: (id) => /^GLM-/i.test(id), vendor: "zai" },
  { test: (id) => /^kimi-/i.test(id), vendor: "moonshot" },
  { test: (id) => /^deepseek-/i.test(id), vendor: "deepseek" },
  { test: (id) => /^mimo-/i.test(id), vendor: "xiaomi-mimo" },
  { test: (id) => /^claude-/i.test(id), vendor: "anthropic" },
  { test: (id) => /^gpt-/i.test(id), vendor: "openai" },
  { test: (id) => /^gemini-/i.test(id), vendor: "google" },
  { test: (id) => /^qwen-/i.test(id), vendor: "alibaba" },
  // Claude-Code OAuth aliases — bare "sonnet"/"opus"/"haiku" + CLAUDE_CODE_OAUTH_TOKEN
  // is the strongest signal that this is a subscription model. We also
  // gate on env in inferVendor() below to avoid mis-tagging non-OAuth
  // models that happen to be named "sonnet".
  { test: (id) => /^(sonnet|opus|haiku)$/i.test(id), vendor: "anthropic-oauth" },
];

/** Infer a vendor key from a model spec. Combines id-prefix and env
 *  signals. Exported for tests. */
export function inferVendor(model: SelectorModel): string {
  const id = model.id || "";
  const envSet = new Set(model.required_env ?? []);

  // 1. Explicit slash-separated prefix wins (e.g. nousresearch/hermes-4-70b).
  const slashIdx = id.indexOf("/");
  if (slashIdx > 0) {
    return id.slice(0, slashIdx).toLowerCase();
  }

  // 2. Bare-id pattern. Special-case the OAuth aliases — they only count
  //    when the env actually demands the OAuth token. Otherwise (e.g.
  //    a hypothetical "sonnet" alias against ANTHROPIC_API_KEY) fall
  //    through and let the env-based fallback bucket it under
  //    "anthropic".
  for (const p of BARE_VENDOR_PATTERNS) {
    if (!p.test(id)) continue;
    if (p.vendor === "anthropic-oauth" && !envSet.has("CLAUDE_CODE_OAUTH_TOKEN")) {
      continue;
    }
    return p.vendor;
  }

  // 3. Env-tuple fallback. Pick the first env's "namespace" as the
  //    vendor — e.g. OPENROUTER_API_KEY → "openrouter".
  const env = model.required_env?.[0];
  if (env) {
    const ns = env.replace(/_API_KEY$|_TOKEN$|_KEY$/i, "").toLowerCase();
    return ns || "unknown";
  }

  return "unknown";
}

/** Build the provider catalog from the template's models[]. Models are
 *  bucketed by `(vendor, sortedEnv)` so two distinct env-tuples for the
 *  same vendor (rare but possible) become two separate entries. */
export function buildProviderCatalog(models: SelectorModel[]): ProviderEntry[] {
  const buckets = new Map<string, ProviderEntry>();

  for (const m of models) {
    const envs = m.required_env ?? [];
    const sortedEnv = [...envs].sort().join("|");
    const vendor = inferVendor(m);
    const id = `${vendor}|${sortedEnv}`;
    const wildcard = m.id.includes("*");

    let entry = buckets.get(id);
    if (!entry) {
      const baseLabel = VENDOR_LABELS[vendor] ?? vendor;
      entry = {
        id,
        vendor,
        label: baseLabel,
        envVars: envs,
        models: [],
        wildcard,
        tooltip: VENDOR_TOOLTIPS[vendor],
      };
      buckets.set(id, entry);
    }
    entry.models.push(m);
    // Wildcard sticks if any model in the bucket is a wildcard — same
    // bucket can't mix wildcard and concrete because they'd typically
    // share required_env but rarely the same vendor. Defensive OR.
    entry.wildcard = entry.wildcard || wildcard;
  }

  // Decorate label with model-count when ≥2 concrete models share the
  // bucket. Helps the user understand "Anthropic API (5 models)" vs
  // "MiniMax (3 models)".
  for (const e of buckets.values()) {
    if (!e.wildcard && e.models.length > 1) {
      e.label = `${e.label} (${e.models.length} models)`;
    }
  }

  return Array.from(buckets.values());
}

/** Find the provider entry that contains a given model id. Used by
 *  callers to back-derive the provider when only the model is known
 *  (e.g. ConfigTab loading from saved state). */
export function findProviderForModel(
  catalog: ProviderEntry[],
  modelId: string,
): ProviderEntry | null {
  if (!modelId) return null;
  for (const p of catalog) {
    if (p.models.some((m) => m.id === modelId)) return p;
    // Wildcard match — entry has model id ending in "*" and the typed
    // id starts with the wildcard's prefix (e.g. "openrouter/anthropic/
    // claude-3.5-sonnet" matches the "openrouter/*" bucket).
    if (p.wildcard) {
      for (const m of p.models) {
        if (!m.id.endsWith("*")) continue;
        const prefix = m.id.slice(0, -1);
        if (modelId.startsWith(prefix)) return p;
      }
    }
  }
  return null;
}

// -----------------------------------------------------------------------------
// Component
// -----------------------------------------------------------------------------

export function ProviderModelSelector({
  models,
  value,
  onChange,
  variant = "stack",
  allowCustomModelEscape = false,
  disabled = false,
  idPrefix,
}: Props) {
  const generatedId = useId();
  const baseId = idPrefix ?? generatedId;
  const providerSelectId = `${baseId}-provider`;
  const modelSelectId = `${baseId}-model`;

  const catalog = useMemo(() => buildProviderCatalog(models), [models]);
  const selected = useMemo(
    () => catalog.find((p) => p.id === value.providerId) ?? null,
    [catalog, value.providerId],
  );

  // True when the user picked the "Custom (type model id)..." escape entry
  // in the model dropdown — switches to free-text. Wildcard providers
  // ALWAYS use free-text, so this flag is for the escape hatch on
  // non-wildcard providers.
  const userPickedCustom = value.model === CUSTOM_MODEL_SENTINEL || (
    !!selected &&
    !selected.wildcard &&
    !!value.model &&
    !selected.models.some((m) => m.id === value.model)
  );
  const useTextInput = (selected?.wildcard ?? false) || userPickedCustom;

  const handleProviderChange = (nextProviderId: string) => {
    const next = catalog.find((p) => p.id === nextProviderId) ?? null;
    if (!next) {
      onChange({ providerId: "", model: "", envVars: [] });
      return;
    }
    // When switching providers:
    //   - wildcard provider → empty (free-text input takes over)
    //   - exactly 1 concrete model → auto-pick (no choice to make)
    //   - 2+ concrete models → leave empty so the operator MUST pick
    //
    // Background: previously this defaulted to `next.models[0]` for any
    // non-wildcard provider, which silently set the alphabetically-first
    // model in the bucket. Bit a real user on 2026-05-03 — they picked
    // the MiniMax provider intending `MiniMax-M2.7` but the form silently
    // set `MiniMax-M2` (first in the list). They never saw the model
    // dropdown change because the provider+model widgets are visually
    // distinct, and the workspace deployed with the wrong model. Caller
    // already disables Deploy/Save while `model.trim() === ""`, so the
    // empty default forces an explicit pick without loosening any other
    // gate.
    const defaultModel = next.wildcard
      ? ""
      : next.models.length === 1
        ? next.models[0]?.id ?? ""
        : "";
    onChange({
      providerId: next.id,
      model: defaultModel,
      envVars: next.envVars,
    });
  };

  const handleModelChange = (nextModel: string) => {
    if (!selected) {
      onChange({ ...value, model: nextModel });
      return;
    }
    onChange({
      providerId: selected.id,
      model: nextModel,
      envVars: selected.envVars,
    });
  };

  const containerClass = variant === "grid" ? "grid grid-cols-2 gap-3" : "space-y-3";

  return (
    <div className={containerClass} data-testid="provider-model-selector">
      <div>
        <label
          htmlFor={providerSelectId}
          className="text-[10px] uppercase tracking-wide text-ink-soft font-semibold mb-1.5 block"
        >
          Provider <span aria-hidden="true" className="text-bad">*</span>
          <span className="sr-only"> (required)</span>
        </label>
        <select
          id={providerSelectId}
          value={value.providerId}
          onChange={(e) => handleProviderChange(e.target.value)}
          disabled={disabled || catalog.length === 0}
          aria-describedby={selected?.tooltip ? `${providerSelectId}-help` : undefined}
          data-testid="provider-select"
          className="w-full bg-surface-sunken border border-line rounded px-2 py-1.5 text-[11px] text-ink focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/20 transition-colors disabled:opacity-50"
        >
          <option value="" disabled>
            — select provider —
          </option>
          {catalog.map((p) => (
            <option key={p.id} value={p.id} title={p.tooltip}>
              {p.label}
            </option>
          ))}
        </select>
        {selected?.tooltip && (
          <p
            id={`${providerSelectId}-help`}
            className="text-[9px] text-ink-soft mt-1 leading-relaxed"
          >
            {selected.tooltip}
          </p>
        )}
        {selected && selected.envVars.length > 0 && (
          <p className="text-[9px] text-ink-soft mt-0.5 font-mono">
            requires: {selected.envVars.join(", ")}
          </p>
        )}
      </div>

      <div>
        <label
          htmlFor={modelSelectId}
          className="text-[10px] uppercase tracking-wide text-ink-soft font-semibold mb-1.5 block"
        >
          Model <span aria-hidden="true" className="text-bad">*</span>
          <span className="sr-only"> (required)</span>
        </label>
        {useTextInput ? (
          <>
            <input
              id={modelSelectId}
              type="text"
              value={
                value.model === CUSTOM_MODEL_SENTINEL ? "" : value.model
              }
              onChange={(e) => handleModelChange(e.target.value.trim())}
              placeholder={
                selected?.wildcard
                  ? wildcardPlaceholder(selected)
                  : "type any model id"
              }
              disabled={disabled || !selected}
              spellCheck={false}
              autoComplete="off"
              data-testid="model-input"
              className="w-full bg-surface-sunken border border-line rounded px-2 py-1.5 text-[11px] text-ink font-mono focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/20 transition-colors disabled:opacity-50"
            />
            <p className="text-[9px] text-ink-soft mt-1 leading-relaxed">
              {selected?.wildcard
                ? wildcardHelpText(selected)
                : "Free-text model id. Make sure the provider can resolve it."}
            </p>
            {!selected?.wildcard && (
              <button
                type="button"
                onClick={() => {
                  // Switch back to dropdown by setting model to first
                  // concrete option.
                  if (selected) {
                    handleModelChange(selected.models[0]?.id ?? "");
                  }
                }}
                className="text-[9px] text-accent hover:text-accent mt-0.5"
              >
                ← back to model list
              </button>
            )}
          </>
        ) : (
          <select
            id={modelSelectId}
            value={
              value.model && selected?.models.some((m) => m.id === value.model)
                ? value.model
                : ""
            }
            onChange={(e) => {
              if (e.target.value === CUSTOM_MODEL_SENTINEL) {
                handleModelChange(CUSTOM_MODEL_SENTINEL);
              } else {
                handleModelChange(e.target.value);
              }
            }}
            disabled={disabled || !selected || selected.models.length === 0}
            data-testid="model-select"
            className="w-full bg-surface-sunken border border-line rounded px-2 py-1.5 text-[11px] text-ink font-mono focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/20 transition-colors disabled:opacity-50"
          >
            <option value="" disabled>
              {selected ? "— select model —" : "— select provider first —"}
            </option>
            {selected?.models
              .filter((m) => !m.id.includes("*"))
              .map((m) => (
                <option
                  key={m.id}
                  value={m.id}
                  title={m.name ?? m.id}
                >
                  {m.name ?? m.id}
                </option>
              ))}
            {allowCustomModelEscape && selected && (
              <option value={CUSTOM_MODEL_SENTINEL}>
                Custom (type model id)…
              </option>
            )}
          </select>
        )}
      </div>
    </div>
  );
}

function wildcardPlaceholder(p: ProviderEntry): string {
  const example = p.models.find((m) => m.id.includes("*"))?.id ?? "";
  if (!example) return "type any model id";
  // Strip trailing star — show the pattern as a hint.
  const prefix = example.replace(/\*$/, "");
  switch (p.vendor) {
    case "huggingface":
      return `e.g. ${prefix}meta-llama/Meta-Llama-3-70B-Instruct`;
    case "openrouter":
      return `e.g. ${prefix}anthropic/claude-3.5-sonnet`;
    case "custom":
      return `e.g. ${prefix}my-local-model`;
    default:
      return `e.g. ${prefix}<model-id>`;
  }
}

function wildcardHelpText(p: ProviderEntry): string {
  switch (p.vendor) {
    case "huggingface":
      return "Any model hosted on Hugging Face Inference. Browse at huggingface.co/models?inference=warm.";
    case "openrouter":
      return "Any of OpenRouter's 200+ routed models. Browse at openrouter.ai/models.";
    case "custom":
      return "Self-hosted endpoint. Configure base_url in your workspace's runtime config (no API key required).";
    case "ai-gateway":
      return "Vercel AI Gateway model id. See vercel.com/docs/ai-gateway.";
    case "opencode-zen":
      return "OpenCode Zen model id. See opencode.zen.";
    default:
      return "Wildcard provider — type the model id in full. Provider routes by id prefix.";
  }
}
