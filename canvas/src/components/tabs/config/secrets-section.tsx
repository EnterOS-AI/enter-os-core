"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { Section } from "./form-inputs";
import { markAllWorkspacesNeedRestart } from "@/lib/canvas-actions";

interface SecretEntry {
  key: string;
  has_value: boolean;
  created_at: string;
  updated_at: string;
  scope?: "global" | "workspace";
}

// Human-friendly labels for well-known env-var names. Used to render
// familiar copy ("Anthropic API Key") instead of the raw variable name
// when the template declares one of these. Unknown names (e.g.
// MINIMAX_API_KEY, ZHIPU_API_KEY) fall through to humanizeKeyName below
// — a generic "Minimax API Key" label is better than no label at all.
//
// SECRETS_WHEN_NO_TEMPLATE is the fallback set shown only when a
// workspace's template doesn't declare any required_env (legacy /
// bare-runtime case). In the normal flow the list is driven by
// runtime_config.required_env passed in from the Config tab.
const KNOWN_LABELS: Record<string, string> = {
  ANTHROPIC_API_KEY: "Anthropic API Key",
  OPENAI_API_KEY: "OpenAI API Key",
  GOOGLE_API_KEY: "Google AI API Key",
  SERP_API_KEY: "SERP API Key",
  OPENROUTER_API_KEY: "OpenRouter API Key",
  HERMES_API_KEY: "Hermes API Key (Nous Research)",
  GROQ_API_KEY: "Groq API Key",
  CEREBRAS_API_KEY: "Cerebras API Key",
  MINIMAX_API_KEY: "Minimax API Key",
  MODEL_PROVIDER: "Model Override (e.g. anthropic:claude-sonnet-4-6)",
};

const SECRETS_WHEN_NO_TEMPLATE = [
  "ANTHROPIC_API_KEY",
  "OPENAI_API_KEY",
  "GOOGLE_API_KEY",
  "SERP_API_KEY",
  "MODEL_PROVIDER",
];

// humanizeKeyName converts SCREAMING_SNAKE_CASE into "Title Case Words"
// so templates that declare uncommon env var names still get a readable
// label. "MINIMAX_API_KEY" → "Minimax API Key". Preserves "API" / "URL"
// acronyms via the normalize step.
function humanizeKeyName(key: string): string {
  const words = key.toLowerCase().split("_").filter(Boolean);
  return words
    .map((w) => {
      const upper = w.toUpperCase();
      // Keep common acronyms upper-case.
      if (["API", "URL", "URI", "ID", "SDK", "MCP", "LLM", "AI"].includes(upper)) {
        return upper;
      }
      return w.charAt(0).toUpperCase() + w.slice(1);
    })
    .join(" ");
}

function labelForKey(key: string): string {
  return KNOWN_LABELS[key] ?? humanizeKeyName(key);
}

function ScopeBadge({ scope }: { scope: "global" | "workspace" | "override" }) {
  if (scope === "global") {
    return <span className="text-[8px] text-warm bg-amber-900/30 px-1.5 py-0.5 rounded" title="Inherited from global secrets">Global</span>;
  }
  if (scope === "override") {
    return <span className="text-[8px] text-purple-400 bg-purple-900/30 px-1.5 py-0.5 rounded" title="Overrides global secret">Override</span>;
  }
  return null;
}

function SecretRow({ label, secretKey, isSet, scope, globalMode, onSave, onDelete }: {
  label: string; secretKey: string; isSet: boolean;
  scope?: "global" | "workspace" | "override";
  globalMode?: boolean;
  onSave: (value: string) => void; onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");

  const actionLabel = (): string => {
    if (editing) return "Cancel";
    if (!isSet) return "Set";
    if (globalMode) return "Update";
    if (scope === "global") return "Override";
    return "Update";
  };

  const isPlaintext = secretKey === "MODEL_PROVIDER";

  return (
    <div className="bg-surface-card/50 rounded px-3 py-2 border border-line/50">
      <div className="flex items-center justify-between">
        <div className="min-w-0">
          <div className="text-[10px] text-ink-mid">{label}</div>
          <div className="flex items-center gap-2 mt-0.5">
            <span className="text-[9px] font-mono text-ink-soft">{secretKey}</span>
            {isSet && (
              <span className="text-[9px] font-mono text-ink-soft tracking-widest" title="Value is set (encrypted)">
                •••••
              </span>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {isSet && <span className="text-[10px] text-good bg-green-900/30 px-1.5 py-0.5 rounded">Set</span>}
          {scope && <ScopeBadge scope={scope} />}
          {!editing && isSet && (globalMode || scope !== "global") && (
            <button type="button" onClick={onDelete} className="text-[11px] text-bad hover:text-bad">Remove</button>
          )}
          <button type="button" onClick={() => setEditing(!editing)} className="text-[11px] text-accent hover:text-accent">
            {actionLabel()}
          </button>
        </div>
      </div>
      {editing && (
        <div className="flex gap-2 mt-2">
          <input
            value={value} onChange={(e) => setValue(e.target.value)}
            placeholder={isPlaintext ? "anthropic:claude-sonnet-4-6" : "sk-..."}
            type={isPlaintext ? "text" : "password"} autoFocus
            className="flex-1 bg-surface-sunken border border-line rounded px-2 py-1 text-[10px] text-ink font-mono focus:outline-none focus:border-accent"
          />
          <button type="button"
            onClick={() => { onSave(value); setEditing(false); setValue(""); }}
            disabled={!value}
            className="px-2 py-1 bg-accent-strong hover:bg-accent text-[10px] rounded text-white disabled:opacity-30"
          >Save</button>
        </div>
      )}
    </div>
  );
}

function CustomSecretRow({ secretKey, scope, globalMode, onSave, onDelete }: {
  secretKey: string;
  scope: "global" | "workspace" | "override";
  globalMode?: boolean;
  onSave: (value: string) => void;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");

  const canDelete = globalMode || scope !== "global";
  const showOverride = !globalMode && scope === "global";

  return (
    <div className="py-1.5 px-2">
      <div className="flex items-center justify-between">
        <div className="min-w-0">
          <span className={`text-[10px] font-mono ${globalMode ? "text-warm" : scope === "global" ? "text-ink-mid" : "text-accent"}`}>
            {secretKey}
          </span>
          <span className="text-[9px] font-mono text-ink-soft tracking-widest ml-2">•••••</span>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <span className="text-[10px] text-good">Set</span>
          {!globalMode && <ScopeBadge scope={scope} />}
          {canDelete && !editing && (
            <button type="button" onClick={onDelete} className="text-[11px] text-bad hover:text-bad">Remove</button>
          )}
          {(canDelete || showOverride) && (
            <button type="button" onClick={() => setEditing(!editing)} className="text-[11px] text-accent hover:text-accent">
              {editing ? "Cancel" : showOverride ? "Override" : "Update"}
            </button>
          )}
        </div>
      </div>
      {editing && (
        <div className="flex gap-2 mt-1.5">
          <input
            value={value} onChange={(e) => setValue(e.target.value)}
            placeholder="New value" type="password" autoFocus
            className="flex-1 bg-surface-sunken border border-line rounded px-2 py-1 text-[10px] text-ink font-mono focus:outline-none focus:border-accent"
          />
          <button type="button"
            onClick={() => { onSave(value); setEditing(false); setValue(""); }}
            disabled={!value}
            className="px-2 py-1 bg-accent-strong hover:bg-accent text-[10px] rounded text-white disabled:opacity-30"
          >Save</button>
        </div>
      )}
    </div>
  );
}

export function SecretsSection({ workspaceId, requiredEnv }: { workspaceId: string; requiredEnv?: string[] }) {
  const [mergedSecrets, setMergedSecrets] = useState<SecretEntry[]>([]);
  const [globalSecrets, setGlobalSecrets] = useState<SecretEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [newKey, setNewKey] = useState("");
  const [newValue, setNewValue] = useState("");
  const [globalMode, setGlobalMode] = useState(false);

  const loadSecrets = useCallback(async () => {
    setLoading(true);
    try {
      const [merged, global] = await Promise.all([
        api.get<SecretEntry[]>(`/workspaces/${workspaceId}/secrets`).catch(() => []),
        api.get<SecretEntry[]>("/settings/secrets").catch(() => []),
      ]);
      setMergedSecrets(merged);
      setGlobalSecrets(global);
    } catch {
      setMergedSecrets([]);
      setGlobalSecrets([]);
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => { loadSecrets(); }, [loadSecrets]);

  const handleSave = async (key: string, value: string) => {
    setError(null);
    try {
      if (globalMode) {
        await api.put("/settings/secrets", { key, value });
        markAllWorkspacesNeedRestart();
      } else {
        await api.put(`/workspaces/${workspaceId}/secrets`, { key, value });
      }
      setNewKey(""); setNewValue(""); setShowAdd(false);
      loadSecrets();
    } catch (e) { setError(e instanceof Error ? e.message : "Failed to save"); }
  };

  const handleDelete = async (key: string) => {
    setError(null);
    try {
      if (globalMode) {
        await api.del(`/settings/secrets/${encodeURIComponent(key)}`);
        markAllWorkspacesNeedRestart();
      } else {
        await api.del(`/workspaces/${workspaceId}/secrets/${encodeURIComponent(key)}`);
      }
      loadSecrets();
    } catch (e) { setError(e instanceof Error ? e.message : "Failed to delete"); }
  };

  // Build lookup sets from the merged view
  const mergedByKey = new Map(mergedSecrets.map((s) => [s.key, s]));
  const globalKeys = new Set(globalSecrets.map((s) => s.key));

  /** Determine scope badge for the workspace (non-global) view */
  const getScope = (entry: SecretEntry): "global" | "workspace" | "override" => {
    if (entry.scope === "workspace" && globalKeys.has(entry.key)) return "override";
    if (entry.scope === "global") return "global";
    return "workspace";
  };

  // For workspace view: use merged secrets from the backend (includes inherited globals)
  // For global view: use global secrets only
  const activeSecrets = globalMode ? globalSecrets : mergedSecrets;

  // Template-driven slots: render one labelled row per env var the
  // template declares. Falls back to a legacy common-keys list when
  // the template has nothing (older workspaces / bare runtimes) so
  // the Secrets section is never empty.
  const templateKeys = (requiredEnv && requiredEnv.length > 0)
    ? requiredEnv
    : SECRETS_WHEN_NO_TEMPLATE;

  // Deduplicate while preserving order — a template that lists the
  // same key twice shouldn't render two rows.
  const seen = new Set<string>();
  const slotKeys = templateKeys.filter((k) => {
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });

  // Split into template-slot keys and user-added custom keys so the
  // latter still surface even when not declared by the template.
  const slotKeySet = new Set(slotKeys);
  const customSecrets = activeSecrets.filter((s) => !slotKeySet.has(s.key));

  return (
    <Section title="Secrets & API Keys" defaultOpen={false}>
      {loading ? (
        <div className="text-[10px] text-ink-soft">Loading secrets...</div>
      ) : (
        <div className="space-y-2">
          {error && <div className="px-2 py-1 bg-red-900/30 border border-red-800 rounded text-[10px] text-bad">{error}</div>}

          {/* Scope toggle */}
          <div className="flex items-center gap-2 pb-1">
            <button
              onClick={() => setGlobalMode(false)}
              className={`text-[10px] px-2 py-0.5 rounded transition-colors ${
                !globalMode ? "bg-accent-strong/20 text-accent border border-accent/30" : "text-white-soft hover:text-white-mid"
              }`}
            >
              This Workspace
            </button>
            <button
              onClick={() => setGlobalMode(true)}
              className={`text-[10px] px-2 py-0.5 rounded transition-colors ${
                globalMode ? "bg-amber-600/20 text-warm border border-amber-500/30" : "text-white-soft hover:text-white-mid"
              }`}
            >
              Global (All Workspaces)
            </button>
          </div>

          {globalMode && (
            <div className="px-2 py-1.5 bg-amber-950/20 border border-amber-800/30 rounded text-[10px] text-warm/80 leading-relaxed">
              Global keys apply to all workspaces. Workspace-level keys override globals with the same name.
            </div>
          )}

          {/* Template-declared slots — one labelled row per env var
              the workspace actually needs. Driven by runtime_config.required_env. */}
          {slotKeys.map((key) => {
            const entry = globalMode
              ? globalSecrets.find((s) => s.key === key)
              : mergedByKey.get(key);
            const isSet = !!entry?.has_value;
            const scope = globalMode ? undefined : (entry ? getScope(entry) : undefined);
            return (
              <SecretRow key={key} label={labelForKey(key)} secretKey={key}
                isSet={isSet}
                scope={scope}
                globalMode={globalMode}
                onSave={(v) => handleSave(key, v)} onDelete={() => handleDelete(key)} />
            );
          })}

          {/* Custom secrets */}
          {customSecrets.map((s) => {
            const scope = globalMode ? ("global" as const) : getScope(s);
            return (
              <CustomSecretRow key={s.key} secretKey={s.key}
                scope={scope}
                globalMode={globalMode}
                onSave={(v) => handleSave(s.key, v)} onDelete={() => handleDelete(s.key)} />
            );
          })}

          {/* Add new */}
          {showAdd ? (
            <div className="bg-surface-card/50 rounded p-2 space-y-1.5 border border-line/50">
              <input value={newKey} onChange={(e) => setNewKey(e.target.value.toUpperCase())} placeholder="KEY_NAME"
                className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-[10px] font-mono text-ink focus:outline-none focus:border-accent" />
              <input value={newValue} onChange={(e) => setNewValue(e.target.value)} placeholder="Value" type="password"
                className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-[10px] text-ink focus:outline-none focus:border-accent" />
              <div className="flex gap-2">
                <button type="button" onClick={() => { if (newKey && newValue) handleSave(newKey, newValue); }} disabled={!newKey || !newValue}
                  className="px-2 py-1 bg-accent-strong hover:bg-accent text-[10px] rounded text-white disabled:opacity-30">
                  Save{globalMode ? " (Global)" : ""}
                </button>
                <button type="button" onClick={() => { setShowAdd(false); setNewKey(""); setNewValue(""); }}
                  className="px-2 py-1 bg-surface-card hover:bg-surface-card text-[10px] rounded text-ink-mid">Cancel</button>
              </div>
            </div>
          ) : (
            <button type="button" onClick={() => setShowAdd(true)} className="text-[10px] text-accent hover:text-accent">
              + Add {globalMode ? "Global " : ""}Variable
            </button>
          )}

          <div className="text-[9px] text-ink-soft pt-1">
            Values are encrypted and never exposed to the browser.
            {globalMode
              ? " Global keys are shared across all workspaces. Restart workspaces to apply changes."
              : " Global keys are inherited; workspace keys override globals with the same name."}
          </div>
        </div>
      )}
    </Section>
  );
}
