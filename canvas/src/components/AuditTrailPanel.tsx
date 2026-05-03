'use client';

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import type { AuditEntry, AuditResponse } from "@/types/audit";

// ── Constants ─────────────────────────────────────────────────────────────────

type EventFilter = "all" | AuditEntry["event_type"];

const BADGE_COLORS: Record<AuditEntry["event_type"], { text: string; bg: string; border: string }> = {
  delegation: { text: "text-accent",   bg: "bg-blue-950/40",   border: "border-blue-800/40" },
  decision:   { text: "text-violet-400", bg: "bg-violet-950/40", border: "border-violet-800/40" },
  gate:       { text: "text-yellow-400", bg: "bg-yellow-950/40", border: "border-yellow-800/40" },
  hitl:       { text: "text-orange-400", bg: "bg-orange-950/40", border: "border-orange-800/40" },
};

const FILTERS: { id: EventFilter; label: string }[] = [
  { id: "all",        label: "All" },
  { id: "delegation", label: "Delegation" },
  { id: "decision",   label: "Decision" },
  { id: "gate",       label: "Gate" },
  { id: "hitl",       label: "HITL" },
];

const AUDIT_LIMIT = 50;

// ── Helpers ───────────────────────────────────────────────────────────────────

/**
 * Format an ISO timestamp as a human-readable relative time string.
 * Exported so unit tests can call it directly without rendering.
 */
export function formatAuditRelativeTime(iso: string, now = Date.now()): string {
  const diff = now - new Date(iso).getTime();
  if (diff < 60_000)      return "just now";
  if (diff < 3_600_000)   return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000)  return `${Math.floor(diff / 3_600_000)}h ago`;
  return new Date(iso).toLocaleDateString();
}

// ── Component ─────────────────────────────────────────────────────────────────

interface Props {
  workspaceId: string;
}

/**
 * AuditTrailPanel — side-panel tab showing the workspace audit ledger.
 *
 * Features:
 * - Color-coded event-type badges (delegation/decision/gate/hitl)
 * - chain_valid=false tamper ⚠ indicator
 * - Event-type filter bar
 * - Cursor-based "Load more" pagination
 * - Relative timestamps refreshed every 30 s
 * - Empty state with icon
 */
export function AuditTrailPanel({ workspaceId }: Props) {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<EventFilter>("all");
  // Relative-time "now" — refreshed every 30 s to keep labels current
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, []);

  // ── URL builder (stable between renders when inputs unchanged) ─────────────

  const buildUrl = useCallback(
    (cursorParam?: string | null): string => {
      const params = new URLSearchParams();
      params.set("limit", String(AUDIT_LIMIT));
      if (filter !== "all") params.set("event_type", filter);
      if (cursorParam) params.set("cursor", cursorParam);
      return `/workspaces/${workspaceId}/audit?${params.toString()}`;
    },
    [workspaceId, filter]
  );

  // ── Initial load (and on filter change) ───────────────────────────────────

  const loadEntries = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<AuditResponse>(buildUrl());
      setEntries(data.entries ?? []);
      setCursor(data.cursor ?? null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load audit trail");
      setEntries([]);
      setCursor(null);
    } finally {
      setLoading(false);
    }
  }, [buildUrl]);

  useEffect(() => {
    loadEntries();
  }, [loadEntries]);

  // ── Pagination (append next page) ─────────────────────────────────────────

  const loadMore = useCallback(async () => {
    if (!cursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const data = await api.get<AuditResponse>(buildUrl(cursor));
      setEntries((prev) => [...prev, ...(data.entries ?? [])]);
      setCursor(data.cursor ?? null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load more entries");
    } finally {
      setLoadingMore(false);
    }
  }, [cursor, loadingMore, buildUrl]);

  // ── Render ─────────────────────────────────────────────────────────────────

  if (loading) {
    return (
      <div className="flex items-center justify-center h-32">
        <span className="text-xs text-ink-soft">Loading audit trail…</span>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      {/* Filter bar */}
      <div className="px-4 py-2.5 border-b border-line/40 flex items-center gap-1 overflow-x-auto shrink-0">
        {FILTERS.map((f) => (
          <button
            type="button"
            key={f.id}
            onClick={() => setFilter(f.id)}
            aria-pressed={filter === f.id}
            className={`px-2 py-1 text-[10px] rounded-md font-medium transition-all shrink-0 ${
              filter === f.id
                ? "bg-surface-card text-ink ring-1 ring-zinc-600"
                : "text-ink-soft hover:text-ink-mid hover:bg-surface-card/60"
            }`}
          >
            {f.label}
          </button>
        ))}
        <div className="flex-1" />
        <button
          type="button"
          onClick={loadEntries}
          className="px-2 py-1 text-[10px] bg-surface-card hover:bg-surface-card text-ink-mid rounded transition-colors shrink-0"
          aria-label="Refresh audit trail"
        >
          ↻
        </button>
      </div>

      {/* Error banner */}
      {error && (
        <div className="mx-4 mt-3 px-3 py-2 bg-red-950/30 border border-red-800/40 rounded text-xs text-bad shrink-0">
          {error}
        </div>
      )}

      {/* Content */}
      <div className="flex-1 overflow-y-auto p-4">
        {entries.length === 0 ? (
          /* Empty state */
          <div className="flex flex-col items-center justify-center py-16 gap-3 text-center">
            <span className="text-4xl text-ink-soft" aria-hidden="true">⊟</span>
            <p className="text-sm font-medium text-ink-mid">No audit events yet</p>
            <p className="text-[11px] text-ink-soft max-w-[200px] leading-relaxed">
              Delegation, decision, gate, and human-in-the-loop events will appear here.
            </p>
          </div>
        ) : (
          <>
            <div className="space-y-1.5" role="list" aria-label="Audit events">
              {entries.map((entry) => (
                <AuditEntryRow key={entry.id} entry={entry} now={now} />
              ))}
            </div>

            {/* Load more */}
            {cursor && (
              <div className="mt-4 flex justify-center">
                <button
                  type="button"
                  onClick={loadMore}
                  disabled={loadingMore}
                  className="px-4 py-2 text-[11px] bg-surface-card hover:bg-surface-card disabled:opacity-50 disabled:cursor-not-allowed text-ink-mid rounded-lg transition-colors"
                >
                  {loadingMore ? "Loading…" : "Load more"}
                </button>
              </div>
            )}

            {/* Entry count footer */}
            <p className="mt-3 text-center text-[9px] text-ink-soft">
              {entries.length} event{entries.length !== 1 ? "s" : ""} loaded
              {cursor ? " · more available" : " · all loaded"}
            </p>
          </>
        )}
      </div>
    </div>
  );
}

// ── AuditEntryRow sub-component ───────────────────────────────────────────────

export interface AuditEntryRowProps {
  entry: AuditEntry;
  now: number;
}

/**
 * Single audit-trail entry row.
 * Exported so tests can render it in isolation without the full panel.
 */
export function AuditEntryRow({ entry, now }: AuditEntryRowProps) {
  const badge = BADGE_COLORS[entry.event_type] ?? {
    text: "text-ink-mid",
    bg: "bg-surface-card/40",
    border: "border-line/40",
  };

  return (
    <div
      role="listitem"
      className="rounded-lg border border-line/60 bg-surface-sunken/50 px-3 py-2.5 space-y-1.5"
    >
      {/* Header row: badge · actor · tamper flag · timestamp */}
      <div className="flex items-center gap-2">
        {/* Event-type badge */}
        <span
          className={`shrink-0 text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded border ${badge.text} ${badge.bg} ${badge.border}`}
          aria-label={`Event type: ${entry.event_type}`}
        >
          {entry.event_type}
        </span>

        {/* Actor name */}
        <span className="text-[10px] text-ink-mid truncate flex-1 min-w-0 font-mono">
          {entry.actor}
        </span>

        {/* Tamper warning — only rendered when chain is invalid */}
        {!entry.chain_valid && (
          <span
            className="shrink-0 text-[11px] text-bad font-bold leading-none"
            title="Chain integrity check failed — this entry may have been tampered with"
            aria-label="Chain integrity warning: tampered entry"
            role="img"
          >
            ⚠
          </span>
        )}

        {/* Relative timestamp */}
        <span className="shrink-0 text-[9px] text-ink-soft">
          {formatAuditRelativeTime(entry.created_at, now)}
        </span>
      </div>

      {/* Summary text */}
      <p className="text-[11px] text-ink-mid leading-relaxed break-words">
        {entry.summary}
      </p>
    </div>
  );
}
