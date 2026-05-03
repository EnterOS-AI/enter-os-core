"use client";

import { useState, useEffect, useCallback, useRef } from "react";
import { useCanvasStore } from "@/store/canvas";
import { api } from "@/lib/api";
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

/**
 * Overlay showing recent A2A communications between workspaces.
 * Renders as a floating log panel that auto-updates.
 */
export function CommunicationOverlay() {
  const [comms, setComms] = useState<Communication[]>([]);
  const [visible, setVisible] = useState(true);
  const selectedNodeId = useCanvasStore((s) => s.selectedNodeId);
  const nodes = useCanvasStore((s) => s.nodes);
  const nodesRef = useRef(nodes);
  nodesRef.current = nodes;

  const fetchComms = useCallback(async () => {
    try {
      // Fetch activity from all online workspaces
      const onlineNodes = nodesRef.current.filter((n) => n.data.status === "online");
      const allComms: Communication[] = [];

      for (const node of onlineNodes.slice(0, 6)) {
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
              const sourceNode = nodes.find((n) => n.id === (a.source_id || a.workspace_id));
              const targetNode = nodes.find((n) => n.id === (a.target_id || ""));
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
          // Skip workspaces that fail
        }
      }

      // Sort by timestamp, newest first, dedupe
      const seen = new Set<string>();
      const sorted = allComms
        .sort((a, b) => b.timestamp.localeCompare(a.timestamp))
        .filter((c) => {
          if (seen.has(c.id)) return false;
          seen.add(c.id);
          return true;
        })
        .slice(0, 20);

      setComms(sorted);
    } catch {
      // Silently handle API errors
    }
  }, []);

  useEffect(() => {
    fetchComms();
    const interval = setInterval(fetchComms, 10000);
    return () => clearInterval(interval);
  }, [fetchComms]);

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
