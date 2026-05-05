"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "@/lib/api";

interface Props {
  workspaceId: string;
}

interface MemoryEntry {
  key: string;
  value: unknown;
  version?: number;
  expires_at: string | null;
  updated_at: string;
}

const AWARENESS_BASE_URL =
  process.env.NEXT_PUBLIC_AWARENESS_URL || "http://localhost:37800";

export function MemoryTab({ workspaceId }: Props) {
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [showAwareness, setShowAwareness] = useState(true);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [newKey, setNewKey] = useState("");
  const [newValue, setNewValue] = useState("");
  const [newTTL, setNewTTL] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [editingKey, setEditingKey] = useState<string | null>(null);
  const [editValue, setEditValue] = useState("");
  const [editTTL, setEditTTL] = useState("");
  const [editError, setEditError] = useState<string | null>(null);

  const awarenessUrl = useMemo(() => {
    try {
      const url = new URL(AWARENESS_BASE_URL);
      url.searchParams.set("workspaceId", workspaceId);
      return url.toString();
    } catch {
      return AWARENESS_BASE_URL;
    }
  }, [workspaceId]);

  const awarenessStatus = useMemo(() => {
    try {
      const url = new URL(AWARENESS_BASE_URL);
      return url.origin.includes("localhost") ? "local" : url.hostname;
    } catch {
      return "unavailable";
    }
  }, []);

  const loadMemory = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<MemoryEntry[]>(`/workspaces/${workspaceId}/memory`);
      setEntries(data);
    } catch (e) {
      setEntries([]);
      setError(e instanceof Error ? e.message : "Failed to load memory");
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    loadMemory();
  }, [loadMemory]);

  const handleAdd = async () => {
    setError(null);
    if (!newKey.trim()) {
      setError("Key is required");
      return;
    }

    let parsedValue: unknown;
    try {
      parsedValue = JSON.parse(newValue);
    } catch {
      parsedValue = newValue;
    }

    const body: Record<string, unknown> = { key: newKey, value: parsedValue };
    if (newTTL) {
      const ttl = parseInt(newTTL);
      if (!Number.isNaN(ttl) && ttl > 0) body.ttl_seconds = ttl;
    }

    try {
      await api.post(`/workspaces/${workspaceId}/memory`, body);
      setNewKey("");
      setNewValue("");
      setNewTTL("");
      setShowAdd(false);
      loadMemory();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to add");
    }
  };

  const handleDelete = async (key: string) => {
    setError(null);
    try {
      await api.del(`/workspaces/${workspaceId}/memory/${encodeURIComponent(key)}`);
      setEntries((prev) => prev.filter((e) => e.key !== key));
      if (expanded === key) setExpanded(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to delete entry");
    }
  };

  const beginEdit = (entry: MemoryEntry) => {
    setEditError(null);
    setEditingKey(entry.key);
    // Stringify objects/arrays as pretty JSON; render plain strings raw so the
    // editor doesn't surprise users with surrounding quotes.
    setEditValue(
      typeof entry.value === "string"
        ? entry.value
        : JSON.stringify(entry.value, null, 2),
    );
    if (entry.expires_at) {
      const remainingMs = new Date(entry.expires_at).getTime() - Date.now();
      const ttl = Math.max(0, Math.floor(remainingMs / 1000));
      setEditTTL(ttl > 0 ? String(ttl) : "");
    } else {
      setEditTTL("");
    }
  };

  const cancelEdit = () => {
    setEditingKey(null);
    setEditValue("");
    setEditTTL("");
    setEditError(null);
  };

  const handleEditSave = async (entry: MemoryEntry) => {
    setEditError(null);

    let parsedValue: unknown;
    try {
      parsedValue = JSON.parse(editValue);
    } catch {
      parsedValue = editValue;
    }

    // if_match_version closes the silent-overwrite hole when two writers
    // race. The handler returns 409 with the current version on mismatch
    // — surface that as a retry hint and reload to pick up the new state.
    const body: Record<string, unknown> = { key: entry.key, value: parsedValue };
    if (typeof entry.version === "number") {
      body.if_match_version = entry.version;
    }
    if (editTTL) {
      const ttl = parseInt(editTTL);
      if (!Number.isNaN(ttl) && ttl > 0) body.ttl_seconds = ttl;
    }

    try {
      await api.post(`/workspaces/${workspaceId}/memory`, body);
      cancelEdit();
      loadMemory();
    } catch (e) {
      const message = e instanceof Error ? e.message : "Failed to save";
      if (message.includes("409") || /if_match_version mismatch/i.test(message)) {
        setEditError("This entry changed since you opened it. Reloading.");
        loadMemory();
      } else {
        setEditError(message);
      }
    }
  };

  const openAwareness = () => {
    window.open(awarenessUrl, "_blank", "noopener,noreferrer");
  };

  if (loading) {
    return <div className="p-4 text-xs text-ink-soft">Loading memory...</div>;
  }

  return (
    <div className="p-4 space-y-4">
      {error && !showAdd && (
        <div role="alert" className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
          {error}
        </div>
      )}

      <section className="space-y-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <div className="text-xs font-medium text-ink">Awareness dashboard</div>
            <p className="text-[10px] text-ink-soft">
              Embedded view for the local Awareness memory UI. The current workspace id is appended to the URL for workspace-scoped routing or future filtering.
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setShowAwareness((prev) => !prev)}
              className="shrink-0 px-2 py-1 bg-surface-card hover:bg-surface-elevated text-[10px] rounded text-ink"
            >
              {showAwareness ? "Collapse" : "Expand"}
            </button>
            <button
              type="button"
              onClick={openAwareness}
              className="shrink-0 px-2 py-1 bg-surface-card hover:bg-surface-elevated text-[10px] rounded text-ink"
            >
              Open
            </button>
          </div>
        </div>

        {showAwareness ? (
          AWARENESS_BASE_URL ? (
            <div className="overflow-hidden rounded-xl border border-line bg-surface-sunken/70 shadow-[0_0_0_1px_rgba(255,255,255,0.02)]">
              <iframe
                title="Awareness dashboard"
                src={awarenessUrl}
                className="h-[520px] w-full border-0"
                loading="lazy"
              />
            </div>
          ) : (
            <div className="rounded-xl border border-dashed border-line bg-surface-sunken/40 p-4 text-xs text-ink-soft">
              Set <code className="font-mono text-ink-mid">NEXT_PUBLIC_AWARENESS_URL</code> to embed the Awareness dashboard here.
            </div>
          )
        ) : (
          <div className="rounded-xl border border-line bg-surface-sunken/50 px-4 py-3 flex items-center justify-between gap-3">
            <div className="min-w-0">
              <p className="text-xs text-ink">Awareness dashboard is collapsed</p>
              <p className="text-[10px] text-ink-soft truncate">
                Workspace context stays linked through <span className="font-mono text-ink-mid">{workspaceId}</span>.
              </p>
            </div>
            <button
              type="button"
              onClick={() => setShowAwareness(true)}
              className="shrink-0 px-2 py-1 bg-accent hover:bg-accent-strong text-[10px] rounded text-white"
            >
              Expand
            </button>
          </div>
        )}

        <div className="grid gap-2 rounded-xl border border-line bg-surface/40 px-3 py-2 text-[10px] text-ink-mid sm:grid-cols-3">
          <div className="flex items-center justify-between gap-2">
            <span className="uppercase tracking-[0.18em] text-ink-soft">Status</span>
            <span className="font-medium text-good">Connected</span>
          </div>
          <div className="flex items-center justify-between gap-2">
            <span className="uppercase tracking-[0.18em] text-ink-soft">Mode</span>
            <span className="font-medium text-ink">{awarenessStatus}</span>
          </div>
          <div className="flex items-center justify-between gap-2 min-w-0">
            <span className="uppercase tracking-[0.18em] text-ink-soft">Workspace</span>
            <span className="font-mono text-ink-mid truncate">{workspaceId}</span>
          </div>
        </div>
      </section>

      <section className="space-y-3 border-t border-line/60 pt-4">
        <div className="flex items-center justify-between">
          <div>
            <div className="text-xs font-medium text-ink">Workspace KV memory</div>
            <p className="text-[10px] text-ink-soft">
              Native platform key-value memory for workspace <span className="font-mono text-ink-mid">{workspaceId}</span>.
            </p>
          </div>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={() => setShowAdvanced((prev) => !prev)}
              className="px-2 py-1 bg-surface-card hover:bg-surface-elevated text-[10px] rounded text-ink-mid"
            >
              {showAdvanced ? "Hide Advanced" : "Advanced"}
            </button>
            <button
              type="button"
              onClick={loadMemory}
              className="px-2 py-1 bg-surface-card hover:bg-surface-elevated text-[10px] rounded text-ink-mid"
            >
              Refresh
            </button>
            <button
              type="button"
              onClick={() => { setShowAdd(!showAdd); if (!showAdd) setShowAdvanced(true); }}
              className="px-2 py-1 bg-accent hover:bg-accent-strong text-[10px] rounded text-white"
            >
              + Add
            </button>
          </div>
        </div>

        {showAdvanced && showAdd && (
          <div className="bg-surface-card rounded p-3 space-y-2 border border-line">
            <input
              value={newKey}
              onChange={(e) => setNewKey(e.target.value)}
              placeholder="Key"
              aria-label="Memory key"
              className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
            />
            <textarea
              value={newValue}
              onChange={(e) => setNewValue(e.target.value)}
              placeholder='Value (JSON or plain text)'
              rows={3}
              aria-label="Memory value (JSON or plain text)"
              className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-xs font-mono text-ink focus:outline-none focus:border-accent resize-none"
            />
            <input
              value={newTTL}
              onChange={(e) => setNewTTL(e.target.value)}
              placeholder="TTL in seconds (optional)"
              aria-label="TTL in seconds (optional)"
              className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
            />
            {error && <div role="alert" className="text-xs text-bad">{error}</div>}
            <div className="flex gap-2">
              <button
                type="button"
                onClick={handleAdd}
                className="px-3 py-1 bg-accent hover:bg-accent-strong text-xs rounded text-white"
              >
                Save
              </button>
              <button
                type="button"
                onClick={() => {
                  setShowAdd(false);
                  setError(null);
                }}
                className="px-3 py-1 bg-surface-card hover:bg-surface-elevated text-xs rounded text-ink-mid"
              >
                Cancel
              </button>
            </div>
          </div>
        )}

        {showAdvanced ? (
          entries.length === 0 ? (
            <p className="text-xs text-ink-soft text-center py-4">No memory entries</p>
          ) : (
            <div className="space-y-1">
              {entries.map((entry) => (
                <div key={entry.key} className="bg-surface-card rounded border border-line">
                  <button
                    type="button"
                    onClick={() => setExpanded(expanded === entry.key ? null : entry.key)}
                    className="w-full flex items-center justify-between px-3 py-2 text-left"
                    aria-expanded={expanded === entry.key}
                  >
                    <span className="text-xs font-mono text-accent">{entry.key}</span>
                    <div className="flex items-center gap-2">
                      {entry.expires_at && (
                        <span className="text-[9px] text-ink-soft">
                          TTL {new Date(entry.expires_at).toLocaleString()}
                        </span>
                      )}
                      <span className="text-[10px] text-ink-soft">
                        {expanded === entry.key ? "▼" : "▶"}
                      </span>
                    </div>
                  </button>

                  {expanded === entry.key && (
                    <div className="px-3 pb-2 space-y-2">
                      {editingKey === entry.key ? (
                        <div className="space-y-2">
                          <textarea
                            value={editValue}
                            onChange={(e) => setEditValue(e.target.value)}
                            rows={4}
                            aria-label={`Edit value for ${entry.key}`}
                            className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-xs font-mono text-ink focus:outline-none focus:border-accent resize-none"
                          />
                          <input
                            value={editTTL}
                            onChange={(e) => setEditTTL(e.target.value)}
                            placeholder="TTL in seconds (blank = no expiry)"
                            aria-label={`Edit TTL for ${entry.key}`}
                            className="w-full bg-surface-sunken border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent"
                          />
                          {editError && (
                            <div role="alert" className="text-[10px] text-bad">
                              {editError}
                            </div>
                          )}
                          <div className="flex gap-2">
                            <button
                              type="button"
                              onClick={() => handleEditSave(entry)}
                              className="px-3 py-1 bg-accent hover:bg-accent-strong text-xs rounded text-white"
                            >
                              Save
                            </button>
                            <button
                              type="button"
                              onClick={cancelEdit}
                              className="px-3 py-1 bg-surface-card hover:bg-surface-elevated text-xs rounded text-ink-mid"
                            >
                              Cancel
                            </button>
                          </div>
                        </div>
                      ) : (
                        <pre className="text-[10px] text-ink-mid bg-surface-sunken rounded p-2 overflow-x-auto max-h-40">
                          {JSON.stringify(entry.value, null, 2)}
                        </pre>
                      )}
                      <div className="flex items-center justify-between">
                        <span className="text-[9px] text-ink-soft">
                          Updated: {new Date(entry.updated_at).toLocaleString()}
                        </span>
                        <div className="flex items-center gap-2">
                          {editingKey !== entry.key && (
                            <button
                              type="button"
                              onClick={() => beginEdit(entry)}
                              className="text-[10px] text-ink-mid hover:bg-surface-elevated rounded px-1 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
                            >
                              Edit
                            </button>
                          )}
                          <button
                            type="button"
                            onClick={() => handleDelete(entry.key)}
                            className="text-[10px] text-bad hover:bg-red-950/40 rounded px-1 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-red-500/60"
                          >
                            Delete
                          </button>
                        </div>
                      </div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )
        ) : (
          <div className="rounded-xl border border-line bg-surface/30 px-4 py-3 flex items-center justify-between gap-3">
            <div className="min-w-0">
              <p className="text-xs text-ink">Advanced workspace memory is hidden</p>
              <p className="text-[10px] text-ink-soft truncate">
                KV entries remain available if you need the raw platform store.
              </p>
            </div>
            <button
              type="button"
              onClick={() => setShowAdvanced(true)}
              className="shrink-0 px-2 py-1 bg-accent hover:bg-accent-strong text-[10px] rounded text-white"
            >
              Show
            </button>
          </div>
        )}
      </section>
    </div>
  );
}
