"use client";

// llm-billing-section.tsx — Config-tab section for the per-workspace
// llm_billing_mode override (internal#691).
//
// Surfaces:
//   - The currently RESOLVED mode for this workspace (the mode the
//     workspace-server's strip gate will use at next provision).
//   - The org-level default (so the user sees what they're inheriting).
//   - A dropdown to set / clear the workspace-level override.
//   - A "source" line so operators can answer "is this inherited or
//     explicit?" without DB archeology (RFC Observability hot-spot).
//
// Hits:
//   GET /admin/workspaces/:id/llm-billing-mode   — read resolution
//   PUT /admin/workspaces/:id/llm-billing-mode   — write {mode: "..."|null}
//
// Both routes are on the per-tenant workspace-server (same origin as the
// other canvas /admin calls). CP's proxy at /cp/admin/workspaces/:id/
// llm-billing-mode exists for ops use; the canvas uses the per-tenant
// path directly to keep the round-trip cheap.

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { Section } from "./form-inputs";

// Mirrors workspace-server/internal/handlers/llm_billing_mode.go::BillingModeResolution.
// Kept as a literal shape (not imported) because canvas has no Go-type bridge.
export interface BillingModeResolution {
  workspace_id: string;
  resolved_mode: "platform_managed" | "byok" | "disabled";
  // Pointer-typed on the Go side: nil = inherit, non-nil = the raw
  // workspace-level override (even if garbled and falling through).
  workspace_override: string | null;
  org_default: "platform_managed" | "byok" | "disabled";
  source: "workspace_override" | "org_default" | "constant_fallback";
}

// The dropdown emits one of these values. "inherit" is the UX-only label
// that maps to a `null` body in the PUT request.
type DropdownChoice = "inherit" | "platform_managed" | "byok" | "disabled";

interface Props {
  workspaceId: string;
}

const MODE_LABELS: Record<DropdownChoice, string> = {
  inherit: "Inherit from org default",
  platform_managed: "Platform-managed (uses Molecule credits)",
  byok: "BYOK (your own OAuth / vendor keys)",
  disabled: "Disabled (no LLM access)",
};

const MODE_DESCRIPTIONS: Record<DropdownChoice, string> = {
  inherit:
    "Use whichever mode is set at the organization level. Recommended unless this specific workspace needs a different billing source.",
  platform_managed:
    "Strip CLAUDE_CODE_OAUTH_TOKEN and vendor API keys from the workspace; route all LLM traffic through Molecule's proxy and bill your org credits.",
  byok:
    "Keep CLAUDE_CODE_OAUTH_TOKEN / vendor API keys in the workspace; LLM traffic goes directly to your provider and is billed to your OAuth subscription or API account.",
  disabled:
    "Block all LLM access for this workspace. Useful for sandbox workspaces that should not consume credits or hit external providers.",
};

const SOURCE_LABELS: Record<BillingModeResolution["source"], string> = {
  workspace_override: "explicit override on this workspace",
  org_default: "inherited from org default",
  constant_fallback:
    "fallback (workspace + org defaults missing or unrecognized — defaulted to platform_managed)",
};

export function LLMBillingSection({ workspaceId }: Props) {
  const [resolution, setResolution] = useState<BillingModeResolution | null>(
    null,
  );
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await api.get<BillingModeResolution>(
        `/admin/workspaces/${workspaceId}/llm-billing-mode`,
      );
      setResolution(res);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load billing mode");
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Current dropdown selection is derived from the resolution. If the
  // override is null, we show "inherit"; otherwise we mirror the raw
  // workspace_override (NOT resolved_mode — that would conflate "explicit
  // platform_managed override" with "inherit while org happens to be
  // platform_managed", which has different semantics on the write side).
  const currentChoice: DropdownChoice = (() => {
    if (!resolution) return "inherit";
    if (resolution.workspace_override == null) return "inherit";
    const raw = resolution.workspace_override;
    if (raw === "platform_managed" || raw === "byok" || raw === "disabled") {
      return raw;
    }
    // Garbled value persisted via some external write. Show inherit so
    // the user can pick a clean value; on save they'll either clear it
    // (PUT null) or overwrite it with a valid one.
    return "inherit";
  })();

  const handleChange = async (choice: DropdownChoice) => {
    if (!resolution) return;
    setSaving(true);
    setError(null);
    setSuccess(false);
    try {
      // "inherit" → PUT {mode: null}; otherwise → PUT {mode: choice}.
      const body = choice === "inherit" ? { mode: null } : { mode: choice };
      const updated = await api.put<BillingModeResolution>(
        `/admin/workspaces/${workspaceId}/llm-billing-mode`,
        body,
      );
      setResolution(updated);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to update billing mode");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Section title="LLM Billing" defaultOpen={false}>
      {loading && (
        <div className="text-[10px] text-ink-mid">Loading billing mode…</div>
      )}

      {error && (
        <div
          role="alert"
          aria-live="assertive"
          className="px-2 py-1 bg-red-900/30 border border-red-800 rounded text-[10px] text-bad mb-2"
        >
          {error}
        </div>
      )}

      {resolution && (
        <div className="space-y-2">
          <div className="text-[10px] text-ink-mid">
            Resolved mode: <strong className="text-ink">{resolution.resolved_mode}</strong>{" "}
            <span className="text-ink-mid">
              ({SOURCE_LABELS[resolution.source]})
            </span>
          </div>
          <div className="text-[10px] text-ink-mid">
            Org default: <span className="text-ink">{resolution.org_default}</span>
          </div>

          <label
            className="block text-[10px] text-ink-mid"
            htmlFor={`llm-billing-mode-${workspaceId}`}
          >
            Override
          </label>
          <select
            id={`llm-billing-mode-${workspaceId}`}
            aria-label="LLM billing mode override"
            value={currentChoice}
            disabled={saving}
            onChange={(e) => void handleChange(e.target.value as DropdownChoice)}
            className="w-full bg-surface-card border border-line rounded p-1 text-[10px] text-ink focus:outline-none focus:border-accent disabled:opacity-50"
          >
            {(Object.keys(MODE_LABELS) as DropdownChoice[]).map((m) => (
              <option key={m} value={m}>
                {MODE_LABELS[m]}
              </option>
            ))}
          </select>

          <div
            className="text-[10px] text-ink-mid leading-snug"
            aria-live="polite"
          >
            {MODE_DESCRIPTIONS[currentChoice]}
          </div>

          {success && (
            <div className="mt-1 px-2 py-1 bg-green-900/30 border border-green-800 rounded text-[10px] text-good">
              Updated. Restart the workspace to apply.
            </div>
          )}

          {resolution.workspace_override != null &&
            !["platform_managed", "byok", "disabled"].includes(
              resolution.workspace_override,
            ) && (
              <div
                role="alert"
                className="mt-1 px-2 py-1 bg-yellow-900/30 border border-yellow-800 rounded text-[10px] text-warning"
              >
                Workspace override has a non-standard value (
                <code>{resolution.workspace_override}</code>) and is being
                ignored. Pick a valid mode above to clear the corrupt value.
              </div>
            )}
        </div>
      )}
    </Section>
  );
}
