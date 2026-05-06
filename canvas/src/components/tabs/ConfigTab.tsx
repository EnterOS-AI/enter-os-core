"use client";

import { useState, useEffect, useCallback, useRef, useId, useMemo } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { type ConfigData, DEFAULT_CONFIG, TextInput, NumberInput, Toggle, TagList, Section } from "./config/form-inputs";
import { parseYaml, toYaml } from "./config/yaml-utils";
import { SecretsSection } from "./config/secrets-section";
import { ExternalConnectionSection } from "./ExternalConnectionSection";
import {
  ProviderModelSelector,
  buildProviderCatalog,
  findProviderForModel,
  type SelectorValue,
} from "../ProviderModelSelector";

interface Props {
  workspaceId: string;
}

// --- Agent Card Section ---

function AgentCardSection({ workspaceId }: { workspaceId: string }) {
  const [card, setCard] = useState<Record<string, unknown> | null>(null);
  const [loading, setLoading] = useState(true);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  useEffect(() => {
    api.get<Record<string, unknown>>(`/workspaces/${workspaceId}`)
      .then((ws) => setCard((ws.agent_card as Record<string, unknown>) || null))
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [workspaceId]);

  const handleSave = async () => {
    setError(null);
    let parsed: unknown;
    try { parsed = JSON.parse(draft); } catch { setError("Invalid JSON"); return; }
    setSaving(true);
    try {
      await api.post("/registry/update-card", { workspace_id: workspaceId, agent_card: parsed });
      setCard(parsed as Record<string, unknown>);
      setSuccess(true);
      setEditing(false);
      setTimeout(() => setSuccess(false), 2000);
    } catch (e) { setError(e instanceof Error ? e.message : "Failed to update"); }
    finally { setSaving(false); }
  };

  return (
    <Section title="Agent Card" defaultOpen={false}>
      {loading ? (
        <div className="text-[10px] text-ink-soft">Loading...</div>
      ) : editing ? (
        <div className="space-y-2">
          <textarea
            aria-label="Agent card JSON editor"
            value={draft} onChange={(e) => setDraft(e.target.value)}
            spellCheck={false} rows={12}
            className="w-full bg-surface-card border border-line rounded p-2 text-[10px] font-mono text-ink focus:outline-none focus:border-accent resize-none"
          />
          {error && <div className="px-2 py-1 bg-red-900/30 border border-red-800 rounded text-[10px] text-bad">{error}</div>}
          <div className="flex gap-2">
            <button type="button" onClick={handleSave} disabled={saving}
              className="px-2 py-1 bg-accent hover:bg-accent-strong text-[10px] rounded text-white disabled:opacity-50 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">
              {saving ? "Saving..." : "Save"}
            </button>
            <button type="button" onClick={() => setEditing(false)}
              className="px-2 py-1 bg-surface-card hover:bg-surface-elevated hover:text-ink text-[10px] rounded text-ink-mid transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">Cancel</button>
          </div>
        </div>
      ) : (
        <div>
          {card ? (
            <pre className="text-[9px] text-ink-mid bg-surface-card/50 rounded p-2 overflow-x-auto max-h-48 border border-line/50">
              {JSON.stringify(card, null, 2)}
            </pre>
          ) : (
            <div className="text-[10px] text-ink-soft">No agent card</div>
          )}
          {success && <div className="mt-2 px-2 py-1 bg-green-900/30 border border-green-800 rounded text-[10px] text-good">Updated</div>}
          <button type="button" onClick={() => { setDraft(JSON.stringify(card || {}, null, 2)); setEditing(true); setError(null); setSuccess(false); }}
            className="mt-2 text-[10px] text-accent hover:text-accent">Edit Agent Card</button>
        </div>
      )}
    </Section>
  );
}

// --- Main ConfigTab ---

interface ModelSpec {
  id: string;
  name?: string;
  required_env?: string[];
}

function arraysEqual(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

interface RuntimeOption {
  value: string;
  label: string;
  models: ModelSpec[];
  // providers is the declarative provider list each template ships in
  // its config.yaml under runtime_config.providers. The /templates API
  // surfaces it (workspace-server templates.go) so canvas stays
  // adapter-driven: hermes ships ~20 slugs, claude-code ships
  // ["anthropic"], gemini-cli ships ["gemini"], etc. Empty list →
  // canvas falls back to deriving unique vendor prefixes from
  // models[].id (still adapter-driven, just inferred).
  providers: string[];
}

// deriveProvidersFromModels — when a template doesn't ship an explicit
// providers list, infer suggestions from the vendor prefixes of its
// model slugs. e.g. ["anthropic:claude-opus-4-7", "openai:gpt-4o",
// "anthropic:claude-sonnet-4-5"] → ["anthropic", "openai"].
//
// This keeps the dropdown adapter-driven for older templates that
// haven't migrated to the explicit `providers:` field yet, AND
// continues to be a useful fallback for any future runtime whose
// derive-provider semantics happen to match the slug prefix.
function deriveProvidersFromModels(models: ModelSpec[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const m of models) {
    if (!m.id) continue;
    // Both ":" (anthropic:claude-opus-4-7) and "/" (nousresearch/hermes-4-70b)
    // are valid vendor separators in our slug taxonomy. Take whichever
    // appears first and split there.
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

// Fallback used when /templates can't be fetched (offline, older backend).
// Keep in sync with manifest.json workspace_templates as a defensive default.
// Model + env suggestions only flow when the backend is reachable.
//
// Runtimes that manage their own config outside the platform's config.yaml
// template. For these, a missing config.yaml is expected and the form
// genuinely can't edit the runtime's settings (there's no platform file
// to write). Hermes is NOT on this list: it DOES ship a platform
// config.yaml via workspace-configs-templates/hermes that controls model,
// runtime_config, required_env, etc. Editing it through this form is
// exactly the point of the platform adaptor. The deep `~/.hermes/
// config.yaml` on the container is a separate runtime-internal file,
// not this one.
const RUNTIMES_WITH_OWN_CONFIG = new Set<string>(["external"]);

const FALLBACK_RUNTIME_OPTIONS: RuntimeOption[] = [
  { value: "", label: "LangGraph (default)", models: [], providers: [] },
  { value: "claude-code", label: "Claude Code", models: [], providers: [] },
  { value: "crewai", label: "CrewAI", models: [], providers: [] },
  { value: "autogen", label: "AutoGen", models: [], providers: [] },
  { value: "deepagents", label: "DeepAgents", models: [], providers: [] },
  { value: "openclaw", label: "OpenClaw", models: [], providers: [] },
  { value: "hermes", label: "Hermes", models: [], providers: [] },
  { value: "gemini-cli", label: "Gemini CLI", models: [], providers: [] },
];

export function ConfigTab({ workspaceId }: Props) {
  const [config, setConfig] = useState<ConfigData>({ ...DEFAULT_CONFIG });
  const [originalYaml, setOriginalYaml] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);
  const [rawMode, setRawMode] = useState(false);
  const [rawDraft, setRawDraft] = useState("");
  const [runtimeOptions, setRuntimeOptions] = useState<RuntimeOption[]>(FALLBACK_RUNTIME_OPTIONS);
  // Provider override (Option B PR-5): stored separately from config.yaml
  // because the value lives in workspace_secrets (encrypted), not in the
  // platform-managed config.yaml. The two endpoints are GET/PUT
  // /workspaces/:id/provider on workspace-server (handlers/secrets.go).
  // Empty = "auto-derive from model slug prefix" — pre-Option-B behavior
  // and what most users want. Setting to a non-empty value writes
  // LLM_PROVIDER into workspace_secrets and triggers an auto-restart so
  // the workspace boots with the new provider in env (and via CP user-
  // data, written into /configs/config.yaml on next provision too).
  const [provider, setProvider] = useState("");
  const [originalProvider, setOriginalProvider] = useState("");
  // Track the model the form first rendered, so handleSave can detect
  // whether the user actually changed it (vs. only edited tier/skills/etc).
  // Two field sources contribute:
  //   1. wsMetadataModel — workspace_secrets.MODEL_PROVIDER (DB)
  //   2. parsed.runtime_config.model — the template's YAML default
  // Whichever was the live runtime value at load time is what currentModelId
  // will display, and it's the value Save must diff against.
  //
  // Why not just diff the YAML directly: after loadConfig mirrors
  // wsMetadataModel into runtime_config.model for display, the YAML diff
  // is always non-zero on a no-op save, fires PUT /model, and triggers
  // an auto-restart for unrelated edits. Why not diff against
  // wsMetadataModel alone: on a hermes workspace where MODEL_PROVIDER
  // was never set (pre-#240 workspaces, or workspaces created via direct
  // API without going through the picker), wsMetadataModel="" but the
  // form shows the YAML default — diffing against "" makes any first
  // save propagate-and-restart even when the user didn't touch the model.
  // Capturing the actual rendered value covers both.
  const [originalModel, setOriginalModel] = useState("");
  const successTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    return () => clearTimeout(successTimerRef.current);
  }, []);

  const loadConfig = useCallback(async () => {
    setLoading(true);
    setError(null);

    // ALWAYS load workspace metadata first (runtime + model). These are the
    // source of truth regardless of whether the runtime uses our config.yaml
    // template. Without this the form falls back to empty/default values on
    // a hermes workspace (which doesn't use our template), creating the
    // appearance that the saved runtime is unset — and worse, clicking Save
    // would silently flip `runtime` from `hermes` back to the dropdown
    // default `LangGraph`. See GH #1894.
    let wsMetadataRuntime = "";
    let wsMetadataModel = "";
    let wsMetadataTier: number | null = null;
    try {
      const ws = await api.get<{ runtime?: string; tier?: number }>(`/workspaces/${workspaceId}`);
      wsMetadataRuntime = (ws.runtime || "").trim();
      if (typeof ws.tier === "number") wsMetadataTier = ws.tier;
    } catch { /* fall back to config.yaml */ }
    try {
      const m = await api.get<{ model?: string }>(`/workspaces/${workspaceId}/model`);
      wsMetadataModel = (m.model || "").trim();
    } catch { /* non-fatal */ }
    // originalModel is set further down once the YAML has been parsed —
    // we want it to reflect what the form ACTUALLY rendered, which may
    // be the YAML's runtime_config.model fallback when MODEL_PROVIDER
    // is empty. Setting it here from wsMetadataModel alone would be
    // wrong for hermes/pre-#240 workspaces.

    // Load explicit provider override (Option B PR-5). Endpoint returns
    // {provider: "", source: "default"} when no override is set, so the
    // empty string is the legitimate "auto-derive" signal — don't treat
    // it as a load error. Non-fatal: an older workspace-server that
    // predates PR-2 returns 404 here; the form falls back to "" and
    // Save just won't PUT the provider field.
    try {
      const p = await api.get<{ provider?: string }>(`/workspaces/${workspaceId}/provider`);
      const loadedProvider = (p.provider || "").trim();
      setProvider(loadedProvider);
      setOriginalProvider(loadedProvider);
    } catch {
      setProvider("");
      setOriginalProvider("");
    }

    // Skip the config.yaml fetch entirely for runtimes that manage
    // their own config (external, hermes, etc.) — they don't have a
    // platform-side template, so the GET would 404. The catch block
    // below handles 404 gracefully, but issuing the request adds
    // browser-console noise + a wasted RTT on every open of the
    // Config tab for the affected workspaces. Reported on
    // production reno-stars 2026-05-05 (workspace runtime=external,
    // 404 on /files/config.yaml visible in the console even though
    // the form rendered correctly).
    if (RUNTIMES_WITH_OWN_CONFIG.has(wsMetadataRuntime)) {
      setConfig({
        ...DEFAULT_CONFIG,
        runtime: wsMetadataRuntime,
        model: wsMetadataModel,
        ...(wsMetadataModel ? { runtime_config: { model: wsMetadataModel } } : {}),
        ...(wsMetadataTier !== null ? { tier: wsMetadataTier } : {}),
      } as ConfigData);
      setOriginalModel(wsMetadataModel);
      setLoading(false);
      return;
    }
    try {
      const res = await api.get<{ content: string }>(`/workspaces/${workspaceId}/files/config.yaml`);
      const parsed = parseYaml(res.content);
      setOriginalYaml(res.content);
      setRawDraft(res.content);
      // Merge: workspace-row metadata is authoritative for the DB-backed
      // fields (tier, runtime, model). config.yaml often lags — handleSave
      // PATCHes tier/runtime directly and a template snapshot in the
      // container can differ from the live row. Show the DB value so the
      // form doesn't contradict the node badge (issue: badge=T3, form=T2).
      const merged = { ...DEFAULT_CONFIG, ...parsed } as ConfigData;
      if (wsMetadataRuntime) merged.runtime = wsMetadataRuntime;
      if (wsMetadataModel) {
        // Single source of truth: MODEL_PROVIDER (DB) is the live runtime
        // value. Override BOTH top-level + nested runtime_config.model so
        // currentModelId (which reads runtime_config.model first) doesn't
        // silently fall through to the template default. Without the
        // nested override, a workspace deployed with `MiniMax-M2` shows
        // the template's `runtime_config.model: sonnet` in the UI even
        // though the container env (and chat) actually use MiniMax-M2.
        merged.model = wsMetadataModel;
        merged.runtime_config = { ...(merged.runtime_config ?? {}), model: wsMetadataModel };
      }
      if (wsMetadataTier !== null) merged.tier = wsMetadataTier;
      // Snapshot the rendered model so handleSave's diff stays scoped to
      // user-initiated changes. mirrors the read precedence in
      // currentModelId so an unrelated save (tier change) doesn't fire
      // a /model PUT just because MODEL_PROVIDER was empty and the form
      // showed the YAML default.
      setOriginalModel(merged.runtime_config?.model || merged.model || "");
      setConfig(merged);
    } catch {
      // No platform-managed config.yaml. Some runtimes (hermes, external)
      // manage their own config outside this template; that's expected, not
      // an error. Populate the form from workspace metadata so the user
      // still sees the saved runtime + model.
      const runtimeManagesOwnConfig = RUNTIMES_WITH_OWN_CONFIG.has(wsMetadataRuntime);
      if (!runtimeManagesOwnConfig) {
        setError("No config.yaml found");
      }
      setConfig({
        ...DEFAULT_CONFIG,
        runtime: wsMetadataRuntime,
        model: wsMetadataModel,
        // Mirror the merged-path fix above — keep top-level + nested in
        // sync so currentModelId picks up wsMetadataModel even when the
        // form falls into the no-config.yaml branch (hermes/external).
        ...(wsMetadataModel ? { runtime_config: { model: wsMetadataModel } } : {}),
        ...(wsMetadataTier !== null ? { tier: wsMetadataTier } : {}),
      } as ConfigData);
      // Same snapshot as the merged-path branch above. Falls back to
      // empty string when neither MODEL_PROVIDER nor a YAML model was
      // present; handleSave's `nextModel && ...` guard then skips the
      // PUT correctly.
      setOriginalModel(wsMetadataModel);
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    loadConfig();
  }, [loadConfig]);

  useEffect(() => {
    let cancelled = false;
    api.get<Array<{ id: string; name?: string; runtime?: string; models?: ModelSpec[]; providers?: string[] }>>("/templates")
      .then((rows) => {
        if (cancelled || !Array.isArray(rows)) return;
        const byRuntime = new Map<string, RuntimeOption>();
        byRuntime.set("", { value: "", label: "LangGraph (default)", models: [], providers: [] });
        for (const r of rows) {
          const v = (r.runtime || "").trim();
          if (!v || v === "langgraph") continue;
          // Last template wins if two templates share a runtime — rare, and the
          // one with the richer models list is probably newer.
          const existing = byRuntime.get(v);
          const models = Array.isArray(r.models) ? r.models : [];
          const providers = Array.isArray(r.providers) ? r.providers : [];
          if (!existing || models.length > existing.models.length) {
            byRuntime.set(v, { value: v, label: r.name || v, models, providers });
          }
        }
        if (byRuntime.size > 1) setRuntimeOptions(Array.from(byRuntime.values()));
      })
      .catch(() => { /* keep fallback */ });
    return () => { cancelled = true; };
  }, []);

  // Models + env hints for the currently-selected runtime.
  const selectedRuntime = runtimeOptions.find((o) => o.value === (config.runtime || "")) ?? null;
  const availableModels: ModelSpec[] = selectedRuntime?.models ?? [];
  // Provider suggestions for the legacy free-text input fallback (used
  // when /templates returned no models for this runtime, e.g. hermes
  // workspaces). Prefer the runtime's declarative providers list,
  // fall back to deriving from model-slug prefixes.
  const providerSuggestionsList: string[] =
    (selectedRuntime?.providers && selectedRuntime.providers.length > 0)
      ? selectedRuntime.providers
      : deriveProvidersFromModels(availableModels);
  const currentModelId = config.runtime_config?.model || config.model || "";
  const currentModelSpec = availableModels.find((m) => m.id === currentModelId) ?? null;

  // Vendor-aware catalog shared with the selector. Memoised so the
  // catalog identity is stable across renders (selector relies on it).
  const providerCatalog = useMemo(
    () => buildProviderCatalog(availableModels),
    [availableModels],
  );

  // Derive the selector's current value from the form state. Provider
  // back-derivation prefers a vendor-key match against `provider`
  // (Option B explicit override), falling back to the model's vendor
  // bucket when no override is set.
  const selectorValue: SelectorValue = useMemo(() => {
    // 1. Prefer explicit vendor match (workspace_secrets MODEL_PROVIDER).
    if (provider) {
      const byVendor = providerCatalog.find((p) => p.vendor === provider);
      if (byVendor) {
        return {
          providerId: byVendor.id,
          model: currentModelId,
          envVars: byVendor.envVars,
        };
      }
    }
    // 2. Back-derive from model id.
    const matched = findProviderForModel(providerCatalog, currentModelId);
    if (matched) {
      return {
        providerId: matched.id,
        model: currentModelId,
        envVars: matched.envVars,
      };
    }
    // 3. Empty — user hasn't picked yet (or template has no models).
    return { providerId: "", model: currentModelId, envVars: [] };
  }, [provider, currentModelId, providerCatalog]);
  const setSelectorValue = (_next: SelectorValue) => {
    // Selector emits `next`; the actual writes happen in the onChange
    // handler in JSX which calls setConfig + setProvider directly.
    // This setter exists only to satisfy ProviderModelSelector's
    // controlled-component contract (it always re-derives from props
    // so the no-op identity is fine).
    void _next;
  };

  const update = <K extends keyof ConfigData>(key: K, value: ConfigData[K]) => {
    setConfig((prev) => ({ ...prev, [key]: value }));
  };

  const updateNested = <K extends keyof ConfigData>(key: K, subKey: string, value: unknown) => {
    setConfig((prev) => ({
      ...prev,
      [key]: { ...(prev[key] as Record<string, unknown>), [subKey]: value },
    }));
  };

  const handleSave = async (restart: boolean) => {
    setSaving(true);
    setError(null);
    setSuccess(false);
    try {
      const content = rawMode ? rawDraft : toYaml(config);
      const runtimeManagesOwnConfig = RUNTIMES_WITH_OWN_CONFIG.has(config.runtime || "");
      // Only write the platform-managed config.yaml when the runtime
      // actually consumes it. Hermes + external runtimes manage their
      // own config file inside the container, so writing this one is a
      // no-op at best and can fail with 404 if config.yaml was never
      // created for this workspace.
      if (!runtimeManagesOwnConfig) {
        await api.put(`/workspaces/${workspaceId}/files/config.yaml`, { content });
      }

      // DB-backed fields (name, tier, runtime, model) live on the
      // workspace row, NOT in config.yaml. Fire separate PATCHes for
      // the ones that actually changed — otherwise a Hermes user edits
      // the form, hits Save, sees the request succeed, then watches the
      // values snap back on the next reload because the workspace row
      // never heard about the change.
      //
      // Diff against the RAW parsed YAML (or the form `config` in non-
      // raw mode) rather than the DEFAULT_CONFIG-merged shape — if the
      // user deleted a field in raw mode the merge would substitute the
      // default (e.g. tier=1) and we'd silently PATCH that down from
      // the stored value. Only fields the user actually typed get sent.
      const oldParsed = parseYaml(originalYaml);
      const nextSource = rawMode
        ? (parseYaml(rawDraft) as Record<string, unknown>)
        : (config as unknown as Record<string, unknown>);
      const dbPatch: Record<string, unknown> = {};
      if (typeof nextSource.name === "string" && nextSource.name && nextSource.name !== oldParsed.name) {
        dbPatch.name = nextSource.name;
      }
      if (typeof nextSource.tier === "number" && nextSource.tier !== (oldParsed.tier ?? null)) {
        dbPatch.tier = nextSource.tier;
      }
      const oldRuntime = (oldParsed.runtime as string) || "";
      if (typeof nextSource.runtime === "string" && nextSource.runtime && nextSource.runtime !== oldRuntime) {
        dbPatch.runtime = nextSource.runtime;
      }
      if (Object.keys(dbPatch).length > 0) {
        await api.patch(`/workspaces/${workspaceId}`, dbPatch);
        // Mirror the DB write into the canvas store node data so the
        // header pill (TIER T2/T3, RUNTIME claude-code/hermes) and the
        // node card update immediately. Without this push, the workspace
        // row reflects the new tier but every UI surface that reads from
        // useCanvasStore.nodes (header badge, ContextMenu, etc.) keeps
        // showing the stale value until the next full hydrate. Bug
        // surfaced 2026-05-03 — user picked T3, hit Save & Restart,
        // database said tier=3, badge still said T2.
        useCanvasStore.getState().updateNodeData(workspaceId, dbPatch);
      }

      // Model has its own endpoint (separate from the general workspace
      // PATCH) because the runtime may need to validate it against the
      // template's supported models list. A model rejection is a
      // partial-save state — we report it as a user-visible warning
      // rather than lying "Saved" and letting the user discover the
      // revert on next reload.
      //
      // Read from runtime_config.model first, then fall back to top-level
      // model. The dropdown's onChange (above, ~line 475) writes to
      // runtime_config.model whenever a runtime is selected (hermes,
      // claude-code, etc.) and only falls back to top-level model when
      // there's no runtime. handleSave used to diff against top-level
      // model only, so for any runtime-bearing workspace the user's
      // model selection never persisted — they'd Save & Restart, the
      // EC2 would boot with HERMES_DEFAULT_MODEL empty, and hermes
      // would fall back to nousresearch/hermes-4-70b → "No LLM provider
      // configured" error in the chat. Caught 2026-04-30 on hongmingwang
      // hermes workspace 32993ee7-…cb9d75d112a5.
      const nextModelRaw = (nextSource.runtime_config as Record<string, unknown> | undefined)?.model;
      const nextModel =
        typeof nextModelRaw === "string" && nextModelRaw
          ? nextModelRaw
          : typeof nextSource.model === "string"
            ? nextSource.model
            : "";
      // Diff against the loaded MODEL_PROVIDER (the runtime source of
      // truth), not the YAML's runtime_config.model. After loadConfig
      // mirrors wsMetadataModel into runtime_config.model for display,
      // nextModel always equals the loaded value on a no-op save —
      // diffing against oldModelRaw (the unmirrored YAML default) would
      // make every Save fire a /model PUT and trigger an auto-restart,
      // even when the user only changed an unrelated field. Comparing
      // against `originalModel` keeps the PUT scoped to actual user
      // intent.
      let modelSaveError: string | null = null;
      if (nextModel && nextModel !== originalModel) {
        try {
          await api.put(`/workspaces/${workspaceId}/model`, { model: nextModel });
          setOriginalModel(nextModel);
        } catch (e) {
          modelSaveError = e instanceof Error ? e.message : "Model update was rejected";
        }
      }

      // Provider override save (Option B PR-5). PUT only when the user
      // changed the dropdown — otherwise an unrelated Save (e.g. tier
      // edit) would re-write the provider unchanged and the server-
      // side auto-restart would fire on every Save, costing the user a
      // ~30s reboot for a no-op change. Server endpoint accepts an
      // empty string to clear the override (deletes the
      // workspace_secrets row); we forward whatever the form holds.
      let providerSaveError: string | null = null;
      const providerChanged = provider !== originalProvider;
      if (providerChanged) {
        try {
          await api.put(`/workspaces/${workspaceId}/provider`, { provider });
          setOriginalProvider(provider);
        } catch (e) {
          providerSaveError = e instanceof Error ? e.message : "Provider update was rejected";
        }
      }

      setOriginalYaml(content);
      if (rawMode) {
        const parsed = parseYaml(content);
        setConfig({ ...DEFAULT_CONFIG, ...parsed } as ConfigData);
      } else {
        setRawDraft(content);
      }
      // SetProvider on the server already triggers an auto-restart for
      // the workspace whenever the value actually changed (see
      // workspace-server/internal/handlers/secrets.go:SetProvider). If
      // the user also clicked Save+Restart we'd kick off a SECOND
      // restart here and the two would race in the canvas store —
      // suppress the redundant call and rely on the server-side one.
      const providerWillAutoRestart = providerChanged && !providerSaveError;
      if (restart && !providerWillAutoRestart) {
        await useCanvasStore.getState().restartWorkspace(workspaceId);
      } else if (!restart) {
        useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: !providerWillAutoRestart });
      }
      // Aggregate partial-save errors. Both modelSaveError and
      // providerSaveError describe rejected updates from independent
      // endpoints — show whichever fired so the user knows which
      // field reverts on next reload (otherwise they'd see "Saved" and
      // be confused why Provider snapped back).
      const partialError = providerSaveError
        ? `Other fields saved, but provider update failed: ${providerSaveError}`
        : modelSaveError
          ? `Other fields saved, but model update failed: ${modelSaveError}`
          : null;
      if (partialError) {
        setError(partialError);
      } else {
        setSuccess(true);
        clearTimeout(successTimerRef.current);
        successTimerRef.current = setTimeout(() => setSuccess(false), 2000);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  // Stable IDs for bare label↔control pairs (WCAG 1.3.1)
  const descriptionId = useId();
  const tierId = useId();
  const runtimeId = useId();
  const effortId = useId();
  const taskBudgetId = useId();
  const sandboxBackendId = useId();

  const providerDirty = provider !== originalProvider;
  const isDirty = (rawMode ? rawDraft !== originalYaml : toYaml(config) !== originalYaml) || providerDirty;

  if (loading) {
    return <div className="p-4 text-xs text-ink-soft">Loading config...</div>;
  }

  return (
    <div className="flex flex-col h-full">
      {/* Mode toggle */}
      <div className="flex items-center justify-between px-3 py-1.5 border-b border-line/40 bg-surface-sunken/30">
        <span className="text-[10px] text-ink-soft">config.yaml</span>
        <label className="flex items-center gap-1.5 cursor-pointer">
          <span className="text-[9px] text-ink-soft">Raw YAML</span>
          <input
            type="checkbox"
            checked={rawMode}
            onChange={(e) => {
              if (e.target.checked) {
                setRawDraft(toYaml(config));
              } else {
                const parsed = parseYaml(rawDraft);
                setConfig({ ...DEFAULT_CONFIG, ...parsed } as ConfigData);
              }
              setRawMode(e.target.checked);
            }}
            className="accent-blue-500"
          />
        </label>
      </div>

      {rawMode ? (
        <div className="flex-1 p-3">
          <textarea
            aria-label="Raw YAML editor"
            value={rawDraft}
            onChange={(e) => setRawDraft(e.target.value)}
            spellCheck={false}
            className="w-full h-full min-h-[300px] bg-surface-card border border-line rounded p-3 text-xs font-mono text-ink focus:outline-none focus:border-accent resize-none"
          />
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto p-3 space-y-2">
          <Section title="General">
            <TextInput label="Name" value={config.name} onChange={(v) => update("name", v)} />
            <div>
              <label htmlFor={descriptionId} className="text-[10px] text-ink-soft block mb-1">Description</label>
              <textarea
                id={descriptionId}
                value={config.description}
                onChange={(e) => update("description", e.target.value)}
                rows={3}
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent resize-none"
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <TextInput label="Version" value={config.version} onChange={(v) => update("version", v)} mono />
              <div>
                <label htmlFor={tierId} className="text-[10px] text-ink-soft block mb-1">Tier</label>
                <select
                  id={tierId}
                  value={config.tier}
                  onChange={(e) => update("tier", parseInt(e.target.value, 10))}
                  className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
                >
                  <option value={1}>T1 — Sandboxed</option>
                  <option value={2}>T2 — Standard</option>
                  <option value={3}>T3 — Privileged</option>
                  <option value={4}>T4 — Full Access</option>
                </select>
              </div>
            </div>
          </Section>

          <Section title="Runtime">
            <div>
              <label htmlFor={runtimeId} className="text-[10px] text-ink-soft block mb-1">Runtime</label>
              <select
                id={runtimeId}
                value={config.runtime || ""}
                onChange={(e) => update("runtime", e.target.value)}
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
              >
                {runtimeOptions.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
            {/* Shared Provider→Model selector. Same component renders in
                MissingKeysModal (deploy onboarding) so the dropdown UX is
                identical across all three surfaces. Provider field maps
                back into the workspace_secrets MODEL_PROVIDER override
                — empty = "auto-derive from model slug" was the pre-PR-5
                behavior; selecting any provider here writes LLM_PROVIDER
                and triggers an auto-restart. */}
            {availableModels.length > 0 ? (
              <ProviderModelSelector
                models={availableModels}
                value={selectorValue}
                onChange={(next) => {
                  setSelectorValue(next);
                  // Mirror selection into the config object the rest of
                  // the form / save handler still reads. Model lands in
                  // runtime_config.model when a runtime is set, else
                  // top-level model. required_env follows the selected
                  // provider's envVars when the existing required_env
                  // was template-driven (don't clobber user-typed envs).
                  setConfig((prev) => {
                    const v = next.model;
                    const prevModelId = prev.runtime_config?.model || prev.model || "";
                    const prevSpec = availableModels.find((m) => m.id === prevModelId) ?? null;
                    const prevRequired = prev.runtime_config?.required_env ?? [];
                    const wasTemplateDriven =
                      prevRequired.length === 0 ||
                      (prevSpec?.required_env?.length
                        ? prevRequired.length === prevSpec.required_env.length &&
                          prevRequired.every((e, i) => e === prevSpec.required_env![i])
                        : false);
                    const nextRequired =
                      next.envVars.length > 0 && wasTemplateDriven
                        ? next.envVars
                        : prevRequired;
                    if (prev.runtime) {
                      return {
                        ...prev,
                        runtime_config: {
                          ...prev.runtime_config,
                          model: v,
                          ...(next.envVars.length > 0 && wasTemplateDriven
                            ? { required_env: nextRequired }
                            : {}),
                        },
                      };
                    }
                    return { ...prev, model: v };
                  });
                  // Map vendor → workspace_secrets MODEL_PROVIDER value.
                  // Hermes-agent derive-provider.sh is the canonical
                  // recogniser, but we approximate by emitting the
                  // catalog vendor key (which matches our hermes
                  // provider taxonomy 1:1 for the slugs we ship).
                  if (next.providerId) {
                    const entry = providerCatalog.find((p) => p.id === next.providerId);
                    if (entry) setProvider(entry.vendor);
                  } else {
                    setProvider("");
                  }
                }}
                variant="grid"
                idPrefix={runtimeId}
                allowCustomModelEscape
              />
            ) : (
              // Fallback when /templates didn't surface any models for
              // this runtime — e.g. hermes workspaces that manage their
              // own ~/.hermes/config.yaml. Power-user free-text inputs
              // for both fields. Provider here writes through to the
              // workspace_secrets MODEL_PROVIDER override.
              <div className="space-y-3">
                <div>
                  <label className="text-[10px] text-ink-soft block mb-1">Model</label>
                  <input
                    type="text"
                    value={currentModelId}
                    onChange={(e) => {
                      const v = e.target.value;
                      setConfig((prev) =>
                        prev.runtime
                          ? { ...prev, runtime_config: { ...prev.runtime_config, model: v } }
                          : { ...prev, model: v },
                      );
                    }}
                    placeholder="e.g. anthropic:claude-sonnet-4-6"
                    className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink font-mono focus:outline-none focus:border-accent"
                  />
                </div>
                <div>
                  <label htmlFor={`${runtimeId}-provider`} className="text-[10px] text-ink-soft block mb-1">
                    Provider
                    <span className="ml-1 text-ink-soft">
                      (override — leave empty to auto-derive from model slug)
                    </span>
                  </label>
                  <input
                    id={`${runtimeId}-provider`}
                    type="text"
                    list={
                      providerSuggestionsList.length > 0
                        ? `${runtimeId}-providers`
                        : undefined
                    }
                    value={provider}
                    onChange={(e) => setProvider(e.target.value.trim())}
                    placeholder={
                      providerSuggestionsList.length > 0
                        ? `e.g. ${providerSuggestionsList.slice(0, 3).join(", ")} (empty = auto-derive)`
                        : "empty = auto-derive from model slug"
                    }
                    aria-label="LLM provider override"
                    data-testid="provider-input"
                    className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink font-mono focus:outline-none focus:border-accent"
                  />
                  {providerSuggestionsList.length > 0 && (
                    <datalist id={`${runtimeId}-providers`}>
                      {providerSuggestionsList.map((p) => (
                        <option key={p} value={p} />
                      ))}
                    </datalist>
                  )}
                </div>
              </div>
            )}
            {provider && provider !== originalProvider && (
              <p className="text-[10px] text-warm mt-1">
                Provider change → workspace will auto-restart on Save.
              </p>
            )}
            <TagList
              label={
                currentModelSpec?.required_env?.length &&
                arraysEqual(config.runtime_config?.required_env ?? [], currentModelSpec.required_env)
                  ? "Required Env Var Names (from template)"
                  : "Required Env Var Names"
              }
              values={config.runtime_config?.required_env ?? []}
              onChange={(v) => updateNested("runtime_config" as keyof ConfigData, "required_env", v)}
              placeholder="variable NAME (e.g. ANTHROPIC_API_KEY) — not the value"
            />
            <p className="text-[10px] text-ink-soft mt-1">
              This declares which env var <em>names</em> the workspace needs.
              Set the actual values in the <strong>Secrets</strong> section
              below — those are encrypted and mounted into the container at
              runtime.
            </p>
            {currentModelSpec?.required_env?.length &&
              !arraysEqual(config.runtime_config?.required_env ?? [], currentModelSpec.required_env) && (
              <div className="text-[10px] text-ink-soft mt-1 flex items-center gap-2">
                <span>
                  Template suggests{" "}
                  <code className="text-ink-mid">{currentModelSpec.required_env.join(", ")}</code>{" "}
                  for <code className="text-ink-mid">{currentModelSpec.name || currentModelSpec.id}</code>.
                </span>
                <button
                  type="button"
                  onClick={() => updateNested("runtime_config" as keyof ConfigData, "required_env", currentModelSpec.required_env)}
                  className="text-accent hover:text-accent underline"
                >
                  Apply
                </button>
              </div>
            )}
          </Section>

          {/* Claude Settings — shown for claude-code runtime or claude/anthropic model names */}
          {(config.runtime === "claude-code" ||
            (config.runtime_config?.model || config.model || "").toLowerCase().includes("claude") ||
            (config.runtime_config?.model || config.model || "").toLowerCase().includes("anthropic")) && (
            <Section title="Claude Settings" defaultOpen={false}>
              <div>
                <label htmlFor={effortId} className="text-[10px] text-ink-soft block mb-1">
                  Effort
                  <span className="ml-1 text-ink-soft">(output_config.effort — Opus 4.7+)</span>
                </label>
                <select
                  id={effortId}
                  value={config.effort || ""}
                  onChange={(e) => update("effort", e.target.value)}
                  className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
                  data-testid="effort-select"
                >
                  <option value="">— unset (model default) —</option>
                  <option value="low">low</option>
                  <option value="medium">medium</option>
                  <option value="high">high</option>
                  <option value="xhigh">xhigh (extended thinking)</option>
                  <option value="max">max — absolute ceiling</option>
                </select>
              </div>
              <div>
                <label htmlFor={taskBudgetId} className="text-[10px] text-ink-soft block mb-1">
                  Task Budget (tokens)
                  <span className="ml-1 text-ink-soft">(output_config.task_budget.total — 0 = unset)</span>
                </label>
                <input
                  id={taskBudgetId}
                  type="number"
                  min={0}
                  step={1000}
                  value={config.task_budget ?? 0}
                  onChange={(e) => update("task_budget", parseInt(e.target.value, 10) || 0)}
                  placeholder="0"
                  className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent font-mono"
                  data-testid="task-budget-input"
                />
              </div>
            </Section>
          )}

          {/* Skills + Tools used to live here as TagList inputs. They were
              redundant with their dedicated tabs:
              - Skills → managed via SkillsTab (per-workspace skill folders)
              - Tools  → managed via the Plugins tab (install/uninstall)
              Editing them here only set the config.yaml field; the
              actual install/load happened elsewhere. Removed to stop
              showing the misnamed list-input affordance. */}

          <Section title="Prompt Files" defaultOpen={false}>
            <p className="text-[10px] text-ink-soft px-1 pb-1">
              Markdown files that compose this workspace&apos;s system prompt.
              Loaded in order at boot from the workspace config dir
              (e.g. <code className="font-mono">system-prompt.md</code>,{' '}
              <code className="font-mono">CLAUDE.md</code>,{' '}
              <code className="font-mono">AGENTS.md</code>). Edit the file
              contents directly via the Files tab.
            </p>
            <TagList label="Files (load order)" values={config.prompt_files || []} onChange={(v) => update("prompt_files", v)} placeholder="e.g. system-prompt.md" />
          </Section>

          <Section title="A2A Protocol" defaultOpen={false}>
            <NumberInput label="Port" value={config.a2a?.port ?? 8000} onChange={(v) => updateNested("a2a" as keyof ConfigData, "port", v)} />
            <Toggle label="Streaming" checked={config.a2a?.streaming ?? true} onChange={(v) => updateNested("a2a" as keyof ConfigData, "streaming", v)} />
            <Toggle label="Push Notifications" checked={config.a2a?.push_notifications ?? true} onChange={(v) => updateNested("a2a" as keyof ConfigData, "push_notifications", v)} />
          </Section>

          <Section title="Delegation" defaultOpen={false}>
            <div className="grid grid-cols-2 gap-3">
              <NumberInput label="Retry Attempts" value={config.delegation?.retry_attempts ?? 3} onChange={(v) => updateNested("delegation" as keyof ConfigData, "retry_attempts", v)} min={0} max={10} />
              <NumberInput label="Retry Delay (s)" value={config.delegation?.retry_delay ?? 5} onChange={(v) => updateNested("delegation" as keyof ConfigData, "retry_delay", v)} min={1} />
            </div>
            <NumberInput label="Timeout (s)" value={config.delegation?.timeout ?? 120} onChange={(v) => updateNested("delegation" as keyof ConfigData, "timeout", v)} min={10} />
            <Toggle label="Escalate on failure" checked={config.delegation?.escalate ?? true} onChange={(v) => updateNested("delegation" as keyof ConfigData, "escalate", v)} />
          </Section>

          <Section title="Sandbox" defaultOpen={false}>
            <div>
              <label htmlFor={sandboxBackendId} className="text-[10px] text-ink-soft block mb-1">Backend</label>
              <select
                id={sandboxBackendId}
                value={config.sandbox?.backend || "docker"}
                onChange={(e) => updateNested("sandbox" as keyof ConfigData, "backend", e.target.value)}
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
              >
                <option value="subprocess">subprocess</option>
                <option value="docker">docker</option>
                <option value="e2b">e2b</option>
              </select>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <TextInput label="Memory Limit" value={config.sandbox?.memory_limit || "256m"} onChange={(v) => updateNested("sandbox" as keyof ConfigData, "memory_limit", v)} mono />
              <NumberInput label="Timeout (s)" value={config.sandbox?.timeout ?? 30} onChange={(v) => updateNested("sandbox" as keyof ConfigData, "timeout", v)} min={5} />
            </div>
          </Section>

          <SecretsSection
            workspaceId={workspaceId}
            requiredEnv={config.runtime_config?.required_env}
          />

          <AgentCardSection workspaceId={workspaceId} />
        </div>
      )}

      {error && (
        <div className="mx-3 mb-2 px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">{error}</div>
      )}
      {!error && RUNTIMES_WITH_OWN_CONFIG.has(config.runtime || "") && (
        <div className="mx-3 mb-2 px-3 py-1.5 bg-surface-sunken/50 border border-line rounded text-xs text-ink-mid">
          {config.runtime === "hermes"
            ? "Hermes manages its own config at ~/.hermes/config.yaml on the workspace host. Edit it via the Terminal tab or the hermes CLI, not this form."
            : "This runtime manages its own config outside the platform template."}
        </div>
      )}
      {!error && config.runtime === "external" && (
        <ExternalConnectionSection workspaceId={workspaceId} />
      )}
      {success && (
        <div className="mx-3 mb-2 px-3 py-1.5 bg-green-900/30 border border-green-800 rounded text-xs text-good">Saved</div>
      )}

      <div className="p-3 border-t border-line flex gap-2">
        <button
          type="button"
          onClick={() => handleSave(true)}
          disabled={!isDirty || saving}
          // Same accent-LIGHTER fix shipped on every other tab.
          className="px-3 py-1.5 bg-accent hover:bg-accent-strong text-xs rounded text-white disabled:opacity-30 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
        >
          {saving ? "Restarting..." : "Save & Restart"}
        </button>
        <button
          type="button"
          onClick={() => handleSave(false)}
          disabled={!isDirty || saving}
          className="px-3 py-1.5 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid disabled:opacity-30 transition-colors"
        >
          Save
        </button>
        <button
          type="button"
          onClick={loadConfig}
          className="px-3 py-1.5 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid ml-auto"
        >
          Reload
        </button>
      </div>
    </div>
  );
}
