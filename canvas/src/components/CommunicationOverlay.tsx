"use client";

import { useState, useEffect, useCallback, useRef } from "react";
import { useCanvasStore } from "@/store/canvas";
import { api } from "@/lib/api";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { COMM_TYPE_LABELS } from "@/lib/design-tokens";

interface Communication {
  id: string;
  sourceId: string;
  targetId: string;
  sourceName: string;
  targetName: string;
  type: "a2a_send" | "a2a_receive" | "task_update";
  summary: string;
  status: string;
  timestamp: string;
  durationMs: number | null;
}

/** Workspace-server `ACTIVITY_LOGGED` payload shape. Pulled out so the
 *  WS handler below has a typed view of the same fields the HTTP
 *  bootstrap consumes — drift between the two paths is a class of bug
 *  AgentCommsPanel hit historically. */
interface ActivityLoggedPayload {
  id?: string;
  activity_type?: string;
  source_id?: string | null;
  target_id?: string | null;
  workspace_id?: string;
  summary?: string | null;
  status?: string;
  duration_ms?: number | null;
  created_at?: string;
}

/** Fan-out cap for the bootstrap HTTP fetch on mount / on visibility
 *  re-open. Kept at 3 (carried over from the 2026-05-04 fix) so a
 *  freshly-mounted overlay on a 15-workspace tenant only spends 3
 *  round-trips bootstrapping. Live updates after that arrive via the
 *  WS subscription below — no polling, no fan-out to maintain. */
const BOOTSTRAP_FAN_OUT_CAP = 3;

/** Cap on the rendered list. Bootstrap + every WS push prepends, the
 *  list is sliced to this size after each update. Mirrors the prior
 *  polling-loop behaviour. */
const COMMS_RENDER_CAP = 20;

/**
 * Overlay showing recent A2A communications between workspaces.
 *
 * Update shape (issue #61 Stage 1, replaces the 30s polling loop):
 *  - On mount (when visible): one HTTP bootstrap per online workspace,
 *    capped at BOOTSTRAP_FAN_OUT_CAP. Yields the initial recent-comms
 *    window without waiting for live events.
 *  - Steady state: subscribes to ACTIVITY_LOGGED via useSocketEvent.
 *    Each event with a matching activity_type from a visible online
 *    workspace gets synthesised into a Communication and prepended.
 *  - Visibility re-open: re-bootstraps so the user sees the freshest
 *    window even if WS was idle while collapsed.
 *
 * No interval poll. The singleton ReconnectingSocket in `store/socket.ts`
 * already owns reconnect/backoff/health-check, and `useSocketEvent`
 * inherits those guarantees. If WS is genuinely unhealthy, the overlay
 * shows the bootstrap snapshot until the next visibility re-open or
 * the next WS reconnect (which fires its own rehydrate burst).
 */
export function CommunicationOverlay() {
  const [comms, setComms] = useState<Communication[]>([]);
  const [visible, setVisible] = useState(true);
  const selectedNodeId = useCanvasStore((s) => s.selectedNodeId);
  const nodes = useCanvasStore((s) => s.nodes);
  // nodesRef gives the WS handler current node-name resolution without
  // re-subscribing on every node-list change. The bus listener is
  // registered exactly once per mount; subscriber-side filtering reads
  // the latest value via this ref.
  const nodesRef = useRef(nodes);
  nodesRef.current = nodes;

  const bootstrapComms = useCallback(async () => {
    try {
      const onlineNodes = nodesRef.current.filter((n) => n.data.status === "online");
      const allComms: Communication[] = [];

      for (const node of onlineNodes.slice(0, BOOTSTRAP_FAN_OUT_CAP)) {
        try {
          const activities = await api.get<Array<{
            id: string;
            workspace_id: string;
            activity_type: string;
            source_id: string | null;
            target_id: string | null;
            summary: string | null;
            status: string;
            duration_ms: number | null;
            created_at: string;
          }>>(`/workspaces/${node.id}/activity?limit=5`);

          for (const a of activities) {
            if (a.activity_type === "a2a_send" || a.activity_type === "a2a_receive") {
              const sourceNode = nodesRef.current.find((n) => n.id === (a.source_id || a.workspace_id));
              const targetNode = nodesRef.current.find((n) => n.id === (a.target_id || ""));
              allComms.push({
                id: a.id,
                sourceId: a.source_id || a.workspace_id,
                targetId: a.target_id || "",
                sourceName: sourceNode?.data.name || "Unknown",
                targetName: targetNode?.data.name || "Unknown",
                type: a.activity_type as Communication["type"],
                summary: a.summary || "",
                status: a.status,
                timestamp: a.created_at,
                durationMs: a.duration_ms,
              });
            }
          }
        } catch {
          // Per-workspace failures must not blank the panel — the same
          // robustness the polling version had.
        }
      }

      // Newest-first with id-dedup, capped at COMMS_RENDER_CAP.
      const seen = new Set<string>();
      const sorted = allComms
        .sort((a, b) => b.timestamp.localeCompare(a.timestamp))
        .filter((c) => {
          if (seen.has(c.id)) return false;
          seen.add(c.id);
          return true;
        })
        .slice(0, COMMS_RENDER_CAP);

      setComms(sorted);
    } catch {
      // Bootstrap failure is non-blocking — the WS subscription below
      // will populate the panel as live events arrive.
    }
  }, []);

  // Bootstrap once on mount + every time the user re-opens after a
  // collapse. Closed-panel state intentionally drops live updates so
  // the panel doesn't churn invisible state — the next open reloads.
  useEffect(() => {
    if (!visible) return;
    bootstrapComms();
  }, [bootstrapComms, visible]);

  // Live-update path. Filters server-side ACTIVITY_LOGGED events down
  // to the comm-overlay-relevant subset and prepends each into the
  // rendered list with the same dedup the bootstrap path uses.
  //
  // Scope guard: ignore events for workspaces not in the visible online
  // set, so a user collapsing one workspace doesn't see its comms
  // continue to scroll in. Same shape the bootstrap path applies.
  useSocketEvent((msg) => {
    if (!visible) return;
    if (msg.event !== "ACTIVITY_LOGGED") return;

    const p = (msg.payload || {}) as ActivityLoggedPayload;
    const type = p.activity_type;
    if (type !== "a2a_send" && type !== "a2a_receive" && type !== "task_update") return;

    const wsId = msg.workspace_id;
    const onlineSet = new Set(
      nodesRef.current.filter((n) => n.data.status === "online").map((n) => n.id),
    );
    if (!onlineSet.has(wsId)) return;

    const sourceId = p.source_id || wsId;
    const targetId = p.target_id || "";
    const sourceNode = nodesRef.current.find((n) => n.id === sourceId);
    const targetNode = nodesRef.current.find((n) => n.id === targetId);

    const incoming: Communication = {
      id: p.id || `${msg.timestamp || Date.now()}:${sourceId}:${targetId}`,
      sourceId,
      targetId,
      sourceName: sourceNode?.data.name || "Unknown",
      targetName: targetNode?.data.name || "Unknown",
      type: type as Communication["type"],
      summary: p.summary || "",
      status: p.status || "ok",
      timestamp: p.created_at || msg.timestamp || new Date().toISOString(),
      durationMs: p.duration_ms ?? null,
    };

    setComms((prev) => {
      // Prepend, dedup by id, re-cap. Functional setState is necessary
      // because two ACTIVITY_LOGGED events arriving in the same React
      // batch would otherwise read a stale `comms` from the closure.
      const seen = new Set<string>();
      const merged = [incoming, ...prev]
        .sort((a, b) => b.timestamp.localeCompare(a.timestamp))
        .filter((c) => {
          if (seen.has(c.id)) return false;
          seen.add(c.id);
          return true;
        })
        .slice(0, COMMS_RENDER_CAP);
      return merged;
    });
  });

  if (!visible || comms.length === 0) {
    return (
      <button
        type="button"
        onClick={() => setVisible(true)}
        aria-label="Show communications panel"
        className="fixed top-16 right-4 z-30 px-3 py-1.5 bg-surface-sunken/90 border border-line/50 rounded-lg text-[10px] text-ink-mid hover:text-ink transition-colors"
      >
        <span aria-hidden="true">↗↙ </span>{comms.length > 0 ? `${comms.length} comms` : "Communications"}
      </button>
    );
  }

  return (
    <div className="fixed top-16 right-4 z-30 w-[320px] max-h-[400px] bg-surface-sunken/95 border border-line/50 rounded-xl shadow-xl shadow-black/30 backdrop-blur-sm overflow-hidden">
      <div className="flex items-center justify-between px-3 py-2 border-b border-line/60">
        <div className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">
          <span aria-hidden="true">↗↙ </span>Communications ({comms.length})
        </div>
        <button
          type="button"
          onClick={() => setVisible(false)}
          aria-label="Close communications panel"
          className="text-ink-soft hover:text-ink-mid text-xs"
        >
          <span aria-hidden="true">✕</span>
        </button>
      </div>

      <div className="overflow-y-auto max-h-[350px] p-2 space-y-1">
        {comms.map((c) => {
          const isSelected = selectedNodeId === c.sourceId || selectedNodeId === c.targetId;
          const typeColor = c.type === "a2a_send" ? "text-cyan-400" : c.type === "a2a_receive" ? "text-accent" : "text-warm";
          const typeIcon = c.type === "a2a_send" ? "↗" : c.type === "a2a_receive" ? "↙" : "◆";
          const statusIcon = c.status === "ok" ? "✓" : c.status === "error" ? "✕" : "⏱";
          const statusColor = c.status === "ok" ? "text-good" : c.status === "error" ? "text-bad" : "text-warm";
          const age = formatAge(c.timestamp);

          return (
            <div
              key={c.id}
              className={`rounded-lg px-2.5 py-1.5 text-[9px] border transition-all ${
                isSelected
                  ? "bg-blue-950/30 border-blue-800/40"
                  : "bg-surface-card/30 border-line/20 hover:bg-surface-card/50"
              }`}
            >
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-1.5 min-w-0">
                  <span className={typeColor} aria-hidden="true">{typeIcon}</span>
                  <span className="sr-only">{COMM_TYPE_LABELS[c.type] ?? c.type}</span>
                  <span className="text-ink-mid font-medium truncate">
                    {c.sourceName}
                  </span>
                  <span className="text-ink-mid" aria-hidden="true">→</span>
                  <span className="sr-only">to</span>
                  <span className="text-ink-mid truncate">{c.targetName}</span>
                </div>
                <div className="flex items-center gap-1 shrink-0">
                  <span className={statusColor} aria-hidden="true">{statusIcon}</span>
                  <span className="sr-only">{c.status}</span>
                  <span className="text-ink-mid">{age}</span>
                </div>
              </div>
              {c.summary && (
                <div className="text-ink-soft truncate mt-0.5 pl-4">{c.summary}</div>
              )}
              {c.durationMs && (
                <div className="text-ink-mid pl-4">{c.durationMs}ms</div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function formatAge(timestamp: string): string {
  const diff = Date.now() - new Date(timestamp).getTime();
  if (diff < 60000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3600000) return `${Math.floor(diff / 60000)}m`;
  if (diff < 86400000) return `${Math.floor(diff / 3600000)}h`;
  return `${Math.floor(diff / 86400000)}d`;
}
