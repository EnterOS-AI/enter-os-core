"use client";

import { useState, useEffect, useRef, useCallback, useId, useMemo } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { api } from "@/lib/api";
import {
  FALLBACK_COMPUTE_OPTIONS,
  type ComputeOptions,
  defaultInstanceForProvider,
  displayDefaultForProvider,
  instanceTypesForProvider,
  parseComputeOptions,
} from "@/lib/compute-options";
import { isSaaSTenant } from "@/lib/tenant";
import { ExternalConnectModal, type ExternalConnectionInfo } from "./ExternalConnectModal";
import {
  ProviderModelSelector,
  buildProviderCatalog,
  buildProviderCatalogFromRegistry,
  findProviderForModel,
  isPlatformManagedProvider,
  type SelectorModel,
  type SelectorValue,
  type RegistryProvider,
  type RegistryModel,
} from "./ProviderModelSelector";

interface WorkspaceOption {
  id: string;
  name: string;
  tier: number;
}

// Subset of the /templates row used here. Mirrors the shape ConfigTab
// reads. `providers` is the per-template declarative list of supported
// LLM providers — sourced from the template's
// runtime_config.providers (config.yaml). When present, it filters
// the modal's provider <select> so an operator can only pick a
// provider the template actually supports.
interface TemplateSpec {
  id: string;
  name?: string;
  runtime?: string;
  model?: string;
  models?: SelectorModel[];
  providers?: string[];
  // internal#718 P3 registry-served fields (additive; absent on older
  // backends and for non-registry runtimes). When registry_backed is true the
  // provider→model catalog is built from registry_providers/registry_models so
  // each model's DERIVED provider (e.g. moonshot/kimi-k2.6 → "platform") drives
  // the dropdown bucket and the create payload's llm_provider — instead of the
  // legacy inferVendor heuristic that slash-splits the id into "moonshot".
  // Mirrors ConfigTab's RuntimeOption loader (RFC#340 Fix C).
  registry_backed?: boolean;
  registry_providers?: RegistryProvider[];
  registry_models?: RegistryModel[];
}

const DEFAULT_RUNTIME = "claude-code";

// Human-readable labels for cloud-provider IDs. Provider ordering and
// instance-type allowlists are SSOT from GET /compute/metadata; labels are
// pure UI chrome and live next to the only consumer (#2489).
const PROVIDER_LABELS: Record<string, string> = {
  aws: "AWS (default)",
  gcp: "GCP",
  hetzner: "Hetzner",
};
const providerLabel = (id: string): string => PROVIDER_LABELS[id] ?? id;

const RUNTIME_OPTIONS = [
  { value: "claude-code", label: "Claude Code" },
  { value: "codex", label: "OpenAI Codex CLI" },
  { value: "google-adk", label: "Google ADK" },
  { value: "hermes", label: "Hermes" },
  { value: "openclaw", label: "OpenClaw" },
];
const BASE_RUNTIME_TEMPLATE_IDS = new Set(["claude-code-default", "codex", "google-adk", "hermes", "openclaw"]);
const DEFAULT_HEADLESS_ROOT_GB = 30;
const DEFAULT_DISPLAY_ROOT_GB = 80;

export function CreateWorkspaceButton() {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [role, setRole] = useState("");
  const [runtime, setRuntime] = useState(DEFAULT_RUNTIME);
  const [template, setTemplate] = useState("");
  const [parentId, setParentId] = useState("");
  const [budgetLimit, setBudgetLimit] = useState("");
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [workspaces, setWorkspaces] = useState<WorkspaceOption[]>([]);
  const [displayEnabled, setDisplayEnabled] = useState(false);
  const [displayInstanceType, setDisplayInstanceType] = useState(
    displayDefaultForProvider(FALLBACK_COMPUTE_OPTIONS),
  );
  const [displayRootGB, setDisplayRootGB] = useState(String(DEFAULT_DISPLAY_ROOT_GB));
  const [displayResolution, setDisplayResolution] = useState("1920x1080");
  // Cloud/compute backend for the workspace box (multi-provider, per-workspace).
  // "aws" default; "gcp"/"hetzner" route to the matching CP WorkspaceProvisioner
  // (a non-tenant-cloud box is reached over a per-workspace tunnel, runtime#95).
  const [cloudProvider, setCloudProvider] = useState("aws");
  // SSOT provider + instance-type metadata from GET /compute/metadata. Starts from
  // the offline fallback and is replaced once the fetch resolves; on fetch error we
  // keep the fallback so the dialog stays usable.
  const [computeOptions, setComputeOptions] = useState<ComputeOptions>(FALLBACK_COMPUTE_OPTIONS);
  // Templates fetched from /api/templates — drives the dynamic provider
  // filter below. Same data source ConfigTab uses (PR #2454). When the
  // selected template declares `runtime_config.providers` in its
  // config.yaml, the modal surfaces only those providers in the
  // <select>. Provider/model options are derived from template models.
  const [templateSpecs, setTemplateSpecs] = useState<TemplateSpec[]>([]);

  // Keep the selected display instance type valid when the cloud provider or SSOT
  // options change. If the current value is not offered for the provider, fall back
  // to the provider's SSOT display default.
  useEffect(() => {
    const valid = instanceTypesForProvider(computeOptions, cloudProvider);
    if (!valid.includes(displayInstanceType)) {
      setDisplayInstanceType(displayDefaultForProvider(computeOptions, cloudProvider));
    }
  }, [cloudProvider, computeOptions, displayInstanceType]);
  // External-runtime path: skip docker provision, mint a workspace_auth_token,
  // and surface the connection snippet in a modal after create. When
  // isExternal is true the template and model fields are hidden (they're
  // meaningless for BYO-compute agents).
  const [isExternal, setIsExternal] = useState(false);
  const [externalRuntime, setExternalRuntime] = useState("external");
  const [externalConnection, setExternalConnection] =
    useState<ExternalConnectionInfo | null>(null);

  const [llmSelection, setLLMSelection] = useState<SelectorValue>({
    providerId: "",
    model: "",
    envVars: [],
  });
  const [llmSecret, setLLMSecret] = useState("");

  // Tier picker: on SaaS every workspace gets its own EC2 VM (Full Access
  // by construction), so we hide the T1/T2/T3 Docker-sandbox tiers and
  // lock to T4 — the full-host access tier. The EC2 size is controlled by
  // the compute profile below. On self-hosted we still offer T1/T2/T3
  // because the Docker-
  // sandbox distinction is a real choice there; T4 is available too for
  // operators who want the full-host tier.
  //
  // SSR-safe via isSaaSTenant() contract (returns false on server); first
  // client render may flip the picker — acceptable one-frame reflow.
  const isSaaS = useMemo(() => isSaaSTenant(), []);
  const TIERS = useMemo(
    () =>
      isSaaS
        ? [{ value: 4, label: "T4", desc: "Full Access" }]
        : [
            { value: 1, label: "T1", desc: "Sandboxed" },
            { value: 2, label: "T2", desc: "Standard" },
            { value: 3, label: "T3", desc: "Privileged" },
            { value: 4, label: "T4", desc: "Full Access" },
          ],
    [isSaaS],
  );
  // T3 ("Privileged") is the self-hosted default — gives agents the
  // read_write workspace mount + Docker daemon access most templates
  // expect to do real work. T1 sandboxed and T2 standard are kept as
  // explicit opt-ins for low-trust agents. SaaS still defaults to T4
  // because every SaaS workspace gets its own EC2 (sibling VMs, no
  // shared blast radius — see isSaaSTenant() / tier picker hide logic).
  const defaultTier = isSaaS ? 4 : 3;
  const [tier, setTier] = useState(defaultTier);

  // Refs for roving tabIndex on the tier radio group (WCAG 2.1 arrow-key nav)
  const radioRefs = useRef<Array<HTMLButtonElement | null>>([]);

  const handleRadioKeyDown = useCallback(
    (e: React.KeyboardEvent, currentIndex: number) => {
      if (e.key === "ArrowDown" || e.key === "ArrowRight") {
        e.preventDefault();
        const next = (currentIndex + 1) % TIERS.length;
        setTier(TIERS[next].value);
        radioRefs.current[next]?.focus();
      } else if (e.key === "ArrowUp" || e.key === "ArrowLeft") {
        e.preventDefault();
        const prev = (currentIndex - 1 + TIERS.length) % TIERS.length;
        setTier(TIERS[prev].value);
        radioRefs.current[prev]?.focus();
      }
    },
    // TIERS is stable (module-level constant pattern), setTier is stable from useState
    // eslint-disable-next-line react-hooks/exhaustive-deps
    []
  );

  const handleRuntimeChange = useCallback((nextRuntime: string) => {
    setRuntime(nextRuntime);
    setTemplate("");
    setLLMSelection({ providerId: "", model: "", envVars: [] });
    setLLMSecret("");
  }, []);

  // Resolve the selected workspace template from /templates. Runtime is
  // deliberately separate: "SEO Agent" is a workspace template, not a
  // runtime, so it must never appear in the runtime selector.
  const selectedTemplateSpec = useMemo<TemplateSpec | null>(() => {
    if (!template) return null;
    return templateSpecs.find((s) => s.id === template) ?? null;
  }, [template, templateSpecs]);
  const selectedRuntimeTemplateSpec = useMemo<TemplateSpec | null>(() => (
    templateSpecs.find((s) => {
      if (!BASE_RUNTIME_TEMPLATE_IDS.has(s.id)) return false;
      const specRuntime = (s.runtime ?? s.id).trim().toLowerCase();
      return s.id === runtime || specRuntime === runtime;
    }) ?? null
  ), [runtime, templateSpecs]);
  const visibleTemplateSpecs = useMemo(
    () => templateSpecs.filter((spec) => {
      if (BASE_RUNTIME_TEMPLATE_IDS.has(spec.id)) return false;
      const specRuntime = (spec.runtime ?? DEFAULT_RUNTIME).trim().toLowerCase();
      return specRuntime === runtime;
    }),
    [runtime, templateSpecs],
  );
  // The /templates row backing the LLM picker: an explicitly-selected
  // workspace template wins, else the base runtime template row.
  const llmSourceSpec = useMemo<TemplateSpec | null>(
    () => selectedTemplateSpec ?? selectedRuntimeTemplateSpec,
    [selectedRuntimeTemplateSpec, selectedTemplateSpec],
  );
  // internal#718 P3 / RFC#340 Fix C: a runtime is registry-backed when the
  // /templates row says so AND it served a non-empty registry_models set.
  // Mirrors ConfigTab's `registryBacked` derivation exactly.
  const registryBacked = useMemo(
    () =>
      llmSourceSpec?.registry_backed === true &&
      (llmSourceSpec.registry_models?.length ?? 0) > 0,
    [llmSourceSpec],
  );
  // Models fed to the selector dropdown. For a registry-backed runtime use the
  // registry-served native set, carrying each model's DERIVED provider so the
  // selector buckets it correctly (moonshot/kimi-k2.6 → "platform", not the
  // inferVendor "moonshot"). Otherwise fall back to the template-served
  // models[] + the legacy heuristic — same fallback ConfigTab keeps.
  const llmModels = useMemo<SelectorModel[]>(
    () => {
      if (registryBacked) {
        return (llmSourceSpec?.registry_models ?? []).map((m) => ({
          id: m.id,
          name: m.name,
          ...(m.provider ? { provider: m.provider } : {}),
        }));
      }
      return llmSourceSpec?.models?.length ? llmSourceSpec.models : [];
    },
    [registryBacked, llmSourceSpec],
  );
  // Registry-backed path: build the catalog from registry_providers/
  // registry_models so dropdown labels + billing + the derived provider come
  // from the provider-registry SSOT (restores the "Platform" bucket). Legacy
  // path: re-infer from models[] via buildProviderCatalog (inferVendor).
  const llmCatalog = useMemo(
    () =>
      registryBacked
        ? buildProviderCatalogFromRegistry(
            llmSourceSpec?.registry_providers ?? [],
            llmSourceSpec?.registry_models ?? [],
          )
        : buildProviderCatalog(llmModels),
    [registryBacked, llmSourceSpec, llmModels],
  );
  const selectedLLMProvider = useMemo(
    () => llmCatalog.find((p) => p.id === llmSelection.providerId) ?? llmCatalog[0],
    [llmCatalog, llmSelection.providerId],
  );

  useEffect(() => {
    if (llmCatalog.length === 0) return;
    const sourceDefault = llmSourceSpec?.model?.trim();
    const platformProvider = llmCatalog.find((p) => p.vendor === "platform");
    const matched = sourceDefault ? findProviderForModel(llmCatalog, sourceDefault) : null;
    const next = platformProvider ?? matched ?? llmCatalog[0];
    const defaultModel = next.models.find((model) => model.id === sourceDefault)?.id
      ?? next.models[0]?.id
      ?? "";
    setLLMSelection({
      providerId: next.id,
      model: next.wildcard ? "" : defaultModel,
      envVars: next.envVars,
    });
    setLLMSecret("");
  }, [llmCatalog, llmSourceSpec]);

  // Reset form and load workspaces whenever dialog opens
  useEffect(() => {
    if (!open) return;
    setName("");
    setRole("");
    setTier(defaultTier);
    setRuntime(DEFAULT_RUNTIME);
    setTemplate("");
    setParentId("");
    setBudgetLimit("");
    setError(null);
    setDisplayEnabled(false);
    setDisplayRootGB(String(DEFAULT_DISPLAY_ROOT_GB));
    setDisplayResolution("1920x1080");
    setExternalRuntime("external");
    setLLMSelection({ providerId: "", model: "", envVars: [] });
    setLLMSecret("");

    api
      .get<WorkspaceOption[]>("/workspaces")
      .then((ws) => setWorkspaces(ws))
      .catch(() => {});
    api
      .get<TemplateSpec[]>("/templates")
      .then((rows) => setTemplateSpecs(Array.isArray(rows) ? rows : []))
      .catch(() => { /* keep empty; create stays blocked until the catalog loads */ });

    // Load SSOT compute metadata, then reset provider + display defaults from it.
    // We fetch fresh each time the dialog opens because the server SSOT can change
    // without a page reload.
    api
      .get<unknown>("/compute/metadata")
      .then((resp) => {
        const parsed = parseComputeOptions(resp);
        if (parsed) {
          setComputeOptions(parsed);
          const nextProvider = parsed.providers[0] ?? "aws";
          setCloudProvider(nextProvider);
          setDisplayInstanceType(displayDefaultForProvider(parsed, nextProvider));
        } else {
          resetToFallbackCompute();
        }
      })
      .catch(() => {
        resetToFallbackCompute();
      });

    function resetToFallbackCompute() {
      setComputeOptions(FALLBACK_COMPUTE_OPTIONS);
      setCloudProvider("aws");
      setDisplayInstanceType(displayDefaultForProvider(FALLBACK_COMPUTE_OPTIONS));
    }

    // defaultTier is stable for the session (derived from window.location),
    // safe to omit from deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const handleCreate = async () => {
    if (!name.trim()) {
      setError("Name is required");
      return;
    }
    if (!isExternal && !llmSelection.model.trim()) {
      setError("Model is required");
      return;
    }
    // Platform-managed providers need NO user credential — the platform injects
    // its own usage token (MOLECULE_LLM_USAGE_TOKEN = tenant admin_token) at
    // provision time. Only BYOK providers require a user-supplied key. (#2245)
    if (
      !isExternal &&
      !isPlatformManagedProvider(selectedLLMProvider) &&
      selectedLLMProvider?.envVars.length &&
      !llmSecret.trim()
    ) {
      setError("Provider credential is required");
      return;
    }
    setCreating(true);
    setError(null);

    const nativeProvider = selectedLLMProvider;

    try {
      const parsedBudget = budgetLimit.trim()
        ? parseFloat(budgetLimit)
        : null;
      const [displayWidth, displayHeight] = displayResolution.split("x").map((v) => parseInt(v, 10));
      const parsedRootGB = parseInt(displayRootGB, 10);

      const createResp = await api.post<{
        id: string;
        status: string;
        external?: boolean;
        connection?: ExternalConnectionInfo;
      }>("/workspaces", {
        name: name.trim(),
        role: role.trim() || undefined,
        // External workspaces don't consume a template — skip it so the
        // backend doesn't try to resolve a non-existent dir and log a
        // misleading "template not found" warning.
        template: isExternal ? undefined : (template.trim() || undefined),
        tier,
        parent_id: parentId || undefined,
        budget_limit: parsedBudget,
        ...(!isExternal && nativeProvider
          ? {
              model: llmSelection.model.trim(),
              llm_provider: nativeProvider.vendor,
              // Only BYOK providers carry a user secret. For platform-managed
              // the token is provisioner-injected; sending an (empty) secret
              // here would clobber it — so omit it entirely. (#2245)
              ...(nativeProvider.envVars.length > 0 &&
              !isPlatformManagedProvider(nativeProvider)
                ? { secrets: { [nativeProvider.envVars[0]]: llmSecret.trim() } }
                : {}),
            }
          : {}),
        ...(!isExternal
          ? {
              compute: displayEnabled
                ? {
                    instance_type: displayInstanceType,
                    volume: { root_gb: Number.isFinite(parsedRootGB) ? parsedRootGB : DEFAULT_DISPLAY_ROOT_GB },
                    display: {
                      mode: "desktop-control",
                      protocol: "novnc",
                      width: Number.isFinite(displayWidth) ? displayWidth : 1920,
                      height: Number.isFinite(displayHeight) ? displayHeight : 1080,
                    },
                    // Only meaningful when CP provisions the box (SaaS), where
                    // the picker is shown. Omit on self-hosted so the payload is
                    // unchanged there.
                    ...(isSaaS ? { provider: cloudProvider } : {}),
                  }
                : {
                    instance_type: defaultInstanceForProvider(computeOptions, cloudProvider),
                    volume: { root_gb: DEFAULT_HEADLESS_ROOT_GB },
                    display: { mode: "none" },
                    ...(isSaaS ? { provider: cloudProvider } : {}),
                  },
            }
          : {}),
        canvas: { x: Math.random() * 400 + 100, y: Math.random() * 300 + 100 },
        // Runtime=external flips the backend into awaiting-agent mode:
        // no container provisioning, token minted, connection payload
        // returned in the response for the modal below.
        ...(isExternal ? { runtime: externalRuntime } : { runtime }),
      });
      // External path: keep the create dialog open just long enough to
      // hand control to the connect modal, then close. The connect
      // modal holds the token; we CANNOT re-fetch it later. If the
      // backend somehow returns external=true without a connection
      // payload we still close the create dialog — the operator will
      // have to mint a token via POST /workspaces/:id/tokens.
      if (isExternal && createResp.connection) {
        setExternalConnection(createResp.connection);
      }
      setOpen(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create workspace");
    } finally {
      setCreating(false);
    }
  };

  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger asChild>
        <button type="button" className="fixed bottom-6 right-6 z-40 px-5 py-2.5 bg-accent hover:bg-accent-strong active:bg-accent text-sm font-medium rounded-xl text-white shadow-lg shadow-accent/20 hover:shadow-xl hover:shadow-accent/30 transition-all duration-200 flex items-center gap-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-2 focus-visible:ring-offset-surface">
          <svg
            width="14"
            height="14"
            viewBox="0 0 14 14"
            fill="none"
            className="shrink-0"
            aria-hidden="true"
          >
            <path
              d="M7 1v12M1 7h12"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            />
          </svg>
          New Workspace
        </button>
      </Dialog.Trigger>

      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-50 bg-black/70 backdrop-blur-sm" />
        <Dialog.Content
          className="fixed z-50 left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-surface-sunken border border-line/60 rounded-2xl shadow-2xl shadow-black/40 w-[400px] max-h-[90vh] overflow-y-auto p-6"
        >
          <Dialog.Title className="text-base font-semibold text-ink mb-1">
            Create Workspace
          </Dialog.Title>
          <p className="text-xs text-ink-mid mb-5">
            Add a new workspace node to the canvas
          </p>

          <div className="space-y-3.5">
            <InputField
              label="Name"
              required
              value={name}
              onChange={setName}
              placeholder="e.g. SEO Agent"
            />
            <InputField
              label="Role"
              value={role}
              onChange={setRole}
              placeholder="e.g. SEO Specialist"
            />
            <InputField
              label="Budget limit (USD)"
              value={budgetLimit}
              onChange={setBudgetLimit}
              placeholder="e.g. 100"
              type="number"
              helper="Leave blank for unlimited"
            />
            {/* External toggle — when on, this workspace is BYO-compute:
                no template, no model, no hermes provider fields. Backend
                returns a copyable connection snippet via the modal. */}
            <label className="flex items-start gap-2 rounded-lg border border-line p-3 cursor-pointer hover:border-line transition-colors">
              <input
                type="checkbox"
                checked={isExternal}
                onChange={(e) => setIsExternal(e.target.checked)}
                className="mt-0.5"
              />
              <div className="text-xs">
                <div className="text-ink font-medium">External agent (bring your own compute)</div>
                <div className="text-ink-mid mt-0.5">
                  Skip the container. We&apos;ll return a workspace_id + auth token + ready-to-paste snippet so an agent running on your laptop / server / CI can register via A2A.
                </div>
              </div>
            </label>

            {isExternal && (
              <div>
                <label className="text-[11px] text-ink-mid block mb-1">
                  External Runtime
                </label>
                <select
                  value={externalRuntime}
                  onChange={(e) => setExternalRuntime(e.target.value)}
                  className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                >
                  <option value="external">Generic External</option>
                  <option value="kimi">Kimi CLI</option>
                  <option value="kimi-cli">Kimi CLI (alt)</option>
                </select>
              </div>
            )}

            {!isExternal && (
              <div className="space-y-3">
                <div>
                  <label htmlFor="runtime-select" className="text-[11px] text-ink-mid block mb-1">
                    Runtime
                  </label>
                  <select
                    id="runtime-select"
                    value={runtime}
                    onChange={(e) => handleRuntimeChange(e.target.value)}
                    className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                  >
                    {RUNTIME_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label htmlFor="workspace-template-select" className="text-[11px] text-ink-mid block mb-1">
                    Workspace Template
                  </label>
                  <select
                    id="workspace-template-select"
                    value={template}
                    onChange={(e) => setTemplate(e.target.value)}
                    className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                  >
                    <option value="">Blank workspace</option>
                    {visibleTemplateSpecs.map((spec) => (
                      <option key={spec.id} value={spec.id}>
                        {spec.name || spec.id}
                      </option>
                    ))}
                  </select>
                </div>
              </div>
            )}

            {!isExternal && selectedLLMProvider && (
              <div className="rounded-lg border border-line/50 bg-surface-card/40 p-3 space-y-3">
                <div className="text-[11px] font-medium text-ink-mid">
                  LLM
                </div>
                <ProviderModelSelector
                  models={llmModels}
                  catalog={registryBacked ? llmCatalog : undefined}
                  value={llmSelection}
                  onChange={(next) => {
                    setLLMSelection(next);
                    setLLMSecret("");
                  }}
                  idPrefix="create-workspace-llm"
                  variant="stack"
                />
                {isPlatformManagedProvider(selectedLLMProvider) ? (
                  <div className="text-[11px] text-ink-soft">
                    Platform-managed — no API key required.
                  </div>
                ) : (
                  selectedLLMProvider.envVars.length > 0 && (
                    <div>
                      <label htmlFor="llm-secret-input" className="text-[11px] text-ink-mid block mb-1">
                        {selectedLLMProvider.envVars[0]}
                      </label>
                      <input
                        id="llm-secret-input"
                        type="password"
                        value={llmSecret}
                        onChange={(e) => setLLMSecret(e.target.value)}
                        autoComplete="off"
                        className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink placeholder-ink-soft focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors font-mono"
                      />
                    </div>
                  )
                )}
              </div>
            )}

            <div>
              <div
                role="radiogroup"
                aria-label="Workspace tier"
                className={`grid gap-1.5 ${isSaaS ? "grid-cols-1" : "grid-cols-4"}`}
              >
                <div className={`text-[11px] text-ink-mid mb-1 ${isSaaS ? "" : "col-span-4"}`}>
                  Tier{isSaaS ? " — dedicated VM" : ""}
                </div>
                {TIERS.map((t, idx) => (
                  <button
                    type="button"
                    key={t.value}
                    ref={(el) => { radioRefs.current[idx] = el; }}
                    role="radio"
                    aria-checked={tier === t.value}
                    tabIndex={tier === t.value ? 0 : -1}
                    onClick={() => setTier(t.value)}
                    onKeyDown={(e) => handleRadioKeyDown(e, idx)}
                    className={`py-2 rounded-lg text-center transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 ${
                      tier === t.value
                        ? "bg-accent-strong/20 border border-accent/50 text-accent"
                        : "bg-surface-card/60 border border-line/40 text-ink-mid hover:text-ink-mid hover:border-line"
                    }`}
                  >
                    <div className="text-xs font-mono font-semibold">
                      {t.label}
                    </div>
                    <div className="text-[10px] mt-0.5 opacity-70">
                      {t.desc}
                    </div>
                  </button>
                ))}
              </div>
            </div>

            {!isExternal && (
              <div className="rounded-lg border border-line/50 bg-surface-card/40 p-3">
                <div className="mb-2 text-[11px] font-medium text-ink-mid">
                  Container Config
                </div>
                {/* Cloud provider — only meaningful when CP provisions the box
                    (SaaS). A non-tenant-cloud workspace is reached over a
                    per-workspace Cloudflare tunnel (runtime#95). */}
                {isSaaS && (
                  <label htmlFor="workspace-cloud-provider" className="mb-3 grid gap-1">
                    <span className="text-xs font-medium text-ink">Cloud provider</span>
                    <select
                      id="workspace-cloud-provider"
                      value={cloudProvider}
                      onChange={(e) => setCloudProvider(e.target.value)}
                      className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                    >
                      {computeOptions.providers.map((p) => (
                        <option key={p} value={p}>
                          {providerLabel(p)}
                        </option>
                      ))}
                    </select>
                  </label>
                )}
                <label className="flex items-center justify-between gap-3">
                  <span className="text-xs font-medium text-ink">Display</span>
                  <input
                    type="checkbox"
                    checked={displayEnabled}
                    onChange={(e) => setDisplayEnabled(e.target.checked)}
                    aria-label="Enable display"
                    className="h-4 w-4"
                  />
                </label>
                {displayEnabled && (
                  <div className="mt-3 grid grid-cols-2 gap-2">
                    <div>
                      <label htmlFor="display-instance-type" className="mb-1 block text-[11px] text-ink-mid">
                        Instance
                      </label>
                      <select
                        id="display-instance-type"
                        value={displayInstanceType}
                        onChange={(e) => setDisplayInstanceType(e.target.value)}
                        className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-2 py-2 text-xs text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                      >
                        {instanceTypesForProvider(computeOptions, cloudProvider).map((it) => (
                          <option key={it} value={it}>
                            {it}
                          </option>
                        ))}
                      </select>
                    </div>
                    <div>
                      <label htmlFor="display-root-gb" className="mb-1 block text-[11px] text-ink-mid">
                        Disk GB
                      </label>
                      <input
                        id="display-root-gb"
                        type="number"
                        min="30"
                        max="500"
                        value={displayRootGB}
                        onChange={(e) => setDisplayRootGB(e.target.value)}
                        className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-2 py-2 text-xs text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                      />
                    </div>
                    <div className="col-span-2">
                      <label htmlFor="display-resolution" className="mb-1 block text-[11px] text-ink-mid">
                        Resolution
                      </label>
                      <select
                        id="display-resolution"
                        value={displayResolution}
                        onChange={(e) => setDisplayResolution(e.target.value)}
                        className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-2 py-2 text-xs text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
                      >
                        <option value="1920x1080">1920 x 1080</option>
                        <option value="1600x900">1600 x 900</option>
                        <option value="1280x720">1280 x 720</option>
                      </select>
                    </div>
                  </div>
                )}
              </div>
            )}

            <div>
              <label htmlFor="parent-workspace-select" className="text-[11px] text-ink-mid block mb-1">
                Parent Workspace
              </label>
              <select
                id="parent-workspace-select"
                value={parentId}
                onChange={(e) => setParentId(e.target.value)}
                className="w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors"
              >
                <option value="">None (root level)</option>
                {workspaces.map((ws) => (
                  <option key={ws.id} value={ws.id}>
                    T{ws.tier} · {ws.name}
                  </option>
                ))}
              </select>
            </div>
          </div>

          {error && (
            <div
              role="alert"
              className="mt-4 px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-xs text-bad"
            >
              {error}
            </div>
          )}

          <div className="flex justify-end gap-2.5 mt-6">
            <Dialog.Close asChild>
              <button type="button" className="px-4 py-2 bg-surface-card hover:bg-surface-elevated hover:text-ink text-sm rounded-lg text-ink-mid transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">
                Cancel
              </button>
            </Dialog.Close>
            <button
              type="button"
              onClick={handleCreate}
              disabled={creating}
              className="px-5 py-2 bg-accent hover:bg-accent-strong active:bg-accent text-sm rounded-lg text-white disabled:opacity-50 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
            >
              {creating ? "Creating..." : "Create"}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
      {/* Rendered as a sibling so it stays mounted after the create dialog
          closes. Without this the auth_token would disappear the moment
          the create modal unmounted its React subtree — the operator
          would never see the copy-paste snippet. */}
      <ExternalConnectModal
        info={externalConnection}
        onClose={() => setExternalConnection(null)}
      />
    </Dialog.Root>
  );
}

function InputField({
  label,
  value,
  onChange,
  placeholder,
  required,
  mono,
  type = "text",
  helper,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  required?: boolean;
  mono?: boolean;
  type?: string;
  helper?: string;
}) {
  // useId() generates a stable, unique ID for the label↔input association,
  // satisfying WCAG 2.1 SC 1.3.1 (Info and Relationships, Level A).
  const inputId = useId();

  return (
    <div>
      <label htmlFor={inputId} className="text-[11px] text-ink-mid block mb-1">
        {label}{" "}
        {required && (
          <>
            <span aria-hidden="true" className="text-bad">
              *
            </span>
            <span className="sr-only"> (required)</span>
          </>
        )}
      </label>
      <input
        id={inputId}
        type={type}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        min={type === "number" ? "0" : undefined}
        step={type === "number" ? "0.01" : undefined}
        className={`w-full bg-surface-card/60 border border-line/50 rounded-lg px-3 py-2 text-sm text-ink placeholder-ink-soft focus:outline-none focus:border-accent/60 focus:ring-1 focus:ring-accent/20 transition-colors ${mono ? "font-mono text-xs" : ""}`}
      />
      {helper && (
        <p className="mt-1 text-xs text-ink-mid">{helper}</p>
      )}
    </div>
  );
}
