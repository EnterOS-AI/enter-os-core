"use client";

import { useCallback, useState, type ReactNode } from "react";
import { api } from "@/lib/api";
import {
  checkDeploySecrets,
  resolveRuntime,
  type PreflightResult,
  type Template,
} from "@/lib/deploy-preflight";
import { MissingKeysModal } from "@/components/MissingKeysModal";

/**
 * useTemplateDeploy — shared preflight + POST + modal wiring for
 * every surface that deploys a workspace from a template.
 *
 * Owns: `checkDeploySecrets` call, `MissingKeysModal` render, the
 * `POST /workspaces` that follows, and per-template `deploying`
 * state. Returns `modal` as a `ReactNode` ready to place inline.
 *
 * Why a hook rather than two copies: the runtime-fallback table
 * (`resolveRuntime`) and the preflight wiring were previously
 * copy-pasted between TemplatePalette and EmptyState. When the
 * copies drifted (palette had the full id-to-runtime map,
 * empty-state had only the `-default` strip), the two surfaces
 * could silently disagree on future templates that need a
 * non-identity mapping. Single owner closes the drift surface.
 */
export interface UseTemplateDeployOptions {
  /** Compute canvas coords for the new workspace. Called once per
   *  successful deploy. Defaults to random coords in the [100, 500] ×
   *  [100, 400] band, matching the sidebar palette's historical
   *  placement. Override for surfaces that want deterministic
   *  placement (e.g. EmptyState's first-deploy "center-ish" target). */
  canvasCoords?: () => { x: number; y: number };

  /** Optional post-deploy side effect — passed the id of the new
   *  workspace. EmptyState uses this to auto-select the node and
   *  flip the side panel to Chat so a fresh tenant sees something
   *  useful. */
  onDeployed?: (workspaceId: string) => void;
}

/** Paired template + preflight result carried through the "user
 *  clicked deploy → modal opens → keys saved → retry" loop. Named
 *  so the `useState` generic and any future signature change have
 *  a single place to track. `preflight.configuredKeys` lets the
 *  modal mark pre-saved entries without re-prompting — the
 *  template-deploy "always ask" flow surfaces the picker even when
 *  preflight.ok is true so the user can pick a different provider
 *  per workspace. */
interface MissingKeysInfo {
  template: Template;
  preflight: PreflightResult;
}

export interface UseTemplateDeployResult {
  /** Template id currently being deployed (incl. the preflight
   *  network call), or null when idle. Callers pass this to disable
   *  the relevant button and show a spinner. */
  deploying: string | null;

  /** Last deploy error message, or null. Cleared on next `deploy`
   *  call. */
  error: string | null;

  /** Kick off a deploy. Opens the missing-keys modal if preflight
   *  returns not-ok; otherwise fires POST /workspaces directly. */
  deploy: (template: Template) => Promise<void>;

  /** The missing-keys modal, ready to place inline. Always non-null
   *  (the underlying component self-gates on `open`), so the caller
   *  can drop `{modal}` anywhere without conditionals. */
  modal: ReactNode;
}

export function useTemplateDeploy(
  options: UseTemplateDeployOptions = {},
): UseTemplateDeployResult {
  const [deploying, setDeploying] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [missingKeysInfo, setMissingKeysInfo] = useState<MissingKeysInfo | null>(null);

  const { canvasCoords, onDeployed } = options;

  /** Actually execute the POST /workspaces call. Split from `deploy`
   *  so the "modal → keys added → retry" path can reuse it without
   *  re-running preflight (the user just proved the keys are now set).
   *
   *  `model` (optional) is the user-picked model slug from the picker
   *  modal. When the template is multi-provider, hermes-style routing
   *  reads the slug prefix at install time to pick the upstream
   *  endpoint, so the slug must reach the workspace verbatim. */
  const executeDeploy = useCallback(
    async (template: Template, model?: string) => {
      setDeploying(template.id);
      setError(null);
      try {
        const coords = canvasCoords
          ? canvasCoords()
          : {
              x: Math.random() * 400 + 100,
              y: Math.random() * 300 + 100,
            };
        const ws = await api.post<{ id: string }>("/workspaces", {
          name: template.name,
          template: template.id,
          tier: template.tier,
          canvas: coords,
          ...(model ? { model } : {}),
        });
        onDeployed?.(ws.id);
      } catch (e) {
        setError(e instanceof Error ? e.message : "Deploy failed");
      } finally {
        setDeploying(null);
      }
    },
    [canvasCoords, onDeployed],
  );

  const deploy = useCallback(
    async (template: Template) => {
      setDeploying(template.id);
      setError(null);
      let preflight: PreflightResult;
      try {
        const runtime = template.runtime ?? resolveRuntime(template.id);
        preflight = await checkDeploySecrets({
          runtime,
          models: template.models,
          required_env: template.required_env,
        });
      } catch (e) {
        // Preflight network failure used to strand `deploying` — the
        // button stayed disabled forever because the throw bypassed
        // the setDeploying(null) in the non-ok branch below. Any
        // future refactor that drops this try block will regress the
        // same way; keep it narrow around just the preflight call
        // so a successful preflight still lets executeDeploy own
        // its own error path.
        setError(e instanceof Error ? e.message : "Preflight check failed");
        setDeploying(null);
        return;
      }
      // Always open the picker — every deploy goes through an
      // explicit confirm-provider/model step. Reasons:
      //   1. Multi-provider templates (e.g. hermes) need a per-
      //      workspace pick or the adapter falls back to its
      //      compiled-in default and 500s with "No LLM provider
      //      configured".
      //   2. Single-provider templates (claude-code, langgraph)
      //      still need the model field — the template's default
      //      may be wrong for the user's billing tier or a model
      //      they explicitly want (sonnet vs opus vs haiku).
      //   3. Even when keys + model are pre-filled, surfacing the
      //      modal one-click-away is the cheapest UX for catching
      //      a misconfigured org BEFORE provisioning an EC2 that
      //      will then sit in degraded.
      // The picker handles the "all-keys-saved single-provider"
      // case as a confirm-only prompt (provider radio is hidden,
      // model input is pre-filled with template.model).
      setMissingKeysInfo({ template, preflight });
      setDeploying(null);
    },
    [],
  );

  // No useCallback here — consumers call this on every render anyway
  // (it's placed inline in JSX), and useCallback's deps would
  // invalidate on every state change, making the memoisation a wash.
  // Plain ReactNode is simpler and equally performant.
  const isMultiProvider = (missingKeysInfo?.preflight.providers.length ?? 0) >= 2;
  // Suggestions for the model field — pull declared model ids from the
  // template. Templates without `models` declared (e.g. claude-code)
  // pass [] which suppresses the model field entirely.
  const modelSuggestions =
    missingKeysInfo?.template.models?.map((m) => m.id) ?? [];
  // Pre-fill the model input with the template's default `model` so
  // confirming without changing it preserves today's behaviour.
  const initialModel = missingKeysInfo?.template.model;
  // When the user has keys configured (preflight.ok) we re-purpose the
  // modal as a "confirm provider/model" prompt — adjust copy
  // accordingly so it doesn't claim keys are missing.
  const allConfigured = missingKeysInfo?.preflight.ok ?? false;
  const modalTitle = allConfigured
    ? "Configure Workspace"
    : undefined;
  const modalDescription = allConfigured
    ? "Pick the provider and model for this workspace. Saved API keys are reused automatically."
    : undefined;
  const modal: ReactNode = (
    <MissingKeysModal
      open={!!missingKeysInfo}
      missingKeys={missingKeysInfo?.preflight.missingKeys ?? []}
      providers={missingKeysInfo?.preflight.providers ?? []}
      runtime={missingKeysInfo?.preflight.runtime ?? ""}
      configuredKeys={missingKeysInfo?.preflight.configuredKeys}
      modelSuggestions={isMultiProvider ? modelSuggestions : undefined}
      // Pass full model specs (id + required_env) so the picker can
      // auto-snap the provider radio when the user picks a model — fixes
      // the "type MiniMax model, see ANTHROPIC_API_KEY" cascade bug.
      // Only relevant in multi-provider mode where the model field is
      // shown.
      models={isMultiProvider ? missingKeysInfo?.template.models : undefined}
      initialModel={isMultiProvider ? initialModel : undefined}
      title={modalTitle}
      description={modalDescription}
      onKeysAdded={(model?: string) => {
        if (missingKeysInfo) {
          const template = missingKeysInfo.template;
          setMissingKeysInfo(null);
          // Intentional fire-and-forget — executeDeploy manages
          // its own error state via setError.
          void executeDeploy(template, model);
        }
      }}
      onCancel={() => setMissingKeysInfo(null)}
    />
  );

  return { deploying, error, deploy, modal };
}
