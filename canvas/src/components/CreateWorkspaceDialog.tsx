"use client";

import { useState, useEffect, useRef, useCallback, useId, useMemo } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { api } from "@/lib/api";
import { isSaaSTenant } from "@/lib/tenant";
import { ExternalConnectModal, type ExternalConnectionInfo } from "./ExternalConnectModal";

interface WorkspaceOption {
  id: string;
  name: string;
  tier: number;
}

interface HermesProvider {
  id: string;
  label: string;
  envVar: string;
  defaultModel: string;
  models: string[];
}

// All providers supported by Hermes runtime via providers.resolve_provider().
// `defaultModel` is the slug injected into the workspace provision request
// when the user picks this provider — template-hermes's derive-provider.sh
// maps the prefix back to the provider name at install time, so this is
// the canonical handshake. `models` are additional suggestions surfaced in
// the datalist so the user can pick a different size without typing the
// whole slug.
export const HERMES_PROVIDERS: HermesProvider[] = [
  { id: "anthropic",  label: "Anthropic (Claude)",    envVar: "ANTHROPIC_API_KEY",  defaultModel: "anthropic/claude-sonnet-4-5",   models: ["anthropic/claude-opus-4-5", "anthropic/claude-sonnet-4-5", "anthropic/claude-haiku-4-5"] },
  { id: "openai",     label: "OpenAI",                envVar: "OPENAI_API_KEY",     defaultModel: "openai/gpt-4o",                 models: ["openai/gpt-4o", "openai/gpt-4o-mini", "openai/o3-mini"] },
  { id: "openrouter", label: "OpenRouter",            envVar: "OPENROUTER_API_KEY", defaultModel: "openrouter/auto",               models: ["openrouter/auto", "openrouter/anthropic/claude-sonnet-4", "openrouter/meta-llama/llama-3.3-70b"] },
  { id: "xai",        label: "xAI (Grok)",            envVar: "XAI_API_KEY",        defaultModel: "xai/grok-4",                    models: ["xai/grok-4", "xai/grok-4-mini"] },
  { id: "gemini",     label: "Google Gemini",         envVar: "GEMINI_API_KEY",     defaultModel: "gemini/gemini-2.5-pro",         models: ["gemini/gemini-2.5-pro", "gemini/gemini-2.5-flash"] },
  { id: "qwen",       label: "Qwen (Alibaba)",        envVar: "QWEN_API_KEY",       defaultModel: "alibaba/qwen3-max",             models: ["alibaba/qwen3-max", "alibaba/qwen3-coder"] },
  { id: "glm",        label: "GLM (Zhipu AI)",        envVar: "GLM_API_KEY",        defaultModel: "zai/glm-4.6",                   models: ["zai/glm-4.6", "zai/glm-4.5-air"] },
  { id: "kimi",       label: "Kimi (Moonshot)",       envVar: "KIMI_API_KEY",       defaultModel: "kimi-coding/kimi-k2",           models: ["kimi-coding/kimi-k2", "kimi-coding/kimi-k1.5"] },
  { id: "minimax",    label: "MiniMax",               envVar: "MINIMAX_API_KEY",    defaultModel: "minimax/MiniMax-M2.7",          models: ["minimax/MiniMax-M2.7", "minimax/MiniMax-M2.7-highspeed", "minimax/MiniMax-M1"] },
  { id: "deepseek",   label: "DeepSeek",              envVar: "DEEPSEEK_API_KEY",   defaultModel: "deepseek/deepseek-chat",        models: ["deepseek/deepseek-chat", "deepseek/deepseek-reasoner"] },
  { id: "groq",       label: "Groq",                  envVar: "GROQ_API_KEY",       defaultModel: "openrouter/groq/llama-3.3-70b", models: ["openrouter/groq/llama-3.3-70b"] },
  { id: "mistral",    label: "Mistral",               envVar: "MISTRAL_API_KEY",    defaultModel: "openrouter/mistralai/mistral-large", models: ["openrouter/mistralai/mistral-large"] },
  { id: "together",   label: "Together AI",           envVar: "TOGETHER_API_KEY",   defaultModel: "openrouter/meta-llama/llama-3.3-70b", models: ["openrouter/meta-llama/llama-3.3-70b"] },
  { id: "fireworks",  label: "Fireworks AI",          envVar: "FIREWORKS_API_KEY",  defaultModel: "openrouter/meta-llama/llama-3.3-70b", models: ["openrouter/meta-llama/llama-3.3-70b"] },
  { id: "hermes",     label: "Hermes / Nous (legacy)", envVar: "HERMES_API_KEY",    defaultModel: "nousresearch/Hermes-3-Llama-3.1-405B", models: ["nousresearch/Hermes-3-Llama-3.1-405B", "nousresearch/Hermes-4-14B"] },
];

export function CreateWorkspaceButton() {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [role, setRole] = useState("");
  const [template, setTemplate] = useState("");
  const [parentId, setParentId] = useState("");
  const [budgetLimit, setBudgetLimit] = useState("");
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [workspaces, setWorkspaces] = useState<WorkspaceOption[]>([]);
  // External-runtime path: skip docker provision, mint a workspace_auth_token,
  // and surface the connection snippet in a modal after create. When
  // isExternal is true the template / model / hermes-provider fields are
  // hidden (they're meaningless for BYO-compute agents).
  const [isExternal, setIsExternal] = useState(false);
  const [externalConnection, setExternalConnection] =
    useState<ExternalConnectionInfo | null>(null);

  // Hermes-specific state
  const [hermesProvider, setHermesProvider] = useState("anthropic");
  const [hermesApiKey, setHermesApiKey] = useState("");
  // Model slug is sent to CP as `model` and plumbed to the workspace EC2
  // as HERMES_DEFAULT_MODEL env var. template-hermes's derive-provider.sh
  // reads the prefix (`minimax/…`, `anthropic/…`) to set
  // HERMES_INFERENCE_PROVIDER at install time. Missing model → provider
  // falls back to "auto" and hermes picks its compiled-in default
  // (Anthropic), which 401s if the user's key is for a different
  // provider. Hence: require model when template=hermes.
  const [hermesModel, setHermesModel] = useState("");

  // Tier picker: on SaaS every workspace gets its own EC2 VM (Full Access
  // by construction), so we hide the T1/T2/T3 Docker-sandbox tiers and
  // lock to T4 — the full-host access tier, which maps to t3.large at the
  // CP level. On self-hosted we still offer T1/T2/T3 because the Docker-
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

  const isHermes = template.trim().toLowerCase() === "hermes";

  // Auto-fill hermesModel with the provider's defaultModel whenever the
  // provider changes, but only if the user hasn't already typed their own
  // slug. Prevents the empty-model → "auto" → Anthropic-default 401 trap.
  useEffect(() => {
    if (!isHermes) return;
    const p = HERMES_PROVIDERS.find((x) => x.id === hermesProvider);
    if (!p) return;
    // Replace model only if current value matches another provider's
    // default (user hasn't customized it) OR is empty.
    const isUntouched =
      hermesModel === "" ||
      HERMES_PROVIDERS.some((x) => x.defaultModel === hermesModel);
    if (isUntouched) setHermesModel(p.defaultModel);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hermesProvider, isHermes]);

  // Reset form and load workspaces whenever dialog opens
  useEffect(() => {
    if (!open) return;
    setName("");
    setRole("");
    setTier(defaultTier);
    setTemplate("");
    setParentId("");
    setBudgetLimit("");
    setError(null);
    setHermesProvider("anthropic");
    setHermesApiKey("");
    setHermesModel("");
    api
      .get<WorkspaceOption[]>("/workspaces")
      .then((ws) => setWorkspaces(ws))
      .catch(() => {});
    // defaultTier is stable for the session (derived from window.location),
    // safe to omit from deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const handleCreate = async () => {
    if (!name.trim()) {
      setError("Name is required");
      return;
    }
    if (isHermes && !hermesApiKey.trim()) {
      setError("API key is required for Hermes workspaces");
      return;
    }
    if (isHermes && !hermesModel.trim()) {
      setError("Model is required for Hermes workspaces — provider routing depends on the model slug prefix");
      return;
    }
    setCreating(true);
    setError(null);

    const provider = isHermes
      ? HERMES_PROVIDERS.find((p) => p.id === hermesProvider)
      : undefined;

    try {
      const parsedBudget = budgetLimit.trim()
        ? parseFloat(budgetLimit)
        : null;

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
        canvas: { x: Math.random() * 400 + 100, y: Math.random() * 300 + 100 },
        // Runtime=external flips the backend into awaiting-agent mode:
        // no container provisioning, token minted, connection payload
        // returned in the response for the modal below.
        ...(isExternal ? { runtime: "external" } : {}),
        ...(!isExternal && isHermes && provider
          ? {
              secrets: { [provider.envVar]: hermesApiKey.trim() },
              model: hermesModel.trim(),
            }
          : {}),
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
        <button type="button" className="fixed bottom-6 right-6 z-40 px-5 py-2.5 bg-blue-600 hover:bg-blue-500 active:bg-blue-700 text-sm font-medium rounded-xl text-white shadow-lg shadow-blue-600/20 hover:shadow-xl hover:shadow-blue-500/30 transition-all duration-200 flex items-center gap-2">
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
          className="fixed z-50 left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-zinc-900 border border-zinc-700/60 rounded-2xl shadow-2xl shadow-black/40 w-[400px] max-h-[90vh] overflow-y-auto p-6"
        >
          <Dialog.Title className="text-base font-semibold text-zinc-100 mb-1">
            Create Workspace
          </Dialog.Title>
          <p className="text-xs text-zinc-500 mb-5">
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
            <label className="flex items-start gap-2 rounded-lg border border-zinc-800 p-3 cursor-pointer hover:border-zinc-700 transition-colors">
              <input
                type="checkbox"
                checked={isExternal}
                onChange={(e) => setIsExternal(e.target.checked)}
                className="mt-0.5"
              />
              <div className="text-xs">
                <div className="text-zinc-200 font-medium">External agent (bring your own compute)</div>
                <div className="text-zinc-500 mt-0.5">
                  Skip the container. We&apos;ll return a workspace_id + auth token + ready-to-paste snippet so an agent running on your laptop / server / CI can register via A2A.
                </div>
              </div>
            </label>

            {!isExternal && (
              <InputField
                label="Template"
                value={template}
                onChange={setTemplate}
                placeholder="e.g. seo-agent (from workspace-configs-templates/)"
                mono
              />
            )}

            <div>
              <div
                role="radiogroup"
                aria-label="Workspace tier"
                className={`grid gap-1.5 ${isSaaS ? "grid-cols-1" : "grid-cols-4"}`}
              >
                <div className={`text-[11px] text-zinc-400 mb-1 ${isSaaS ? "" : "col-span-4"}`}>
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
                    className={`py-2 rounded-lg text-center transition-colors ${
                      tier === t.value
                        ? "bg-blue-600/20 border border-blue-500/50 text-blue-300"
                        : "bg-zinc-800/60 border border-zinc-700/40 text-zinc-400 hover:text-zinc-300 hover:border-zinc-600"
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

            <div>
              <label className="text-[11px] text-zinc-400 block mb-1">
                Parent Workspace
              </label>
              <select
                value={parentId}
                onChange={(e) => setParentId(e.target.value)}
                className="w-full bg-zinc-800/60 border border-zinc-700/50 rounded-lg px-3 py-2 text-sm text-zinc-100 focus:outline-none focus:border-blue-500/60 focus:ring-1 focus:ring-blue-500/20 transition-colors"
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

          {/* Hermes provider configuration — shown only when template === "hermes" */}
          {isHermes && (
            <div
              className="mt-4 rounded-xl border border-violet-700/40 bg-violet-950/20 p-4 space-y-3"
              data-testid="hermes-provider-section"
            >
              <p className="text-[11px] font-semibold text-violet-400 uppercase tracking-wide">
                Hermes Provider
              </p>
              <p className="text-[11px] text-zinc-500 -mt-1">
                Choose the AI provider and paste your API key. The key is
                stored as an encrypted workspace secret.
              </p>

              <div>
                <label
                  htmlFor="hermes-provider-select"
                  className="text-[11px] text-zinc-400 block mb-1"
                >
                  Provider
                </label>
                <select
                  id="hermes-provider-select"
                  value={hermesProvider}
                  onChange={(e) => setHermesProvider(e.target.value)}
                  aria-label="Hermes provider"
                  className="w-full bg-zinc-800/60 border border-zinc-700/50 rounded-lg px-3 py-2 text-sm text-zinc-100 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20 transition-colors"
                >
                  {HERMES_PROVIDERS.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.label}
                    </option>
                  ))}
                </select>
              </div>

              <div>
                <label
                  htmlFor="hermes-api-key-input"
                  className="text-[11px] text-zinc-400 block mb-1"
                >
                  API Key{" "}
                  <span aria-hidden="true" className="text-red-400">
                    *
                  </span>
                  <span className="sr-only"> (required)</span>
                </label>
                <input
                  id="hermes-api-key-input"
                  type="password"
                  value={hermesApiKey}
                  onChange={(e) => setHermesApiKey(e.target.value)}
                  placeholder="sk-…"
                  aria-label="Hermes API key"
                  autoComplete="off"
                  className="w-full bg-zinc-800/60 border border-zinc-700/50 rounded-lg px-3 py-2 text-sm text-zinc-100 placeholder-zinc-600 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20 transition-colors font-mono"
                />
              </div>

              <div>
                <label
                  htmlFor="hermes-model-input"
                  className="text-[11px] text-zinc-400 block mb-1"
                >
                  Model{" "}
                  <span aria-hidden="true" className="text-red-400">
                    *
                  </span>
                  <span className="sr-only"> (required)</span>
                </label>
                <input
                  id="hermes-model-input"
                  type="text"
                  value={hermesModel}
                  onChange={(e) => setHermesModel(e.target.value)}
                  placeholder="e.g. minimax/MiniMax-M2.7"
                  aria-label="Hermes model slug"
                  autoComplete="off"
                  spellCheck={false}
                  list="hermes-model-suggestions"
                  className="w-full bg-zinc-800/60 border border-zinc-700/50 rounded-lg px-3 py-2 text-sm text-zinc-100 placeholder-zinc-600 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20 transition-colors font-mono"
                />
                <datalist id="hermes-model-suggestions">
                  {HERMES_PROVIDERS.find((p) => p.id === hermesProvider)?.models.map(
                    (m) => <option key={m} value={m} />,
                  )}
                </datalist>
                <p className="text-[10px] text-zinc-500 mt-1">
                  Slug determines which provider hermes routes to at install time.
                </p>
              </div>
            </div>
          )}

          {error && (
            <div
              role="alert"
              className="mt-4 px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-xs text-red-400"
            >
              {error}
            </div>
          )}

          <div className="flex justify-end gap-2.5 mt-6">
            <Dialog.Close asChild>
              <button type="button" className="px-4 py-2 bg-zinc-800 hover:bg-zinc-700 text-sm rounded-lg text-zinc-300 transition-colors">
                Cancel
              </button>
            </Dialog.Close>
            <button
              type="button"
              onClick={handleCreate}
              disabled={creating}
              className="px-5 py-2 bg-blue-600 hover:bg-blue-500 active:bg-blue-700 text-sm rounded-lg text-white disabled:opacity-50 transition-colors"
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
      <label htmlFor={inputId} className="text-[11px] text-zinc-400 block mb-1">
        {label}{" "}
        {required && (
          <>
            <span aria-hidden="true" className="text-red-400">
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
        className={`w-full bg-zinc-800/60 border border-zinc-700/50 rounded-lg px-3 py-2 text-sm text-zinc-100 placeholder-zinc-500 focus:outline-none focus:border-blue-500/60 focus:ring-1 focus:ring-blue-500/20 transition-colors ${mono ? "font-mono text-xs" : ""}`}
      />
      {helper && (
        <p className="mt-1 text-xs text-zinc-500">{helper}</p>
      )}
    </div>
  );
}
