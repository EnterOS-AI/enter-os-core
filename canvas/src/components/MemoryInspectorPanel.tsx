'use client';

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { MemoryEditorDialog } from "@/components/MemoryEditorDialog";

// ── Types ─────────────────────────────────────────────────────────────────────

/** Memory entry returned by GET /workspaces/:id/memories */
export interface MemoryEntry {
  id: string;
  workspace_id: string;
  content: string;
  scope: "LOCAL" | "TEAM" | "GLOBAL";
  namespace: string;
  created_at: string;
  /**
   * Semantic similarity score (0–1). Only present when the API is queried
   * with ?q=<query> and the pgvector backend has been deployed.
   * Absent on plain list fetches — renders gracefully without a badge.
   */
  similarity_score?: number;
}

type Scope = "LOCAL" | "TEAM" | "GLOBAL";
const SCOPES: Scope[] = ["LOCAL", "TEAM", "GLOBAL"];

interface Props {
  workspaceId: string;
}

// ── Helpers ───────────────────────────────────────────────────────────────────

/**
 * Sanitise a memory id for use in an HTML id attribute.
 */
function sanitizeId(id: string): string {
  return id.replace(/[^a-zA-Z0-9]/g, "-");
}

function formatRelativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h`;
  return new Date(iso).toLocaleDateString();
}

// ── Skeleton rows ──────────────────────────────────────────────────────────────

function MemorySkeletonRows() {
  return (
    <div className="space-y-1.5" aria-busy="true" aria-label="Loading entries">
      {Array.from({ length: 3 }).map((_, i) => (
        <div
          key={i}
          className="rounded-lg border border-line/60 bg-surface-sunken/50 px-3 py-3 animate-pulse"
        >
          <div className="flex items-center gap-2">
            <div className="h-2 rounded bg-surface-card/50 flex-1" />
            <div className="h-2 rounded bg-surface-card/50 w-8" />
            <div className="h-2 rounded bg-surface-card/50 w-6" />
            <div className="h-2 rounded bg-surface-card/50 w-10" />
          </div>
        </div>
      ))}
    </div>
  );
}

// ── Component ─────────────────────────────────────────────────────────────────

export function MemoryInspectorPanel({ workspaceId }: Props) {
  const [activeScope, setActiveScope] = useState<Scope>("LOCAL");
  const [activeNamespace, setActiveNamespace] = useState("");
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // ── Search state (debounced) ────────────────────────────────────────────────
  const [searchQuery, setSearchQuery] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");

  useEffect(() => {
    const timer = setTimeout(
      () => setDebouncedQuery(searchQuery.trim()),
      300
    );
    return () => clearTimeout(timer);
  }, [searchQuery]);

  // ── Delete state ─────────────────────────────────────────────────────────────
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);

  // ── Editor state (Add + Edit share one modal) ───────────────────────────────
  type EditorState =
    | { mode: "add" }
    | { mode: "edit"; entry: MemoryEntry }
    | null;
  const [editorState, setEditorState] = useState<EditorState>(null);

  // ── Data loading ────────────────────────────────────────────────────────────

  const loadEntries = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      params.set("scope", activeScope);
      if (debouncedQuery) params.set("q", debouncedQuery);
      if (activeNamespace) params.set("namespace", activeNamespace);

      const url = `/workspaces/${workspaceId}/memories?${params.toString()}`;
      const data = await api.get<MemoryEntry[]>(url);

      // When a semantic query is active, sort by similarity_score descending.
      const sorted = debouncedQuery
        ? [...data].sort(
            (a, b) => (b.similarity_score ?? 0) - (a.similarity_score ?? 0)
          )
        : data;
      setEntries(sorted);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load memories");
      setEntries([]);
    } finally {
      setLoading(false);
    }
  }, [workspaceId, activeScope, debouncedQuery, activeNamespace]);

  useEffect(() => {
    loadEntries();
  }, [loadEntries]);

  // ── Delete handlers ─────────────────────────────────────────────────────────

  const confirmDelete = useCallback(async () => {
    if (!pendingDeleteId) return;
    const id = pendingDeleteId;
    setPendingDeleteId(null);

    // Optimistic removal
    setEntries((prev) => prev.filter((e) => e.id !== id));

    try {
      await api.del(`/workspaces/${workspaceId}/memories/${encodeURIComponent(id)}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Delete failed — reloading...");
      await loadEntries();
    }
  }, [pendingDeleteId, workspaceId, loadEntries]);

  // ── Render ──────────────────────────────────────────────────────────────────

  if (loading && entries.length === 0 && !error) {
    return (
      <div className="flex items-center justify-center h-32">
        <span className="text-xs text-ink-soft">Loading memories…</span>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      {/* Scope tabs */}
      <div className="px-4 pt-3 pb-2 border-b border-line/40 shrink-0">
        <div className="flex items-center gap-1">
          {SCOPES.map((scope) => (
            <button
              type="button"
              key={scope}
              onClick={() => setActiveScope(scope)}
              aria-pressed={activeScope === scope}
              className={[
                "px-3 py-1 text-[11px] rounded transition-colors",
                activeScope === scope
                  ? "bg-accent-strong text-white"
                  : "bg-surface-card text-ink-mid hover:bg-surface-card hover:text-ink",
              ].join(" ")}
            >
              {scope}
            </button>
          ))}
        </div>
      </div>

      {/* Search bar + namespace filter */}
      <div className="px-4 pt-3 pb-2 border-b border-line/40 shrink-0 space-y-2">
        <div className="relative flex items-center">
          {/* Magnifying glass icon */}
          <svg
            width="12"
            height="12"
            viewBox="0 0 16 16"
            fill="none"
            className="absolute left-2.5 text-ink-soft pointer-events-none shrink-0"
            aria-hidden="true"
          >
            <circle cx="7" cy="7" r="4.5" stroke="currentColor" strokeWidth="1.5" />
            <path d="M11 11l3 3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
          <input
            type="search"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="Semantic search…"
            aria-label="Search memories"
            className="w-full bg-surface-sunken border border-line/60 focus:border-accent/60 rounded-lg pl-8 pr-7 py-1.5 text-[11px] text-ink placeholder-zinc-600 focus:outline-none transition-colors"
          />
          {searchQuery && (
            <button
              type="button"
              onClick={() => {
                setSearchQuery("");
                setDebouncedQuery("");
              }}
              aria-label="Clear search"
              className="absolute right-2 text-ink-soft hover:text-ink transition-colors text-sm leading-none"
            >
              ×
            </button>
          )}
        </div>

        {/* Namespace filter */}
        <div className="flex items-center gap-2">
          <label htmlFor="namespace-filter" className="text-[10px] text-ink-soft shrink-0">
            Namespace:
          </label>
          <input
            id="namespace-filter"
            type="text"
            value={activeNamespace}
            onChange={(e) => setActiveNamespace(e.target.value)}
            placeholder="all namespaces"
            aria-label="Filter by namespace"
            className="flex-1 bg-surface-sunken border border-line/60 focus:border-accent/60 rounded px-2 py-1 text-[11px] text-ink placeholder-zinc-600 focus:outline-none transition-colors min-w-0"
          />
        </div>
      </div>

      {/* Toolbar */}
      <div className="px-4 py-2.5 border-b border-line/40 flex items-center justify-between shrink-0">
        <span className="text-[11px] text-ink-soft">
          {debouncedQuery
            ? `${entries.length} result${entries.length !== 1 ? "s" : ""}`
            : entries.length === 1
            ? "1 memory"
            : `${entries.length} memories`}
        </span>
        <div className="flex items-center gap-1.5">
          <button
            type="button"
            onClick={() => setEditorState({ mode: "add" })}
            className="px-2 py-1 text-[11px] bg-accent hover:bg-accent-strong text-white rounded transition-colors"
            aria-label="Add memory"
          >
            + Add
          </button>
          <button
            type="button"
            onClick={loadEntries}
            className="px-2 py-1 text-[11px] bg-surface-card hover:bg-surface-card text-ink-mid rounded transition-colors"
            aria-label="Refresh memories"
          >
            ↻ Refresh
          </button>
        </div>
      </div>

      {/* Error banner */}
      {error && (
        <div
          role="alert"
          aria-live="assertive"
          className="mx-4 mt-3 px-3 py-2 bg-red-950/30 border border-red-800/40 rounded text-xs text-bad shrink-0"
        >
          {error}
        </div>
      )}

      {/* Content */}
      <div className="flex-1 overflow-y-auto p-4">
        {loading ? (
          <MemorySkeletonRows />
        ) : entries.length === 0 ? (
          debouncedQuery ? (
            <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
              <span className="text-4xl text-ink-soft" aria-hidden="true">◇</span>
              <p className="text-sm font-medium text-ink-mid">
                No memories match your search
              </p>
              <p className="text-[11px] text-ink-soft max-w-[200px] leading-relaxed">
                Try a different query or{" "}
                <button
                  type="button"
                  onClick={() => {
                    setSearchQuery("");
                    setDebouncedQuery("");
                  }}
                  className="text-accent hover:text-accent underline transition-colors"
                >
                  clear the search
                </button>
                .
              </p>
            </div>
          ) : (
            <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
              <span className="text-4xl text-ink-soft" aria-hidden="true">◇</span>
              <p className="text-sm font-medium text-ink-mid">No {activeScope} memories</p>
              <p className="text-[11px] text-ink-soft max-w-[200px] leading-relaxed">
                {activeScope === "LOCAL"
                  ? "This workspace has not written any local memories yet."
                  : activeScope === "TEAM"
                  ? "No team memories shared with this workspace yet."
                  : "No global memories exist yet."}
              </p>
            </div>
          )
        ) : (
          <div className="space-y-1.5">
            {entries.map((entry) => (
              <MemoryEntryRow
                key={entry.id}
                entry={entry}
                onEdit={() => setEditorState({ mode: "edit", entry })}
                onDelete={() => setPendingDeleteId(entry.id)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Delete confirmation dialog */}
      <ConfirmDialog
        open={pendingDeleteId !== null}
        title="Delete memory"
        message={`Delete this ${activeScope} memory? This cannot be undone.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDeleteId(null)}
      />

      {/* Add / Edit dialog */}
      {editorState?.mode === "add" && (
        <MemoryEditorDialog
          open={true}
          mode="add"
          workspaceId={workspaceId}
          defaultScope={activeScope}
          defaultNamespace={activeNamespace || "general"}
          onClose={() => setEditorState(null)}
          onSaved={loadEntries}
        />
      )}
      {editorState?.mode === "edit" && (
        <MemoryEditorDialog
          open={true}
          mode="edit"
          workspaceId={workspaceId}
          entry={editorState.entry}
          onClose={() => setEditorState(null)}
          onSaved={loadEntries}
        />
      )}
    </div>
  );
}

// ── MemoryEntryRow sub-component ──────────────────────────────────────────────

interface MemoryEntryRowProps {
  entry: MemoryEntry;
  onEdit: () => void;
  onDelete: () => void;
}

function MemoryEntryRow({ entry, onEdit, onDelete }: MemoryEntryRowProps) {
  const [expanded, setExpanded] = useState(false);
  const bodyId = `mem-body-${sanitizeId(entry.id)}`;

  return (
    <div className="rounded-lg border border-line/60 bg-surface-sunken/50 overflow-hidden">
      {/* Header row */}
      <button
        type="button"
        className="w-full flex items-center gap-2 px-3 py-2.5 text-left hover:bg-surface-card/30 transition-colors"
        onClick={() => setExpanded((prev) => !prev)}
        aria-expanded={expanded}
        aria-controls={bodyId}
      >
        {/* Scope badge */}
        <span
          className={[
            "text-[9px] shrink-0 font-mono px-1 py-0.5 rounded",
            entry.scope === "LOCAL"
              ? "bg-surface-card text-ink-mid"
              : entry.scope === "TEAM"
              ? "bg-blue-950 text-accent"
              : "bg-violet-950 text-violet-400",
          ].join(" ")}
          title={`Scope: ${entry.scope}`}
        >
          {entry.scope[0]}
        </span>

        {/* Namespace tag */}
        <span className="text-[9px] shrink-0 font-mono text-ink-soft truncate max-w-[80px]" title={entry.namespace}>
          {entry.namespace}
        </span>

        {/* Content preview */}
        <span className="flex-1 min-w-0 text-[10px] font-mono text-ink-mid truncate text-left">
          {entry.content.length > 60 ? entry.content.slice(0, 60) + "…" : entry.content}
        </span>

        {/* Similarity badge */}
        {entry.similarity_score != null && (
          <span
            className={[
              "text-[9px] shrink-0 font-mono tabular-nums",
              entry.similarity_score >= 0.8
                ? "text-accent"
                : "text-ink-mid",
            ].join(" ")}
            title={`Similarity: ${(entry.similarity_score * 100).toFixed(1)}%`}
            data-testid="similarity-badge"
          >
            {Math.round(entry.similarity_score * 100)}%
          </span>
        )}

        <span className="text-[9px] text-ink-soft shrink-0">
          {formatRelativeTime(entry.created_at)}
        </span>
        <span className="text-[9px] text-ink-soft shrink-0" aria-hidden="true">
          {expanded ? "▼" : "▶"}
        </span>
      </button>

      {/* Expanded body */}
      {expanded && (
        <div
          id={bodyId}
          role="region"
          aria-label="Memory details"
          className="border-t border-line/50 px-3 pb-3 pt-2 space-y-2"
        >
          <pre className="text-[10px] font-mono text-ink-mid bg-surface rounded p-2 overflow-x-auto max-h-48 whitespace-pre-wrap break-all">
            {entry.content}
          </pre>
          <div className="flex items-center justify-between gap-2">
            <span className="text-[9px] text-ink-soft">
              Created: {new Date(entry.created_at).toLocaleString()}
            </span>
            <div className="flex items-center gap-1.5 shrink-0">
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onEdit();
                }}
                aria-label="Edit memory"
                className="text-[10px] px-2 py-0.5 bg-surface-card hover:bg-surface-elevated border border-line/40 rounded text-ink-mid hover:text-ink transition-colors"
              >
                Edit
              </button>
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onDelete();
                }}
                aria-label="Delete memory"
                className="text-[10px] px-2 py-0.5 bg-red-950/40 hover:bg-red-900/50 border border-red-900/30 rounded text-bad transition-colors"
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
