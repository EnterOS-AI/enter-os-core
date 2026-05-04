"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";

interface Props {
  workspaceId: string;
}

interface EventEntry {
  id: string;
  event_type: string;
  workspace_id: string | null;
  payload: Record<string, unknown>;
  created_at: string;
}

// Use semantic warm-paper tokens so colors flip with theme. Earlier
// the table referenced text-yellow-400 / text-purple-400 (Tailwind
// raw colors, no theme variant), which read fine in dark mode but
// washed out in the warm-paper light theme. text-warm covers the
// "degraded" amber tone in both modes; AGENT_CARD_UPDATED is informational
// metadata, so reuse text-accent for theme-consistency.
const EVENT_COLORS: Record<string, string> = {
  WORKSPACE_ONLINE: "text-good",
  WORKSPACE_OFFLINE: "text-ink-mid",
  WORKSPACE_DEGRADED: "text-warm",
  WORKSPACE_PROVISIONING: "text-accent",
  WORKSPACE_REMOVED: "text-bad",
  WORKSPACE_PROVISION_FAILED: "text-bad",
  AGENT_CARD_UPDATED: "text-accent",
};

export function EventsTab({ workspaceId }: Props) {
  const [events, setEvents] = useState<EventEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const loadEvents = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<EventEntry[]>(`/events/${workspaceId}`);
      setEvents(data);
    } catch (e) {
      setEvents([]);
      setError(e instanceof Error ? e.message : "Failed to load events");
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    loadEvents();
  }, [loadEvents]);

  // Auto-refresh every 10s
  useEffect(() => {
    const interval = setInterval(loadEvents, 10000);
    return () => clearInterval(interval);
  }, [loadEvents]);

  if (loading && events.length === 0) {
    return <div className="p-4 text-xs text-ink-soft">Loading events...</div>;
  }

  return (
    <div className="p-4 space-y-2">
      <div className="flex items-center justify-between mb-2">
        <span className="text-xs text-ink-mid">{events.length} events</span>
        <button
          type="button"
          onClick={loadEvents}
          // Was hover:bg-surface-card on top of bg-surface-card — silent
          // no-op hover. Lift to surface-elevated, matching the Cancel
          // pattern from ConfirmDialog.
          className="px-2 py-1 bg-surface-card hover:bg-surface-elevated hover:text-ink text-[10px] rounded text-ink-mid transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Refresh
        </button>
      </div>

      {error && (
        <div className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
          {error}
        </div>
      )}

      {!error && events.length === 0 ? (
        <p className="text-xs text-ink-soft text-center py-4">No events yet</p>
      ) : (
        <div className="space-y-1">
          {events.map((event) => {
            const isOpen = expanded === event.id;
            const panelId = `events-payload-${event.id}`;
            return (
              <div key={event.id} className="bg-surface-card rounded border border-line">
                <button
                  type="button"
                  onClick={() => setExpanded(isOpen ? null : event.id)}
                  // aria-expanded + aria-controls so screen readers
                  // announce the open/closed state and link the row to
                  // its payload panel. Without these, AT users hear
                  // a generic "button" with no indication that it
                  // toggles or what it controls.
                  aria-expanded={isOpen}
                  aria-controls={panelId}
                  className="w-full flex items-center gap-2 px-3 py-2 text-left rounded-t hover:bg-surface-elevated/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/50 transition-colors"
                >
                  <span
                    className={`text-xs font-mono ${
                      EVENT_COLORS[event.event_type] || "text-ink-mid"
                    }`}
                  >
                    {event.event_type}
                  </span>
                  <span className="text-[9px] text-ink-soft ml-auto">
                    {formatTime(event.created_at)}
                  </span>
                  <span aria-hidden="true" className="text-[10px] text-ink-soft">
                    {isOpen ? "▼" : "▶"}
                  </span>
                </button>

                {isOpen && (
                  <div id={panelId} className="px-3 pb-2">
                    <pre className="text-[10px] text-ink-mid bg-surface-sunken rounded p-2 overflow-x-auto max-h-40">
                      {JSON.stringify(event.payload, null, 2)}
                    </pre>
                    <div className="mt-1 text-[9px] text-ink-soft font-mono">
                      ID: {event.id}
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function formatTime(iso: string): string {
  const d = new Date(iso);
  const now = new Date();
  const diff = now.getTime() - d.getTime();

  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
  return d.toLocaleDateString();
}
