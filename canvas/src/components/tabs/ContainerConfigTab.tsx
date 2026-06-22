"use client";

import { useEffect, useMemo, useState } from "react";
import { api } from "@/lib/api";
import {
  FALLBACK_COMPUTE_OPTIONS,
  type ComputeOptions,
  defaultInstanceForProvider,
  instanceTypesForProvider,
  normalizeProvider,
  parseComputeOptions,
} from "@/lib/compute-options";
import { runtimeDisplayName } from "@/lib/runtime-names";
import { isSaaSTenant } from "@/lib/tenant";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import type { WorkspaceCompute } from "@/store/socket";

const RUNTIME_OPTIONS = ["claude-code", "codex", "hermes", "openclaw", "kimi", "kimi-cli", "external"];
const RESOLUTIONS = ["1280x720", "1440x900", "1920x1080", "2560x1440"];
const DEFAULT_HEADLESS_ROOT_GB = 30;

type Props = {
  workspaceId: string;
  data: Pick<
    WorkspaceNodeData,
    "runtime" | "status" | "needsRestart" | "activeTasks" | "deliveryMode"
    | "workspaceAccess" | "maxConcurrentTasks" | "compute" | "applyTemplateOnRestart"
  >;
};

type FormState = {
  runtime: string;
  provider: string; // cloud backend; editable in SaaS (in-place switch recreates the box)
  instanceType: string;
  rootGB: string;
  displayEnabled: boolean;
  displayMode: string;
  displayProtocol: string;
  resolution: string;
  dataPersistence: string; // "" (auto) | "persist" | "ephemeral" — internal#734
};

// internal#734: per-workspace durable-data choice. "" = auto (desktop-control
// keeps data, others follow the org default). Human labels for the selector.
const DATA_PERSISTENCE_OPTIONS = ["", "persist", "ephemeral"];
const dataPersistenceLabel = (v: string): string =>
  v === "persist" ? "Always keep (persist)" : v === "ephemeral" ? "Don't keep (ephemeral)" : "Auto";

// Cloud/compute backend display name (read-only fallback for non-SaaS / legacy).
const cloudProviderLabel = (v: string | undefined): string =>
  v === "gcp" ? "GCP" : v === "hetzner" ? "Hetzner" : "AWS";

export function ContainerConfigTab({ workspaceId, data }: Props) {
  // Provider is editable only in SaaS (CP-provisioned boxes). Local/Docker has no
  // cloud-provider concept, so we keep the read-only badge there.
  const isSaaS = useMemo(() => isSaaSTenant(), []);
  const runtime = data.runtime;
  const provider = data.compute?.provider;
  const instanceType = data.compute?.instance_type;
  const rootGB = data.compute?.volume?.root_gb;
  const displayMode = data.compute?.display?.mode;
  const displayProtocol = data.compute?.display?.protocol;
  const displayWidth = data.compute?.display?.width;
  const displayHeight = data.compute?.display?.height;
  const dataPersistence = data.compute?.data_persistence;
  const initial = useMemo(
    () => formFromData({ runtime, provider, instanceType, rootGB, displayMode, displayProtocol, displayWidth, displayHeight, dataPersistence }),
    [runtime, provider, instanceType, rootGB, displayMode, displayProtocol, displayWidth, displayHeight, dataPersistence],
  );
  const [form, setForm] = useState<FormState>(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);
  // core#2489: provider + instance-type dropdowns are populated from the
  // workspace-server SSOT (GET /compute/metadata) so they can't drift from
  // what the PATCH validation accepts. Start from the offline fallback and
  // replace it once the fetch resolves; on fetch error we keep the fallback
  // (the dropdowns still work, just from the in-bundle mirror).
  const [computeOptions, setComputeOptions] = useState<ComputeOptions>(FALLBACK_COMPUTE_OPTIONS);

  useEffect(() => {
    setForm(initial);
    setError(null);
    setSuccess(false);
  }, [initial]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        // /compute/metadata is a public, workspace-independent endpoint (the data
        // is platform constraints, not org secrets) — no need to refetch on
        // workspaceId change; one fetch per tab mount is enough.
        const resp = await api.get<unknown>("/compute/metadata");
        if (cancelled) return;
        // Defensive: only adopt a well-formed payload; otherwise keep the fallback.
        // Map the server's per-provider object shape into the flat internal
        // ComputeOptions shape the helpers + selectors consume.
        const parsed = parseComputeOptions(resp);
        if (parsed) {
          setComputeOptions(parsed);
        }
      } catch {
        // Fetch failed (offline / older server) — keep FALLBACK_COMPUTE_OPTIONS.
        // The dropdowns stay usable; worst case they show the in-bundle mirror.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const workspaceAccess = formatAccess(data.workspaceAccess);
  const maxConcurrentTasks = data.maxConcurrentTasks ? String(data.maxConcurrentTasks) : "platform-managed";
  const deliveryMode = data.deliveryMode || "push";
  const dirty = JSON.stringify(form) !== JSON.stringify(initial);
  const restartLabel = dirty ? "Save & Restart" : "Restart to apply";
  const resolutionOptions = RESOLUTIONS.includes(form.resolution)
    ? RESOLUTIONS
    : [form.resolution, ...RESOLUTIONS];

  const save = async (restart: boolean) => {
    setError(null);
    setSuccess(false);

    setSaving(true);
    try {
      let applyTemplateOnRestart = data.applyTemplateOnRestart ?? false;
      if (dirty) {
        // In-place cloud switch is DESTRUCTIVE: changing the provider recreates the
        // box on the new cloud (the workspace-server deprovisions the old box on
        // its old cloud first, then the restart provisions on the new one). Confirm
        // before doing it — the current box and any non-persisted state are lost.
        const providerChanged = normalizeProvider(form.provider) !== normalizeProvider(initial.provider);
        if (providerChanged && typeof window !== "undefined") {
          const ok = window.confirm(
            `Switch this workspace to ${cloudProviderLabel(form.provider)}? This RECREATES the box on the new cloud — the current box and any non-persisted state are replaced.`,
          );
          if (!ok) {
            setSaving(false);
            return;
          }
        }

        const rootGB = parseInt(form.rootGB, 10);
        if (!Number.isFinite(rootGB)) {
          setError("Root volume must be a number");
          return;
        }

        const [width, height] = form.resolution.split("x").map((v) => parseInt(v, 10));
        const compute: WorkspaceCompute = {
          instance_type: form.instanceType,
          volume: { root_gb: rootGB },
          display: form.displayEnabled
            ? { mode: form.displayMode, protocol: form.displayProtocol, width, height }
            : { mode: "none" },
          // internal#734: omit when "auto" so the wire/default behavior is unchanged.
          ...(form.dataPersistence ? { data_persistence: form.dataPersistence } : {}),
          // Cloud backend: send the (possibly switched) provider. Omit for the
          // default (aws) so a non-switching AWS save keeps the wire unchanged;
          // a switch TO aws (omit) vs FROM aws (explicit) both register correctly
          // because the workspace-server normalizes ""→aws when diffing.
          ...(normalizeProvider(form.provider) !== "aws" ? { provider: normalizeProvider(form.provider) } : {}),
        };

        const resp = await api.patch<{ needs_restart?: boolean }>(`/workspaces/${workspaceId}`, {
          runtime: form.runtime,
          compute,
        });
        useCanvasStore.getState().updateNodeData(workspaceId, {
          runtime: form.runtime,
          compute,
          needsRestart: resp.needs_restart ?? true,
          applyTemplateOnRestart: form.runtime !== initial.runtime,
        });
        applyTemplateOnRestart = form.runtime !== initial.runtime;
      }

      if (restart) {
        await useCanvasStore.getState().restartWorkspace(workspaceId, {
          applyTemplate: applyTemplateOnRestart,
        });
      }
      setSuccess(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="p-4 space-y-4">
      <section className="rounded-lg border border-line/50 bg-surface-card/40 p-4">
        <div className="mb-3 flex items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold text-ink">Container Config</h3>
            {/* Non-SaaS (local/Docker) has no cloud-provider concept → read-only
                badge. In SaaS the provider is an editable selector in the form. */}
            {!isSaaS && (
              <span
                title="Cloud provider for this workspace's compute"
                className="rounded-full border border-line/60 bg-surface-sunken px-2 py-0.5 font-mono text-[10px] uppercase tracking-wide text-ink-mid"
              >
                {cloudProviderLabel(provider)}
              </span>
            )}
          </div>
          {data.needsRestart && <span className="text-[11px] text-warm">Restart required</span>}
        </div>

        <div className="grid grid-cols-1 gap-3 text-[11px]">
          <SelectField
            id="runtime-image-profile"
            label="Runtime image"
            value={form.runtime}
            options={RUNTIME_OPTIONS}
            optionLabel={runtimeDisplayName}
            onChange={(runtime) => setForm((s) => ({ ...s, runtime }))}
          />
          {isSaaS && (
            <SelectField
              id="cloud-provider"
              label="Cloud provider"
              value={normalizeProvider(form.provider)}
              options={computeOptions.providers}
              optionLabel={(v) => cloudProviderLabel(v)}
              // Switching cloud resets the instance type to the new provider's
              // default (an AWS t3.* is invalid on Hetzner, etc.) — also keeps the
              // instance-type dropdown below in sync with the provider's sizes.
              onChange={(provider) =>
                setForm((s) => ({
                  ...s,
                  provider,
                  instanceType: instanceTypesForProvider(computeOptions, provider).includes(s.instanceType)
                    ? s.instanceType
                    : defaultInstanceForProvider(computeOptions, provider),
                }))
              }
            />
          )}
          <SelectField
            id="instance-type"
            label="Instance type"
            value={form.instanceType}
            options={instanceTypesForProvider(computeOptions, form.provider)}
            onChange={(instanceType) => setForm((s) => ({ ...s, instanceType }))}
          />
          <label className="grid gap-1" htmlFor="root-volume-gb">
            <span className="text-ink-mid">Root volume</span>
            <div className="flex items-center gap-2">
              <input
                id="root-volume-gb"
                aria-label="Root volume"
                type="number"
                min={30}
                max={500}
                value={form.rootGB}
                onChange={(e) => setForm((s) => ({ ...s, rootGB: e.target.value }))}
                className="min-w-0 flex-1 rounded-md border border-line/60 bg-surface-sunken px-3 py-2 font-mono text-ink outline-none focus:border-accent"
              />
              <span className="text-ink-mid">GB</span>
            </div>
          </label>
          <label className="flex items-center justify-between gap-3 rounded-md bg-surface-sunken/40 px-3 py-2">
            <span className="text-ink-mid">Display</span>
            <input
              type="checkbox"
              aria-label="Enable display"
              checked={form.displayEnabled}
              onChange={(e) => setForm((s) => ({
                ...s,
                displayEnabled: e.target.checked,
                displayMode: e.target.checked && s.displayMode === "none" ? "desktop-control" : s.displayMode,
                displayProtocol: e.target.checked && !s.displayProtocol ? "novnc" : s.displayProtocol,
              }))}
              className="h-4 w-4 accent-accent"
            />
          </label>
          {form.displayEnabled && (
            <SelectField
              id="display-resolution"
              label="Resolution"
              value={form.resolution}
              options={resolutionOptions}
              onChange={(resolution) => setForm((s) => ({ ...s, resolution }))}
            />
          )}
          <SelectField
            id="data-persistence"
            label="Saved data (cookies, downloads, memory)"
            value={form.dataPersistence}
            options={DATA_PERSISTENCE_OPTIONS}
            optionLabel={dataPersistenceLabel}
            onChange={(dataPersistence) => setForm((s) => ({ ...s, dataPersistence }))}
          />
          <p className="-mt-1 text-[10px] leading-snug text-ink-soft">
            Whether this workspace&apos;s data survives a restart/recreate. Auto keeps it for
            browser (desktop) workspaces; Ephemeral never keeps it (privacy).
          </p>
        </div>

        <div className="mt-4 flex items-center justify-end gap-2">
          {error && <span className="mr-auto text-[11px] text-bad">{error}</span>}
          {success && <span className="mr-auto text-[11px] text-good">Saved</span>}
          <button
            type="button"
            disabled={!dirty || saving}
            onClick={() => setForm(initial)}
            className="rounded-md border border-line/60 px-3 py-2 text-[11px] text-ink-mid disabled:cursor-not-allowed disabled:opacity-50"
          >
            Reset
          </button>
          <button
            type="button"
            disabled={!dirty || saving}
            onClick={() => save(false)}
            className="rounded-md bg-accent px-3 py-2 text-[11px] font-medium text-white disabled:cursor-not-allowed disabled:opacity-50"
          >
            {saving ? "Saving..." : "Save"}
          </button>
          <button
            type="button"
            disabled={(!dirty && !data.needsRestart) || saving}
            onClick={() => save(true)}
            className="rounded-md bg-ink px-3 py-2 text-[11px] font-medium text-surface disabled:cursor-not-allowed disabled:opacity-50"
          >
            {saving ? "Restarting..." : restartLabel}
          </button>
        </div>
      </section>

      <section className="rounded-lg border border-line/50 bg-surface-card/40 p-4">
        <h3 className="mb-3 text-sm font-semibold text-ink">Status</h3>
        <dl className="grid grid-cols-1 gap-2 text-[11px]">
          <ConfigRow label="Container status" value={data.status} />
          <ConfigRow label="Active tasks" value={String(data.activeTasks ?? 0)} />
          <ConfigRow label="Workspace access" value={workspaceAccess} />
          <ConfigRow label="Max concurrent tasks" value={maxConcurrentTasks} />
          <ConfigRow label="Mounted workspace path" value="/workspace" />
          <ConfigRow label="Delivery mode" value={deliveryMode} />
        </dl>
      </section>
    </div>
  );
}

function formFromData(data: {
  runtime?: string;
  provider?: string;
  instanceType?: string;
  rootGB?: number;
  displayMode?: string;
  displayProtocol?: string;
  displayWidth?: number;
  displayHeight?: number;
  dataPersistence?: string;
}): FormState {
  const width = data.displayWidth ?? 1920;
  const height = data.displayHeight ?? 1080;
  const resolution = `${width}x${height}`;
  const provider = normalizeProvider(data.provider);
  return {
    runtime: data.runtime || "claude-code",
    provider,
    // Falls back to the offline default only when no instance type is persisted;
    // the server SSOT default matches FALLBACK_COMPUTE_OPTIONS, and the dropdown
    // re-syncs to the fetched options once they resolve.
    instanceType: data.instanceType || defaultInstanceForProvider(FALLBACK_COMPUTE_OPTIONS, provider),
    rootGB: String(data.rootGB || DEFAULT_HEADLESS_ROOT_GB),
    displayEnabled: !!data.displayMode && data.displayMode !== "none",
    displayMode: data.displayMode && data.displayMode !== "none" ? data.displayMode : "desktop-control",
    displayProtocol: data.displayProtocol || "novnc",
    resolution,
    dataPersistence: data.dataPersistence || "",
  };
}

function SelectField({
  id,
  label,
  value,
  options,
  optionLabel = (v: string) => v,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  options: string[];
  optionLabel?: (value: string) => string;
  onChange: (value: string) => void;
}) {
  return (
    <label className="grid gap-1" htmlFor={id}>
      <span className="text-ink-mid">{label}</span>
      <select
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-md border border-line/60 bg-surface-sunken px-3 py-2 font-mono text-ink outline-none focus:border-accent"
      >
        {options.map((option) => (
          <option key={option} value={option}>
            {optionLabel(option)}
          </option>
        ))}
      </select>
    </label>
  );
}

function formatAccess(value: string | null | undefined): string {
  if (!value) return "none";
  return value.replace(/_/g, "-");
}

function ConfigRow({
  label,
  value,
}: {
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-start justify-between gap-3 rounded-md bg-surface-sunken/40 px-3 py-2">
      <dt className="text-ink-mid">{label}</dt>
      <dd className="min-w-0 text-right">
        <div className="font-mono text-ink break-words">{value}</div>
      </dd>
    </div>
  );
}
