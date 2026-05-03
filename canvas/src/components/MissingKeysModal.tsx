"use client";

import { useState, useEffect, useCallback, useRef, useMemo } from "react";
import { createPortal } from "react-dom";
import { api } from "@/lib/api";
import {
  getKeyLabel,
  type ModelSpec,
  type ProviderChoice,
} from "@/lib/deploy-preflight";
import {
  ProviderModelSelector,
  buildProviderCatalog,
  findProviderForModel,
  type SelectorValue,
} from "./ProviderModelSelector";

interface Props {
  open: boolean;
  /** Flat list of every candidate env var. Used as the fallback input
   *  set when `providers` is empty (or length 1). */
  missingKeys: string[];
  /** Grouped provider options derived from the template's models[] /
   *  required_env. When length ≥ 2 the modal shows a radio picker. */
  providers?: ProviderChoice[];
  /** Runtime slug — used only for the "The <runtime> runtime …"
   *  headline; behavior is driven by providers/missingKeys. */
  runtime: string;
  /** Called when all required keys for the chosen provider are saved.
   *  Receives the model slug if the modal collected one (template-deploy
   *  flow); legacy callers ignore it. */
  onKeysAdded: (model?: string) => void;
  /** Called when the user cancels the deploy. */
  onCancel: () => void;
  /** Optional — open the Settings Panel (Config tab → Secrets). */
  onOpenSettings?: () => void;
  /** If provided, secrets save at workspace scope instead of global. */
  workspaceId?: string;
  /** Set of env var names already configured in the relevant scope
   *  (global or workspace). When provided, entries whose key is already
   *  in this set start as `saved: true` so the user can confirm without
   *  re-entering. Used by the template-deploy "always ask" flow so a
   *  user can pick a different provider even when global env covers
   *  the default one. */
  configuredKeys?: Set<string>;
  /** Model slug suggestions (datalist) — populated from the template's
   *  models[]. When non-empty the picker renders a model input above
   *  the API-key fields. The picker passes the entered slug back via
   *  onKeysAdded. */
  modelSuggestions?: string[];
  /** Full model specs from the template (with required_env per model).
   *  When provided, the picker auto-snaps the provider radio to the
   *  matching provider as the user changes the model — fixes the
   *  "type MiniMax model, see ANTHROPIC_API_KEY field" cascade bug
   *  (sibling of the ConfigTab cascade fix in #2516). Optional so
   *  callers without model→provider mapping data can still use the
   *  picker as-is. */
  models?: ModelSpec[];
  /** Pre-fill the model input. */
  initialModel?: string;
  /** Override the modal's title + description copy. The default
   *  "Missing API Keys" title misreads when the modal is opened to
   *  pick provider/model with keys already configured. */
  title?: string;
  description?: string;
}

interface KeyEntry {
  key: string;
  value: string;
  saved: boolean;
  saving: boolean;
  error: string | null;
}

/**
 * MissingKeysModal
 * ----------------
 * Dispatches between two modes based on what the template declares:
 *
 *  1. PROVIDER PICKER — when the preflight returned ≥2 `providers` (e.g.
 *     a Hermes template whose models[].required_env enumerate OpenRouter,
 *     Anthropic, Nous-native, etc.). Radio list of options, saving the
 *     chosen option's env vars satisfies the deploy.
 *
 *  2. ALL-KEYS — every entry in `missingKeys` rendered as its own input,
 *     all must save before Deploy. Used when the template has a single
 *     provider option or no declared alternatives.
 *
 * The modal never hardcodes per-runtime provider lists; the upstream
 * preflight derives that from the template config.yaml.
 */
export function MissingKeysModal({
  open,
  missingKeys,
  providers,
  runtime,
  onKeysAdded,
  onCancel,
  onOpenSettings,
  workspaceId,
  configuredKeys,
  modelSuggestions,
  models,
  initialModel,
  title,
  description,
}: Props) {
  const pickerProviders = providers ?? [];
  const pickerMode = pickerProviders.length > 1;

  if (pickerMode) {
    return (
      <ProviderPickerModal
        open={open}
        providers={pickerProviders}
        runtime={runtime}
        onKeysAdded={onKeysAdded}
        onCancel={onCancel}
        onOpenSettings={onOpenSettings}
        workspaceId={workspaceId}
        configuredKeys={configuredKeys}
        modelSuggestions={modelSuggestions}
        models={models}
        initialModel={initialModel}
        title={title}
        description={description}
      />
    );
  }

  // Prefer the (single) provider's envVars over the raw missingKeys when
  // we have one — the provider list is already de-duped and ordered.
  const keys =
    pickerProviders.length === 1 ? pickerProviders[0].envVars : missingKeys;

  return (
    <AllKeysModal
      open={open}
      missingKeys={keys}
      runtime={runtime}
      onKeysAdded={onKeysAdded}
      onCancel={onCancel}
      onOpenSettings={onOpenSettings}
      workspaceId={workspaceId}
    />
  );
}

// -----------------------------------------------------------------------------
// Provider-picker mode — choose one option, save its env var(s), deploy.
// -----------------------------------------------------------------------------

/** Provider id derived from a model spec — sorted+joined required_env,
 *  matching the formula in providersFromTemplate(). When the model has
 *  no required_env (local/self-hosted endpoints) returns null, since
 *  there's no provider option the radio could snap to. Exported for
 *  the cascade-snap test. */
export function providerIdForModel(
  modelId: string,
  models: ModelSpec[] | undefined,
): string | null {
  const trimmed = modelId.trim();
  if (!trimmed || !models) return null;
  const m = models.find((x) => x.id === trimmed);
  if (!m?.required_env || m.required_env.length === 0) return null;
  return [...m.required_env].sort().join("|");
}

function ProviderPickerModal({
  open,
  providers,
  runtime,
  onKeysAdded,
  onCancel,
  onOpenSettings,
  workspaceId,
  configuredKeys,
  modelSuggestions,
  models,
  initialModel,
  title,
  description,
}: {
  open: boolean;
  providers: ProviderChoice[];
  runtime: string;
  onKeysAdded: (model?: string) => void;
  onCancel: () => void;
  onOpenSettings?: () => void;
  workspaceId?: string;
  configuredKeys?: Set<string>;
  modelSuggestions?: string[];
  models?: ModelSpec[];
  initialModel?: string;
  title?: string;
  description?: string;
}) {
  // Single model source: `models` from caller when present, else
  // synthesize a stub list from the legacy `providers` shape so older
  // callers (pre-PR-2534) still drive the picker. ProviderModelSelector
  // and findProviderForModel BOTH consume this list — passing the same
  // shape to both keeps ids identical, so back-derivation matches the
  // dropdown's option values.
  const selectorModels = useMemo(() => {
    if (models && models.length > 0) return models;
    return providers.map((p) => ({
      id: p.id,
      name: p.label,
      required_env: p.envVars,
    }));
  }, [models, providers]);

  const catalog = useMemo(() => buildProviderCatalog(selectorModels), [selectorModels]);

  // Initial selector value: prefer back-derivation from initialModel
  // (template-deploy passes the template default), then the first
  // provider already satisfied by configuredKeys, then catalog[0].
  const initial = useMemo<SelectorValue>(() => {
    if (initialModel) {
      const matched = findProviderForModel(catalog, initialModel);
      if (matched) {
        return {
          providerId: matched.id,
          model: initialModel,
          envVars: matched.envVars,
        };
      }
    }
    if (configuredKeys) {
      const satisfied = catalog.find((p) =>
        p.envVars.every((k) => configuredKeys.has(k)),
      );
      if (satisfied) {
        return {
          providerId: satisfied.id,
          model: satisfied.wildcard ? "" : satisfied.models[0]?.id ?? "",
          envVars: satisfied.envVars,
        };
      }
    }
    const first = catalog[0];
    if (!first) return { providerId: "", model: "", envVars: [] };
    return {
      providerId: first.id,
      model: first.wildcard ? "" : first.models[0]?.id ?? "",
      envVars: first.envVars,
    };
  }, [catalog, initialModel, configuredKeys]);

  const [selectorValue, setSelectorValue] = useState<SelectorValue>(initial);
  const [entries, setEntries] = useState<KeyEntry[]>([]);
  const firstInputRef = useRef<HTMLInputElement>(null);

  // Legacy compat: map the selector value back into the old `selected`/
  // `model` shape for the rest of the modal body (footer copy, etc.).
  const selected = useMemo(
    () =>
      providers.find((p) => p.id === selectorValue.providerId) ??
      providers[0],
    [providers, selectorValue.providerId],
  );
  const model = selectorValue.model;
  const showModelInput = catalog.length > 0;

  useEffect(() => {
    if (!open) return;
    setSelectorValue(initial);
  }, [open, initial]);

  useEffect(() => {
    if (!open) return;
    setEntries(
      selectorValue.envVars.map((key) => ({
        key,
        value: "",
        // Pre-mark as saved when the key is already in the configured
        // set (global or workspace scope). Lets the user click Deploy
        // without re-entering a key the platform already holds.
        saved: configuredKeys?.has(key) ?? false,
        saving: false,
        error: null,
      })),
    );
  }, [open, selectorValue.envVars, configuredKeys]);

  useEffect(() => {
    if (!open) return;
    const raf = requestAnimationFrame(() => firstInputRef.current?.focus());
    return () => cancelAnimationFrame(raf);
  }, [open, selectorValue.providerId]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onCancel]);

  const updateEntry = useCallback(
    (index: number, updates: Partial<KeyEntry>) => {
      setEntries((prev) =>
        prev.map((e, i) => (i === index ? { ...e, ...updates } : e)),
      );
    },
    [],
  );

  const handleSaveKey = useCallback(
    async (index: number) => {
      const entry = entries[index];
      if (!entry.value.trim()) return;
      updateEntry(index, { saving: true, error: null });
      try {
        if (workspaceId) {
          await api.put(`/workspaces/${workspaceId}/secrets`, {
            key: entry.key,
            value: entry.value.trim(),
          });
        } else {
          await api.put("/settings/secrets", {
            key: entry.key,
            value: entry.value.trim(),
          });
        }
        updateEntry(index, { saved: true, saving: false });
      } catch (e) {
        updateEntry(index, {
          saving: false,
          error: e instanceof Error ? e.message : "Failed to save",
        });
      }
    },
    [entries, updateEntry, workspaceId],
  );

  if (!open) return null;
  // Portal to document.body for the same reason as
  // OrgImportPreflightModal — several callers (TemplatePalette,
  // EmptyState) render the modal inside their own fixed+filtered
  // containers, which re-anchor the "fixed" positioning to the
  // wrapper's bounds instead of the viewport.
  if (typeof document === "undefined") return null;

  const allSaved = entries.length > 0 && entries.every((e) => e.saved);
  const anySaving = entries.some((e) => e.saving);
  const runtimeLabel = runtime
    .replace(/[-_]/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());

  return createPortal(
    // z-[60] so this stacks ABOVE OrgImportPreflightModal (z-50).
    // Both can be on screen at once during an org import: the org-
    // preflight is open while the user clicks a per-workspace deploy
    // that triggers MissingKeys. Without the explicit z-order the
    // backdrop click might dismiss the wrong modal depending on
    // React's commit ordering.
    <div className="fixed inset-0 z-[60] flex items-center justify-center">
      <div
        aria-hidden="true"
        className="absolute inset-0 bg-black/70 backdrop-blur-sm"
        onClick={onCancel}
      />

      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="missing-keys-title"
        className="relative bg-surface-sunken border border-line rounded-xl shadow-2xl shadow-black/50 max-w-[480px] w-full mx-4 max-h-[80vh] overflow-auto"
      >
        <div className="px-5 py-4 border-b border-line">
          <div className="flex items-center gap-2 mb-1">
            <div
              className="w-5 h-5 rounded-md bg-amber-600/20 border border-amber-500/30 flex items-center justify-center"
              aria-hidden="true"
            >
              <svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
                <path d="M6 1L11 10H1L6 1Z" stroke="#fbbf24" strokeWidth="1.2" strokeLinejoin="round" />
                <path d="M6 5V7" stroke="#fbbf24" strokeWidth="1.2" strokeLinecap="round" />
                <circle cx="6" cy="8.5" r="0.5" fill="#fbbf24" />
              </svg>
            </div>
            <h3 id="missing-keys-title" className="text-sm font-semibold text-ink">
              {title ?? "Missing API Keys"}
            </h3>
          </div>
          <p className="text-[12px] text-ink-mid leading-relaxed">
            {description ?? (
              <>
                The <span className="text-warm font-medium">{runtimeLabel}</span>{" "}
                runtime supports multiple providers. Pick one and paste its API key.
              </>
            )}
          </p>
        </div>

        <div className="px-5 py-4 space-y-3">
          {/* Shared provider→model selector. Source of truth for provider
              taxonomy + model filtering. Same component is used in
              ConfigTab so behavior + vendor split is identical across
              all 3 deploy surfaces (modal here, settings tab, template
              palette flow). */}
          <ProviderModelSelector
            models={selectorModels}
            value={selectorValue}
            onChange={setSelectorValue}
            variant="stack"
            idPrefix="provider-picker"
          />

          <div className="space-y-2">
            {entries.map((entry, index) => (
              <div
                key={entry.key}
                className="bg-surface-card/50 rounded-lg px-3 py-2.5 border border-line/50"
              >
                <div className="flex items-center justify-between mb-1.5">
                  <div>
                    <div className="text-[11px] text-ink-mid font-medium">
                      {getKeyLabel(entry.key)}
                    </div>
                    <div className="text-[9px] font-mono text-ink-soft">{entry.key}</div>
                  </div>
                  {entry.saved && (
                    <span className="text-[9px] text-good bg-emerald-900/30 px-1.5 py-0.5 rounded flex items-center gap-1">
                      <svg width="8" height="8" viewBox="0 0 8 8" fill="none" aria-hidden="true">
                        <path d="M1.5 4L3.5 6L6.5 2" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" />
                      </svg>
                      Saved
                    </span>
                  )}
                </div>

                {!entry.saved && (
                  <div className="flex gap-2 mt-2">
                    <input
                      value={entry.value}
                      onChange={(e) => updateEntry(index, { value: e.target.value.trimStart() })}
                      placeholder={entry.key.includes("API_KEY") ? "sk-..." : "Enter value"}
                      type="password"
                      ref={index === 0 ? firstInputRef : undefined}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" && entry.value.trim()) {
                          handleSaveKey(index);
                        }
                      }}
                      className="flex-1 bg-surface-sunken border border-line rounded px-2 py-1.5 text-[11px] text-ink font-mono focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/20 transition-colors"
                    />
                    <button
                      onClick={() => handleSaveKey(index)}
                      disabled={!entry.value.trim() || entry.saving}
                      className="px-3 py-1.5 bg-accent-strong hover:bg-accent text-[11px] rounded text-white disabled:opacity-30 transition-colors shrink-0"
                    >
                      {entry.saving ? "..." : "Save"}
                    </button>
                  </div>
                )}

                {entry.error && (
                  <div className="mt-1.5 text-[10px] text-bad">{entry.error}</div>
                )}
              </div>
            ))}
          </div>
        </div>

        <div className="px-5 py-3 border-t border-line bg-surface/50 flex items-center justify-between gap-2">
          <div>
            {onOpenSettings && (
              <button
                onClick={onOpenSettings}
                className="text-[11px] text-accent hover:text-accent transition-colors"
              >
                Open Settings Panel
              </button>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={onCancel}
              className="px-3.5 py-1.5 text-[12px] text-ink-mid hover:text-ink bg-surface-card hover:bg-surface-card border border-line rounded-lg transition-colors"
            >
              Cancel Deploy
            </button>
            <button
              onClick={() => onKeysAdded(showModelInput ? model.trim() : undefined)}
              disabled={
                !allSaved ||
                anySaving ||
                !selectorValue.providerId ||
                (showModelInput && model.trim() === "")
              }
              className="px-3.5 py-1.5 text-[12px] bg-accent-strong hover:bg-accent text-white rounded-lg transition-colors disabled:opacity-40"
            >
              {allSaved ? "Deploy" : entries.length > 1 ? "Add Keys" : "Add Key"}
            </button>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  );
}

// -----------------------------------------------------------------------------
// All-keys mode — every missingKey rendered as its own input, all required.
// -----------------------------------------------------------------------------

function AllKeysModal({
  open,
  missingKeys,
  runtime,
  onKeysAdded,
  onCancel,
  onOpenSettings,
  workspaceId,
}: {
  open: boolean;
  missingKeys: string[];
  runtime: string;
  onKeysAdded: () => void;
  onCancel: () => void;
  onOpenSettings?: () => void;
  workspaceId?: string;
}) {
  const [entries, setEntries] = useState<KeyEntry[]>([]);
  const [globalError, setGlobalError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setEntries(
      missingKeys.map((key) => ({
        key,
        value: "",
        saved: false,
        saving: false,
        error: null,
      })),
    );
    setGlobalError(null);
  }, [open, missingKeys]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onCancel]);

  const updateEntry = useCallback(
    (index: number, updates: Partial<KeyEntry>) => {
      setEntries((prev) =>
        prev.map((entry, i) => (i === index ? { ...entry, ...updates } : entry)),
      );
    },
    [],
  );

  const handleSaveKey = useCallback(
    async (index: number) => {
      const entry = entries[index];
      if (!entry.value.trim()) return;

      updateEntry(index, { saving: true, error: null });

      try {
        if (workspaceId) {
          await api.put(`/workspaces/${workspaceId}/secrets`, {
            key: entry.key,
            value: entry.value.trim(),
          });
        } else {
          await api.put("/settings/secrets", {
            key: entry.key,
            value: entry.value.trim(),
          });
        }
        updateEntry(index, { saved: true, saving: false });
      } catch (e) {
        updateEntry(index, {
          saving: false,
          error: e instanceof Error ? e.message : "Failed to save",
        });
      }
    },
    [entries, updateEntry, workspaceId],
  );

  const handleAddKeysAndDeploy = useCallback(() => {
    const anySaving = entries.some((e) => e.saving);
    if (anySaving) {
      setGlobalError("Please wait for all keys to finish saving.");
      return;
    }
    const allSaved = entries.every((e) => e.saved);
    if (!allSaved) {
      setGlobalError("Please save all required keys before deploying.");
      return;
    }
    onKeysAdded();
  }, [entries, onKeysAdded]);

  // Focus trap: auto-focus first input when modal opens
  useEffect(() => {
    if (!open) return;
    const timer = requestAnimationFrame(() => {
      document.getElementById("missing-keys-title")?.focus();
    });
    return () => cancelAnimationFrame(timer);
  }, [open]);

  if (!open) return null;
  if (typeof document === "undefined") return null;

  const allSaved = entries.length > 0 && entries.every((e) => e.saved);
  const anySaving = entries.some((e) => e.saving);
  const runtimeLabel = runtime
    .replace(/[-_]/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());

  return createPortal(
    // z-[60] so this stacks ABOVE OrgImportPreflightModal (z-50).
    // Both can be on screen at once during an org import: the org-
    // preflight is open while the user clicks a per-workspace deploy
    // that triggers MissingKeys. Without the explicit z-order the
    // backdrop click might dismiss the wrong modal depending on
    // React's commit ordering.
    <div className="fixed inset-0 z-[60] flex items-center justify-center">
      <div
        className="absolute inset-0 bg-black/70 backdrop-blur-sm"
        aria-hidden="true"
        onClick={onCancel}
      />

      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="missing-keys-title"
        className="relative bg-surface-sunken border border-line rounded-xl shadow-2xl shadow-black/50 max-w-[440px] w-full mx-4 max-h-[80vh] overflow-auto"
      >
        <div className="px-5 py-4 border-b border-line">
          <div className="flex items-center gap-2 mb-1">
            <div
              className="w-5 h-5 rounded-md bg-amber-600/20 border border-amber-500/30 flex items-center justify-center"
              aria-hidden="true"
            >
              <svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
                <path d="M6 1L11 10H1L6 1Z" stroke="#fbbf24" strokeWidth="1.2" strokeLinejoin="round" />
                <path d="M6 5V7" stroke="#fbbf24" strokeWidth="1.2" strokeLinecap="round" />
                <circle cx="6" cy="8.5" r="0.5" fill="#fbbf24" />
              </svg>
            </div>
            <h3 id="missing-keys-title" className="text-sm font-semibold text-ink">
              Missing API Keys
            </h3>
          </div>
          <p className="text-[12px] text-ink-mid leading-relaxed">
            The <span className="text-warm font-medium">{runtimeLabel}</span>{" "}
            runtime requires the following keys to be configured before deploying.
          </p>
        </div>

        <div className="px-5 py-4 space-y-3 max-h-[50vh] overflow-y-auto">
          {entries.map((entry, index) => (
            <div
              key={entry.key}
              className="bg-surface-card/50 rounded-lg px-3 py-2.5 border border-line/50"
            >
              <div className="flex items-center justify-between mb-1">
                <div>
                  <div className="text-[11px] text-ink-mid font-medium">
                    {getKeyLabel(entry.key)}
                  </div>
                  <div className="text-[9px] font-mono text-ink-soft">{entry.key}</div>
                </div>
                {entry.saved && (
                  <span className="text-[9px] text-good bg-emerald-900/30 px-1.5 py-0.5 rounded flex items-center gap-1">
                    <svg width="8" height="8" viewBox="0 0 8 8" fill="none">
                      <path d="M1.5 4L3.5 6L6.5 2" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" />
                    </svg>
                    Saved
                  </span>
                )}
              </div>

              {!entry.saved && (
                <div className="flex gap-2 mt-2">
                  <input
                    value={entry.value}
                    onChange={(e) => updateEntry(index, { value: e.target.value.trimStart() })}
                    placeholder={entry.key.includes("API_KEY") ? "sk-..." : "Enter value"}
                    type="password"
                    autoFocus={index === 0}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" && entry.value.trim()) {
                        handleSaveKey(index);
                      }
                    }}
                    className="flex-1 bg-surface-sunken border border-line rounded px-2 py-1.5 text-[11px] text-ink font-mono focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/20 transition-colors"
                  />
                  <button
                    type="button"
                    onClick={() => handleSaveKey(index)}
                    disabled={!entry.value.trim() || entry.saving}
                    className="px-3 py-1.5 bg-accent-strong hover:bg-accent text-[11px] rounded text-white disabled:opacity-30 transition-colors shrink-0"
                  >
                    {entry.saving ? "..." : "Save"}
                  </button>
                </div>
              )}

              {entry.error && <div className="mt-1.5 text-[10px] text-bad">{entry.error}</div>}
            </div>
          ))}

          {globalError && (
            <div className="px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-[11px] text-bad">
              {globalError}
            </div>
          )}
        </div>

        <div className="px-5 py-3 border-t border-line bg-surface/50 flex items-center justify-between gap-2">
          <div>
            {onOpenSettings && (
              <button
                type="button"
                onClick={onOpenSettings}
                className="text-[11px] text-accent hover:text-accent transition-colors"
              >
                Open Settings Panel
              </button>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onCancel}
              className="px-3.5 py-1.5 text-[12px] text-ink-mid hover:text-ink bg-surface-card hover:bg-surface-card border border-line rounded-lg transition-colors"
            >
              Cancel Deploy
            </button>
            <button
              type="button"
              onClick={handleAddKeysAndDeploy}
              disabled={!allSaved || anySaving}
              className="px-3.5 py-1.5 text-[12px] bg-accent-strong hover:bg-accent text-white rounded-lg transition-colors disabled:opacity-40"
            >
              {anySaving ? "Saving..." : allSaved ? "Deploy" : "Add Keys"}
            </button>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  );
}
