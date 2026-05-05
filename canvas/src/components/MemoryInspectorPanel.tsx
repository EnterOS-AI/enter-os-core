'use client';

/**
 * MemoryInspectorPanel — Memory v2 redesign.
 *
 * Reads the canvas Memory tab from the v2 plugin via the
 * workspace-server proxy at /v2/{namespaces,memories}, replacing the
 * v1 LOCAL/TEAM/GLOBAL trio that mapped to the deprecated
 * shared_context model.
 *
 * Surface differences from v1:
 *   - Namespace dropdown driven by GET /v2/namespaces (workspace /
 *     team / org / custom — labels rendered server-side).
 *   - Per-row badges for kind (fact|summary|checkpoint), source
 *     (agent|runtime|user), pin (📌), TTL countdown, and propagation
 *     source-workspace if the memory came from a peer.
 *   - No Edit affordance — v2's plugin contract has no PATCH; the
 *     model is forget + recommit. Delete (Forget) stays.
 *
 * Shipping note: when the plugin isn't wired (MEMORY_PLUGIN_URL
 * unset), every endpoint returns 503 with a clear hint. The panel
 * surfaces that as a banner so operators know to set the env var,
 * rather than rendering a perpetual empty state that looks like
 * "no memories yet".
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '@/lib/api';
import { ConfirmDialog } from '@/components/ConfirmDialog';

// ── Types ─────────────────────────────────────────────────────────────────────

export type NamespaceKind = 'workspace' | 'team' | 'org' | 'custom';

export interface NamespaceView {
  name: string;
  kind: NamespaceKind;
  label: string;
}

export interface NamespacesResponse {
  readable: NamespaceView[];
  writable: NamespaceView[];
}

export type MemoryKind = 'fact' | 'summary' | 'checkpoint';
export type MemorySource = 'agent' | 'runtime' | 'user';

export interface MemoryV2 {
  id: string;
  namespace: string;
  content: string;
  kind: MemoryKind;
  source: MemorySource;
  pin: boolean;
  expires_at?: string | null;
  created_at: string;
  /** 0..1 plugin similarity score; only present when ?q= is set. */
  score?: number | null;
  /** workspace_id of the peer that originated this memory if propagation is in play. */
  source_workspace_id?: string;
}

interface MemoriesResponse {
  memories: MemoryV2[];
}

// MemoryEntry kept as a back-compat type alias so any other component
// still importing it doesn't break the build. New consumers should
// prefer MemoryV2 — the v1 shape (LOCAL/TEAM/GLOBAL scope) is gone.
//
// `unknown` is used over `any` so TS still flags accidental field
// access on the legacy shape.
export type MemoryEntry = MemoryV2;

interface Props {
  workspaceId: string;
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function sanitizeId(id: string): string {
  return id.replace(/[^a-zA-Z0-9]/g, '-');
}

function formatRelativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h`;
  return new Date(iso).toLocaleDateString();
}

/**
 * Render a TTL countdown like "12h", "3d", or "expired" (when the
 * stored expires_at is in the past). Non-fatal if expires_at is null
 * or invalid — falls through to empty string so the badge doesn't
 * render.
 */
export function formatTTL(expiresAt: string | null | undefined): string {
  if (!expiresAt) return '';
  const ts = new Date(expiresAt).getTime();
  if (Number.isNaN(ts)) return '';
  const diff = ts - Date.now();
  if (diff <= 0) return 'expired';
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h`;
  return `${Math.floor(diff / 86_400_000)}d`;
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

const ALL_NAMESPACES = '__all__';

export function MemoryInspectorPanel({ workspaceId }: Props) {
  const [namespaces, setNamespaces] = useState<NamespacesResponse | null>(null);
  const [activeNamespace, setActiveNamespace] = useState<string>(ALL_NAMESPACES);
  const [entries, setEntries] = useState<MemoryV2[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Plugin-disabled banner (503 from server). Stored separately so we
  // can keep showing the namespace dropdown empty rather than
  // hiding the whole panel.
  const [pluginUnavailable, setPluginUnavailable] = useState(false);

  // Search state (debounced)
  const [searchQuery, setSearchQuery] = useState('');
  const [debouncedQuery, setDebouncedQuery] = useState('');

  useEffect(() => {
    const timer = setTimeout(() => setDebouncedQuery(searchQuery.trim()), 300);
    return () => clearTimeout(timer);
  }, [searchQuery]);

  // Delete state
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);

  // ── Namespace loading ──────────────────────────────────────────────────────

  const loadNamespaces = useCallback(async () => {
    try {
      const data = await api.get<NamespacesResponse>(
        `/workspaces/${workspaceId}/v2/namespaces`,
      );
      setNamespaces(data);
      setPluginUnavailable(false);
    } catch (e) {
      // 503 indicates the plugin isn't wired. Surface it specially —
      // anything else stays as a generic load failure that the
      // entries-load path will also flag.
      const msg = e instanceof Error ? e.message : '';
      if (msg.includes('503') || msg.toLowerCase().includes('plugin is not configured')) {
        setPluginUnavailable(true);
      }
      setNamespaces({ readable: [], writable: [] });
    }
  }, [workspaceId]);

  // ── Entries loading ────────────────────────────────────────────────────────

  const loadEntries = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      if (activeNamespace !== ALL_NAMESPACES) {
        params.set('namespace', activeNamespace);
      }
      if (debouncedQuery) params.set('q', debouncedQuery);

      const url = `/workspaces/${workspaceId}/v2/memories?${params.toString()}`;
      const data = await api.get<MemoriesResponse>(url);

      // When a semantic query is active and the plugin returns
      // scores, sort by score descending so the most-relevant hit
      // sits at the top. Empty score → push to bottom.
      const sorted = debouncedQuery
        ? [...data.memories].sort(
            (a, b) => (b.score ?? 0) - (a.score ?? 0),
          )
        : data.memories;
      setEntries(sorted);
    } catch (e) {
      const msg = e instanceof Error ? e.message : 'Failed to load memories';
      if (msg.includes('503') || msg.toLowerCase().includes('plugin is not configured')) {
        setPluginUnavailable(true);
        setError(null); // surfaced via banner, not row error
      } else {
        setError(msg);
      }
      setEntries([]);
    } finally {
      setLoading(false);
    }
  }, [workspaceId, activeNamespace, debouncedQuery]);

  useEffect(() => {
    loadNamespaces();
  }, [loadNamespaces]);

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
      await api.del(`/workspaces/${workspaceId}/v2/memories/${encodeURIComponent(id)}`);
    } catch (e) {
      // Reload first (which clears any stale error), THEN set the
      // delete-failure message — otherwise loadEntries' own
      // `setError(null)` wipes our error before the user sees it.
      // Caught by the rollback test in MemoryInspectorPanel.test.tsx.
      const msg = e instanceof Error ? e.message : 'Delete failed — reloading…';
      await loadEntries();
      setError(msg);
    }
  }, [pendingDeleteId, workspaceId, loadEntries]);

  // ── Namespace dropdown options ─────────────────────────────────────────────

  const dropdownOptions = useMemo(() => {
    const opts: Array<{ value: string; label: string; kind?: NamespaceKind }> = [
      { value: ALL_NAMESPACES, label: 'All namespaces' },
    ];
    if (namespaces) {
      for (const ns of namespaces.readable) {
        opts.push({ value: ns.name, label: ns.label, kind: ns.kind });
      }
    }
    return opts;
  }, [namespaces]);

  // ── Render ──────────────────────────────────────────────────────────────────

  if (loading && entries.length === 0 && !error && !pluginUnavailable) {
    return (
      <div className="flex items-center justify-center h-32">
        <span className="text-xs text-ink-soft">Loading memories…</span>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      {/* Plugin-unavailable banner */}
      {pluginUnavailable && (
        <div
          role="alert"
          aria-live="polite"
          className="mx-4 mt-3 px-3 py-2 bg-amber-950/30 border border-amber-800/40 rounded text-xs text-amber-300 shrink-0"
          data-testid="plugin-unavailable-banner"
        >
          Memory plugin not configured. Set <code>MEMORY_PLUGIN_URL</code> on the
          workspace-server to enable v2 memory.
        </div>
      )}

      {/* Namespace dropdown */}
      <div className="px-4 pt-3 pb-2 border-b border-line/40 shrink-0 space-y-2">
        <div className="flex items-center gap-2">
          <label htmlFor="namespace-dropdown" className="text-[10px] text-ink-soft shrink-0">
            Namespace:
          </label>
          <select
            id="namespace-dropdown"
            value={activeNamespace}
            onChange={(e) => setActiveNamespace(e.target.value)}
            aria-label="Filter by namespace"
            disabled={pluginUnavailable}
            className="flex-1 bg-surface-sunken border border-line/60 focus:border-accent/60 rounded px-2 py-1 text-[11px] text-ink focus:outline-none transition-colors min-w-0 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {dropdownOptions.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
                {opt.kind ? `  (${opt.kind})` : ''}
              </option>
            ))}
          </select>
        </div>

        {/* Search bar */}
        <div className="relative flex items-center">
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
            disabled={pluginUnavailable}
            className="w-full bg-surface-sunken border border-line/60 focus:border-accent/60 rounded-lg pl-8 pr-7 py-1.5 text-[11px] text-ink placeholder-zinc-600 focus:outline-none transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          />
          {searchQuery && (
            <button
              type="button"
              onClick={() => {
                setSearchQuery('');
                setDebouncedQuery('');
              }}
              aria-label="Clear search"
              className="absolute right-2 text-ink-soft hover:text-ink transition-colors text-sm leading-none"
            >
              ×
            </button>
          )}
        </div>
      </div>

      {/* Toolbar */}
      <div className="px-4 py-2.5 border-b border-line/40 flex items-center justify-between shrink-0">
        <span className="text-[11px] text-ink-soft">
          {debouncedQuery
            ? `${entries.length} result${entries.length !== 1 ? 's' : ''}`
            : entries.length === 1
              ? '1 memory'
              : `${entries.length} memories`}
        </span>
        <button
          type="button"
          onClick={loadEntries}
          disabled={pluginUnavailable}
          className="px-2 py-1 text-[11px] bg-surface-card hover:bg-surface-card text-ink-mid rounded transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          aria-label="Refresh memories"
        >
          ↻ Refresh
        </button>
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
          <EmptyState query={debouncedQuery} pluginUnavailable={pluginUnavailable} />
        ) : (
          <div className="space-y-1.5">
            {entries.map((entry) => (
              <MemoryEntryRow
                key={entry.id}
                entry={entry}
                onDelete={() => setPendingDeleteId(entry.id)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Delete confirmation dialog */}
      <ConfirmDialog
        open={pendingDeleteId !== null}
        title="Forget memory"
        message="Forget this memory? This cannot be undone."
        confirmLabel="Forget"
        confirmVariant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDeleteId(null)}
      />
    </div>
  );
}

// ── Empty state ─────────────────────────────────────────────────────────────

function EmptyState({
  query,
  pluginUnavailable,
}: {
  query: string;
  pluginUnavailable: boolean;
}) {
  if (pluginUnavailable) {
    // The banner already explains the problem; the empty rows just
    // mirror it so the operator sees both signals.
    return (
      <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
        <span className="text-4xl text-ink-soft" aria-hidden="true">
          ◇
        </span>
        <p className="text-sm font-medium text-ink-mid">Memory plugin disabled</p>
        <p className="text-[11px] text-ink-soft max-w-[220px] leading-relaxed">
          See banner above for the operator-side fix.
        </p>
      </div>
    );
  }
  if (query) {
    return (
      <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
        <span className="text-4xl text-ink-soft" aria-hidden="true">
          ◇
        </span>
        <p className="text-sm font-medium text-ink-mid">No memories match your search</p>
        <p className="text-[11px] text-ink-soft max-w-[200px] leading-relaxed">
          Try a different query or clear the search.
        </p>
      </div>
    );
  }
  return (
    <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
      <span className="text-4xl text-ink-soft" aria-hidden="true">
        ◇
      </span>
      <p className="text-sm font-medium text-ink-mid">No memories yet</p>
      <p className="text-[11px] text-ink-soft max-w-[220px] leading-relaxed">
        Agents commit memories via MCP tools (commit_memory, commit_summary). They
        appear here once written.
      </p>
    </div>
  );
}

// ── MemoryEntryRow sub-component ──────────────────────────────────────────────

interface MemoryEntryRowProps {
  entry: MemoryV2;
  onDelete: () => void;
}

const KIND_BADGE_CLASS: Record<MemoryKind, string> = {
  fact: 'bg-surface-card text-ink-mid',
  summary: 'bg-blue-950 text-accent',
  checkpoint: 'bg-violet-950 text-violet-400',
};

const SOURCE_BADGE_CLASS: Record<MemorySource, string> = {
  agent: 'bg-surface-card text-ink-mid',
  runtime: 'bg-amber-950 text-amber-300',
  user: 'bg-emerald-950 text-emerald-400',
};

function MemoryEntryRow({ entry, onDelete }: MemoryEntryRowProps) {
  const [expanded, setExpanded] = useState(false);
  const bodyId = `mem-body-${sanitizeId(entry.id)}`;
  const ttl = formatTTL(entry.expires_at);

  return (
    <div
      className="rounded-lg border border-line/60 bg-surface-sunken/50 overflow-hidden"
      data-testid={`memory-row-${entry.id}`}
    >
      {/* Header row */}
      <button
        type="button"
        className="w-full flex items-center gap-2 px-3 py-2.5 text-left hover:bg-surface-card/30 transition-colors"
        onClick={() => setExpanded((prev) => !prev)}
        aria-expanded={expanded}
        aria-controls={bodyId}
      >
        {/* Kind badge */}
        <span
          className={[
            'text-[9px] shrink-0 font-mono px-1 py-0.5 rounded',
            KIND_BADGE_CLASS[entry.kind] ?? 'bg-surface-card text-ink-mid',
          ].join(' ')}
          title={`Kind: ${entry.kind}`}
          data-testid="kind-badge"
        >
          {entry.kind[0].toUpperCase()}
        </span>

        {/* Source badge */}
        <span
          className={[
            'text-[9px] shrink-0 font-mono px-1 py-0.5 rounded',
            SOURCE_BADGE_CLASS[entry.source] ?? 'bg-surface-card text-ink-mid',
          ].join(' ')}
          title={`Source: ${entry.source}`}
          data-testid="source-badge"
        >
          {entry.source}
        </span>

        {/* Pin indicator */}
        {entry.pin && (
          <span
            className="text-[9px] shrink-0"
            title="Pinned"
            data-testid="pin-badge"
            aria-label="Pinned"
          >
            📌
          </span>
        )}

        {/* Namespace tag */}
        <span
          className="text-[9px] shrink-0 font-mono text-ink-soft truncate max-w-[100px]"
          title={entry.namespace}
        >
          {entry.namespace}
        </span>

        {/* Content preview */}
        <span className="flex-1 min-w-0 text-[10px] font-mono text-ink-mid truncate text-left">
          {entry.content.length > 60 ? entry.content.slice(0, 60) + '…' : entry.content}
        </span>

        {/* Score badge (semantic search only) */}
        {entry.score != null && (
          <span
            className={[
              'text-[9px] shrink-0 font-mono tabular-nums',
              entry.score >= 0.8 ? 'text-accent' : 'text-ink-mid',
            ].join(' ')}
            title={`Similarity: ${(entry.score * 100).toFixed(1)}%`}
            data-testid="score-badge"
          >
            {Math.round(entry.score * 100)}%
          </span>
        )}

        {/* TTL countdown */}
        {ttl && (
          <span
            className={[
              'text-[9px] shrink-0 font-mono',
              ttl === 'expired' ? 'text-bad' : 'text-amber-400',
            ].join(' ')}
            title={`Expires: ${entry.expires_at}`}
            data-testid="ttl-badge"
          >
            ⌛{ttl}
          </span>
        )}

        {/* Source workspace badge (propagated memory) */}
        {entry.source_workspace_id && (
          <span
            className="text-[9px] shrink-0 font-mono text-violet-400"
            title={`From: ${entry.source_workspace_id}`}
            data-testid="source-workspace-badge"
          >
            ⇡{entry.source_workspace_id.slice(0, 6)}
          </span>
        )}

        <span className="text-[9px] text-ink-soft shrink-0">
          {formatRelativeTime(entry.created_at)}
        </span>
        <span className="text-[9px] text-ink-soft shrink-0" aria-hidden="true">
          {expanded ? '▼' : '▶'}
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
              {entry.expires_at && ` · Expires: ${new Date(entry.expires_at).toLocaleString()}`}
            </span>
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onDelete();
              }}
              aria-label="Forget memory"
              className="text-[10px] px-2 py-0.5 bg-red-950/40 hover:bg-red-900/50 border border-red-900/30 rounded text-bad transition-colors shrink-0"
            >
              Forget
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
