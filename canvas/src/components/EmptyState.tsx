"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { OrgTemplatesSection } from "./TemplatePalette";
import { type Template } from "@/lib/deploy-preflight";
import { useTemplateDeploy } from "@/hooks/useTemplateDeploy";
import { Spinner } from "./Spinner";
import { TIER_CONFIG } from "@/lib/design-tokens";

export function EmptyState() {
  const [templates, setTemplates] = useState<Template[]>([]);
  const [loading, setLoading] = useState(true);
  const [blankCreating, setBlankCreating] = useState(false);
  const [blankError, setBlankError] = useState<string | null>(null);

  useEffect(() => {
    api
      .get<Template[]>("/templates")
      .then((t) => setTemplates(t))
      .catch(() => setTemplates([]))
      .finally(() => setLoading(false));
  }, []);

  // Canvas fills in a visible "center-ish" spot on a fresh tenant so
  // the user doesn't have to pan to find their new workspace. Fixed
  // (200, 150) instead of the sidebar's random placement because the
  // canvas is guaranteed empty when this component mounts.
  const firstDeployCoords = useCallback(() => ({ x: 200, y: 150 }), []);

  // After the POST succeeds, auto-select the new workspace and flip
  // the panel to Chat. This is a UX flourish that only makes sense
  // on first deploy (the canvas is empty so the selection can't
  // surprise anyone); the sidebar intentionally skips this step.
  // 500 ms delay so React Flow has a frame to render the new node
  // before it receives focus.
  const handleDeployed = useCallback((workspaceId: string) => {
    setTimeout(() => {
      useCanvasStore.getState().selectNode(workspaceId);
      useCanvasStore.getState().setPanelTab("chat");
    }, 500);
  }, []);

  const { deploy, deploying, error, modal } = useTemplateDeploy({
    canvasCoords: firstDeployCoords,
    onDeployed: handleDeployed,
  });

  // "Create blank" bypasses templates entirely — no preflight, no
  // modal, just POST /workspaces with a default name. Deliberately
  // NOT routed through useTemplateDeploy because it has no
  // `template.id` to deploy against.
  //
  // tier is omitted so the backend picks a SaaS-aware default
  // (T4 on SaaS, T3 on self-hosted — see WorkspaceHandler.DefaultTier).
  // The previous hardcoded `tier: 2` shipped every fresh-tenant agent
  // at Standard regardless of host, which surprised SaaS users whose
  // CreateWorkspaceDialog already defaults to T4.
  const createBlank = async () => {
    setBlankCreating(true);
    setBlankError(null);
    try {
      const ws = await api.post<{ id: string }>("/workspaces", {
        name: "My First Agent",
        canvas: firstDeployCoords(),
      });
      handleDeployed(ws.id);
    } catch (e) {
      setBlankError(e instanceof Error ? e.message : "Create failed");
    } finally {
      setBlankCreating(false);
    }
  };

  // Any active gesture locks every button so the user can't fire a
  // second POST while the first is still in flight.
  const anyDeploying = !!deploying || blankCreating;
  const displayError = error ?? blankError;

  return (
    <div className="absolute inset-0 flex items-start justify-center pointer-events-none z-[1] overflow-y-auto py-8">
      <div className="relative max-w-2xl w-full rounded-3xl border border-line/70 bg-surface/80 backdrop-blur-xl px-8 py-8 text-center shadow-2xl shadow-black/40 pointer-events-auto mx-4">
        <div className="absolute inset-x-8 top-0 h-px bg-gradient-to-r from-transparent via-blue-500/50 to-transparent" />

        {/* Logo */}
        <div className="w-16 h-16 mx-auto mb-4 rounded-2xl bg-gradient-to-br from-sky-500/20 via-blue-500/20 to-violet-500/20 border border-accent/20 flex items-center justify-center">
          <svg width="28" height="28" viewBox="0 0 28 28" fill="none">
            <rect x="3" y="3" width="10" height="10" rx="2" stroke="#60a5fa" strokeWidth="1.5" opacity="0.65" />
            <rect x="15" y="3" width="10" height="10" rx="2" stroke="#60a5fa" strokeWidth="1.5" opacity="0.65" />
            <rect x="9" y="15" width="10" height="10" rx="2" stroke="#60a5fa" strokeWidth="1.5" opacity="0.65" />
            <path d="M8 13v2M20 13v4M14 13v2" stroke="#60a5fa" strokeWidth="1.5" strokeLinecap="round" opacity="0.45" />
          </svg>
        </div>

        <p className="text-[10px] font-semibold uppercase tracking-[0.28em] text-sky-400/80 mb-2">
          Welcome to Molecule AI
        </p>
        <h2 className="text-xl font-semibold text-ink mb-1">
          Deploy your first agent
        </h2>
        <p className="text-sm text-ink-mid mb-6 leading-relaxed">
          Pick a template to get started instantly, or create a blank workspace.
        </p>

        {/* Template grid */}
        {loading ? (
          <div className="flex items-center justify-center gap-2 text-xs text-ink-mid py-4">
            <Spinner />
            Loading templates...
          </div>
        ) : templates.length > 0 ? (
          <div className="grid grid-cols-2 sm:grid-cols-3 gap-2.5 mb-4 text-left">
            {templates.map((t) => {
              const tierColor = TIER_CONFIG[t.tier]?.border || TIER_CONFIG[1].border;
              return (
                <button
                  type="button"
                  key={t.id}
                  onClick={() => void deploy(t)}
                  disabled={anyDeploying}
                  className="group rounded-xl border border-line/60 bg-surface-sunken/50 px-3.5 py-3 hover:border-accent/40 hover:bg-surface-sunken/80 transition-all disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:border-line/60 disabled:hover:bg-surface-sunken/50 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70"
                >
                  <div className="flex items-center gap-2 mb-1">
                    <span className="text-sm font-medium text-ink group-hover:text-ink truncate">
                      {deploying === t.id ? "Deploying..." : t.name}
                    </span>
                    <span className={`text-[8px] font-mono font-semibold px-1.5 py-0.5 rounded-md border ${tierColor}`}>
                      T{t.tier}
                    </span>
                  </div>
                  <p className="text-[11px] text-ink-soft line-clamp-2 leading-relaxed">
                    {t.description || "No description"}
                  </p>
                  {t.skill_count > 0 && (
                    <p className="text-[9px] text-ink-soft mt-1.5">
                      {t.skill_count} skill{t.skill_count !== 1 ? "s" : ""}
                      {t.model ? ` · ${t.model}` : ""}
                    </p>
                  )}
                </button>
              );
            })}
          </div>
        ) : null}

        {/* Create blank */}
        <button
          type="button"
          onClick={createBlank}
          disabled={anyDeploying}
          className="w-full rounded-xl border border-dashed border-line/60 bg-surface-sunken/30 px-4 py-3 text-sm text-ink-mid hover:text-ink hover:border-line hover:bg-surface-sunken/50 transition-all disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:text-ink-mid disabled:hover:border-line/60 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70"
        >
          {blankCreating ? "Creating..." : "+ Create blank workspace"}
        </button>

        {/* Org templates — instantiate a whole team in one click */}
        <div className="mt-4 pt-4 border-t border-line/50 text-left">
          <OrgTemplatesSection />
        </div>

        {displayError && (
          <div role="alert" className="mt-3 px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-xs text-bad">
            {displayError}
          </div>
        )}

        {/* Missing-keys preflight modal — owned by useTemplateDeploy,
            shared with TemplatePalette. Rendered inline here so it
            overlays this card naturally. */}
        {modal}

        {/* Tips */}
        <div className="mt-5 pt-4 border-t border-line/50">
          <div className="flex items-center justify-center gap-6 text-[10px] text-ink-mid">
            <span>Drag to nest workspaces into teams</span>
            <span className="text-ink-soft">|</span>
            <span>Right-click for actions</span>
            <span className="text-ink-soft">|</span>
            <span>Press <kbd className="px-1 py-0.5 bg-surface-card rounded text-ink-soft font-mono">&#8984;K</kbd> to search</span>
          </div>
        </div>
      </div>
    </div>
  );
}
