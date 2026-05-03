"use client";

import { useState, useEffect, useCallback, useRef, useId, cloneElement, type ReactElement } from "react";
import { api } from "@/lib/api";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { StatusDot } from "../StatusDot";
import { BudgetSection } from "./BudgetSection";
import { WorkspaceUsage } from "../WorkspaceUsage";
import { ConsoleModal } from "../ConsoleModal";

interface Props {
  workspaceId: string;
  data: WorkspaceNodeData;
}

interface PeerData {
  id: string;
  name: string;
  role: string | null;
  status: string;
  tier: number;
}

export function DetailsTab({ workspaceId, data }: Props) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(data.name);
  const [role, setRole] = useState(data.role || "");
  const [tier, setTier] = useState(data.tier);
  const [peers, setPeers] = useState<PeerData[]>([]);
  const [saving, setSaving] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [peersError, setPeersError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);
  const [consoleOpen, setConsoleOpen] = useState(false);
  const updateNodeData = useCanvasStore((s) => s.updateNodeData);
  const removeSubtree = useCanvasStore((s) => s.removeSubtree);
  const selectNode = useCanvasStore((s) => s.selectNode);
  // Ref for the "Delete Workspace" trigger — Cancel returns focus here
  const deleteButtonRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    setName(data.name);
    setRole(data.role || "");
    setTier(data.tier);
  }, [data.name, data.role, data.tier]);

  const loadPeers = useCallback(async () => {
    setPeersError(null);
    try {
      const peerList = await api.get<PeerData[]>(`/registry/${workspaceId}/peers`);
      setPeers(peerList);
    } catch (e) {
      setPeersError(e instanceof Error ? e.message : "Failed to load peers");
    }
  }, [workspaceId]);

  useEffect(() => {
    // The /registry/:id/peers endpoint requires a workspace-scoped
    // bearer token (validateDiscoveryCaller) which the canvas session
    // doesn't hold. For a still-provisioning or failed workspace there
    // are no peers to show anyway — skip the fetch so the Details tab
    // doesn't flood devtools with 401 noise and so the empty Peers
    // section renders cleanly.
    if (data.status !== "online" && data.status !== "degraded") {
      setPeers([]);
      setPeersError(null);
      return;
    }
    loadPeers();
  }, [loadPeers, data.status]);

  const handleSave = async () => {
    setSaving(true);
    setSaveError(null);
    try {
      await api.patch(`/workspaces/${workspaceId}`, {
        name,
        role: role || null,
        tier,
      });
      updateNodeData(workspaceId, { name, role: role || "", tier });
      setEditing(false);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    setDeleteError(null);
    try {
      await api.del(`/workspaces/${workspaceId}?confirm=true`);
      // Mirror the server-side cascade — drop the row + every
      // descendant locally so the canvas reflects the deletion
      // immediately, even when the WS is dead and the per-descendant
      // WORKSPACE_REMOVED events never arrive.
      removeSubtree(workspaceId);
      selectNode(null);
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : "Failed to delete");
    }
  };

  const handleRestart = async () => {
    setRestarting(true);
    setRestartError(null);
    try {
      await api.post(`/workspaces/${workspaceId}/restart`, {});
      updateNodeData(workspaceId, { status: "provisioning" });
    } catch (e) {
      setRestartError(e instanceof Error ? e.message : "Failed to restart");
    } finally {
      setRestarting(false);
    }
  };

  const isRestartable = data.status === "offline" || data.status === "failed" || data.status === "degraded";

  const agentCard = data.agentCard;
  const skills = getSkills(agentCard);

  return (
    <div className="p-4 space-y-4">
      {/* Editable fields */}
      <Section title="Workspace">
        {editing ? (
          <div className="space-y-2">
            <Field label="Name">
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-sm text-ink focus:outline-none focus:border-accent"
              />
            </Field>
            <Field label="Role">
              <input
                value={role}
                onChange={(e) => setRole(e.target.value)}
                placeholder="e.g. SEO Specialist"
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-sm text-ink focus:outline-none focus:border-accent"
              />
            </Field>
            <Field label="Tier">
              <select
                value={tier}
                onChange={(e) => setTier(Number(e.target.value))}
                className="w-full bg-surface-card border border-line rounded px-2 py-1 text-sm text-ink focus:outline-none focus:border-accent"
              >
                <option value={1}>Tier 1 — No privileges</option>
                <option value={2}>Tier 2 — Browser</option>
                <option value={3}>Tier 3 — Desktop</option>
                <option value={4}>Tier 4 — VM</option>
              </select>
            </Field>
            {saveError && (
              <div className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
                {saveError}
              </div>
            )}
            <div className="flex gap-2 pt-1">
              <button
                type="button"
                onClick={handleSave}
                disabled={saving}
                className="px-3 py-1 bg-accent-strong hover:bg-accent text-xs rounded text-white disabled:opacity-50"
              >
                {saving ? "Saving..." : "Save"}
              </button>
              <button
                type="button"
                onClick={() => {
                  setEditing(false);
                  setSaveError(null);
                  setName(data.name);
                  setRole(data.role || "");
                  setTier(data.tier);
                }}
                className="px-3 py-1 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid"
              >
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <div className="space-y-1.5">
            <Row label="Name" value={data.name} />
            <Row label="Role" value={data.role || "—"} />
            <Row label="Tier" value={`T${data.tier}`} />
            <Row label="Status" value={data.status} />
            <Row label="URL" value={data.url || "—"} mono />
            <Row label="Parent" value={data.parentId || "root"} mono />
            <Row label="Active Tasks" value={String(data.activeTasks)} />
            {data.status === "degraded" && (
              <Row label="Error Rate" value={`${((data.lastErrorRate ?? 0) * 100).toFixed(0)}%`} />
            )}
            {isRestartable && (
              <div className="pt-2">
                {restartError && (
                  <div className="mb-2 px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
                    {restartError}
                  </div>
                )}
                <button
                  type="button"
                  onClick={handleRestart}
                  disabled={restarting}
                  className="px-3 py-1 bg-green-700 hover:bg-green-600 text-xs rounded text-white disabled:opacity-50"
                >
                  {restarting ? "Restarting..." : data.status === "failed" ? "Retry" : "Restart"}
                </button>
              </div>
            )}
            <button
              type="button"
              onClick={() => setEditing(true)}
              className="mt-2 px-3 py-1 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid"
            >
              Edit
            </button>
          </div>
        )}
      </Section>

      {/* Error details — shown when the workspace failed to boot. The
          control plane's bootstrap watcher writes last_sample_error with
          the real traceback from the EC2 serial console, so users see
          "ModuleNotFoundError: ..." instead of a generic timeout. */}
      {(data.status === "failed" || (data.status === "degraded" && data.lastSampleError)) && (
        <Section title="Error">
          {data.lastSampleError ? (
            <pre
              data-testid="details-error-log"
              className="text-[11px] text-bad font-mono whitespace-pre-wrap break-all bg-red-950/20 border border-red-900/40 rounded p-2 max-h-[240px] overflow-auto leading-tight"
            >
              {data.lastSampleError}
            </pre>
          ) : (
            <p className="text-xs text-ink-soft">No error detail recorded.</p>
          )}
          <button
            type="button"
            onClick={() => setConsoleOpen(true)}
            className="mt-2 px-3 py-1 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid border border-line"
          >
            View console output
          </button>
        </Section>
      )}

      {/* Budget — dedicated section with live usage stats (#541) */}
      <BudgetSection workspaceId={workspaceId} />

      {/* Token usage + spend — wired to GET /workspaces/:id/metrics (#592) */}
      <WorkspaceUsage workspaceId={workspaceId} />

      {/* Agent Card / Skills */}
      {skills.length > 0 && (
        <Section title="Skills">
          <div className="space-y-1">
            {skills.map((s) => (
              <div key={s.id} className="flex items-start gap-2">
                <span className="text-xs text-accent font-mono shrink-0">{s.id}</span>
                {s.description && (
                  <span className="text-xs text-ink-soft">{s.description}</span>
                )}
              </div>
            ))}
          </div>
        </Section>
      )}

      {/* Peers */}
      <Section title={`Peers (${peers.length})`}>
        {peersError ? (
          <p className="text-xs text-bad">{peersError}</p>
        ) : peers.length === 0 && data.status !== "online" && data.status !== "degraded" ? (
          <p className="text-xs text-ink-soft">
            Peers are only discoverable while the workspace is online.
          </p>
        ) : peers.length === 0 ? (
          <p className="text-xs text-ink-soft">No reachable peers</p>
        ) : (
          <div className="space-y-1">
            {peers.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => selectNode(p.id)}
                className="w-full flex items-center gap-2 px-2 py-1 rounded hover:bg-surface-card text-left"
              >
                <StatusDot status={p.status} />
                <span className="text-xs text-ink">{p.name}</span>
                {p.role && <span className="text-[10px] text-ink-soft">{p.role}</span>}
              </button>
            ))}
          </div>
        )}
      </Section>

      {/* Delete */}
      <Section title="Danger Zone">
        {deleteError && (
          <div className="mb-2 px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
            {deleteError}
          </div>
        )}
        {confirmDelete ? (
          <div
            role="alertdialog"
            aria-modal="true"
            aria-labelledby="delete-confirm-title"
            className="space-y-2"
          >
            <h3 id="delete-confirm-title" className="text-xs font-medium text-bad">
              Confirm deletion
            </h3>
            <div className="flex gap-2">
              <button
                type="button"
                onClick={handleDelete}
                className="px-3 py-1 bg-red-600 hover:bg-red-500 text-xs rounded text-white"
              >
                Confirm Delete
              </button>
              <button
                type="button"
                onClick={() => {
                  setConfirmDelete(false);
                  setDeleteError(null);
                  // Return focus to the trigger so keyboard users aren't stranded
                  deleteButtonRef.current?.focus();
                }}
                className="px-3 py-1 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid"
              >
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <button
            type="button"
            ref={deleteButtonRef}
            onClick={() => setConfirmDelete(true)}
            className="px-3 py-1 bg-surface-card hover:bg-red-900 border border-line hover:border-red-700 text-xs rounded text-ink-mid hover:text-bad transition-colors"
          >
            Delete Workspace
          </button>
        )}
      </Section>

      {/* Portal-rendered console modal — mounted at the root of this tab
          but appears above everything via createPortal(document.body). */}
      <ConsoleModal
        workspaceId={workspaceId}
        workspaceName={data.name}
        open={consoleOpen}
        onClose={() => setConsoleOpen(false)}
      />
    </div>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <h3 className="text-xs font-semibold text-ink-mid uppercase tracking-wider mb-2">{title}</h3>
      {children}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  const fieldId = useId();
  return (
    <div>
      <label htmlFor={fieldId} className="text-[10px] text-ink-soft block mb-0.5">{label}</label>
      {cloneElement(children as ReactElement<{ id?: string }>, { id: fieldId })}
    </div>
  );
}

function Row({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex justify-between">
      <span className="text-xs text-ink-soft">{label}</span>
      <span className={`text-xs text-ink ${mono ? "font-mono" : ""} text-right max-w-[200px] truncate`}>
        {value}
      </span>
    </div>
  );
}

function getSkills(card: Record<string, unknown> | null): { id: string; description?: string }[] {
  if (!card) return [];
  const skills = card.skills;
  if (!Array.isArray(skills)) return [];
  return skills.map((s: Record<string, unknown>) => ({
    id: String(s.id || s.name || ""),
    description: s.description ? String(s.description) : undefined,
  })).filter((s) => s.id);
}
