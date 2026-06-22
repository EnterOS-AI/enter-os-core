'use client';

import { useEffect, useMemo, useCallback, useRef } from "react";
import { type Edge, MarkerType } from "@xyflow/react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import type { ActivityEntry } from "@/types/activity";

// ── Constants ─────────────────────────────────────────────────────────────────

/** 60-minute look-back window for delegation activity */
export const A2A_WINDOW_MS = 60 * 60 * 1000;

/** Threshold for "hot" edges: < 5 minutes → animated + violet stroke */
export const A2A_HOT_MS = 5 * 60 * 1_000;

// ── Helpers ───────────────────────────────────────────────────────────────────

/** Format millisecond timestamp as human-readable relative time ("2m ago"). */
export function formatA2ARelativeTime(ts: number, now = Date.now()): string {
  const diff = now - ts;
  if (diff < 60_000) return "just now";
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  return `${Math.floor(diff / 3_600_000)}h ago`;
}

// ── Pure aggregation function (exported for unit tests) ───────────────────────

/**
 * Converts raw delegation activity rows into React Flow overlay edges.
 *
 * Rules applied:
 * - Only `method === "delegate"` rows (initiation, not result) to avoid double-counting.
 * - Rows older than A2A_WINDOW_MS are discarded.
 * - Rows with null source_id or target_id are skipped.
 * - Multiple rows on the same source→target pair are aggregated (count + latest timestamp).
 * - Edge is animated + violet-500 when lastAt < A2A_HOT_MS ago; otherwise blue-500.
 * - All styles have `pointerEvents: "none"` so canvas nodes remain draggable.
 */
export function buildA2AEdges(
  rows: ActivityEntry[],
  now = Date.now()
): Edge[] {
  const cutoff = now - A2A_WINDOW_MS;

  // 1. Filter: only delegate initiations within the window with valid endpoints
  const initiations = rows.filter(
    (r) =>
      r.method === "delegate" &&
      r.source_id != null &&
      r.target_id != null &&
      new Date(r.created_at).getTime() > cutoff
  );

  if (initiations.length === 0) return [];

  // 2. Aggregate by "source→target" pair
  type Agg = { source: string; target: string; count: number; lastAt: number };
  const map = new Map<string, Agg>();

  for (const row of initiations) {
    const source = row.source_id as string;
    const target = row.target_id as string;
    const key = `${source}→${target}`;
    const ts = new Date(row.created_at).getTime();
    const prev = map.get(key) ?? { source, target, count: 0, lastAt: 0 };
    map.set(key, {
      ...prev,
      count: prev.count + 1,
      lastAt: Math.max(prev.lastAt, ts),
    });
  }

  // 3. Build React Flow Edge objects. We tag every overlay edge with
  //    type: "a2a" so React Flow renders it via our custom A2AEdge
  //    component (canvas/A2AEdge.tsx). The custom component portals
  //    its label out of the SVG layer so it (a) doesn't get hidden
  //    behind workspace cards and (b) is clickable.
  return Array.from(map.values()).map(({ source, target, count, lastAt }) => {
    const isHot = now - lastAt < A2A_HOT_MS;
    const stroke = isHot ? "#8b5cf6" : "#3b82f6"; // violet-500 : blue-500

    const callWord = count === 1 ? "call" : "calls";
    const label = `${count} ${callWord} · ${formatA2ARelativeTime(lastAt, now)}`;

    return {
      id: `a2a-${source}-${target}`,
      type: "a2a",
      source,
      target,
      animated: isHot,
      markerEnd: {
        type: MarkerType.ArrowClosed,
        color: stroke,
        width: 12,
        height: 12,
      },
      style: {
        stroke,
        strokeWidth: 2,
        // Path itself stays non-interactive so node drags through
        // the line still work. The clickable target is the label
        // pill, which sets pointerEvents: all on its own div.
        pointerEvents: "none" as React.CSSProperties["pointerEvents"],
      },
      // `label` keeps the same string for back-compat with any test
      // that asserts on it (e.g. buildA2AEdges output shape). Custom
      // edge reads the rich data from `data` so the label visual is
      // not constrained to a string anymore.
      label,
      data: {
        count,
        lastAt,
        isHot,
        label,
      },
    };
  });
}

// ── Component ─────────────────────────────────────────────────────────────────

/**
 * A2ATopologyOverlay — null-rendering side-effect component.
 *
 * Fetches delegation activity from all visible workspace nodes (fan-out),
 * aggregates into directed edges, and writes them to the canvas store as
 * `a2aEdges`. Canvas.tsx merges these with topology edges and passes the
 * combined list to ReactFlow.
 *
 * Update shape (issue #61 Stage 2, replaces the 60s polling loop):
 *  - On mount (when showA2AEdges): one HTTP fan-out per visible workspace
 *    (delegation rows, 60-min window). Bootstraps the local row buffer.
 *  - Steady state: subscribes to ACTIVITY_LOGGED via useSocketEvent.
 *    Each delegation event from a visible workspace is appended to the
 *    buffer; edges are re-derived via the existing buildA2AEdges helper.
 *  - showA2AEdges toggle off: clears edges + buffer.
 *  - Visible-ID-set change: re-bootstraps so a freshly-shown workspace
 *    backfills its 60-min history (existing visibleIdsKey selector
 *    behaviour preserved — that's the 2026-05-04 render-loop fix).
 *
 * No interval poll. The singleton ReconnectingSocket already owns
 * reconnect / backoff / health-check; useSocketEvent inherits those.
 *
 * Mount this inside CanvasInner (no ReactFlow hook dependency).
 */
export function A2ATopologyOverlay() {
  const showA2AEdges = useCanvasStore((s) => s.showA2AEdges);
  // Stable Zustand action reference — safe to call inside effects
  const setA2AEdges = useCanvasStore((s) => s.setA2AEdges);

  // Subscribe to a STABLE STRING KEY of visible workspace IDs, not the
  // nodes array itself. Zustand returns a new array reference on every
  // store update (status flips, position drags, peer-discovery writes,
  // workspace-tab opens, etc.) — even when the set of visible IDs is
  // unchanged. Selecting a sorted-CSV string makes Zustand's default
  // shallow-equal short-circuit the re-render unless the actual ID set
  // changes.
  //
  // Why this matters: previously visibleIds was useMemo'd on `nodes`, so
  // the array reference recreated on every store mutation. fetchAndUpdate
  // (useCallback'd on visibleIds) then recreated, the useEffect re-fired,
  // it tore down the 60s setInterval and immediately re-ran the fan-out.
  // With ~5 store updates/second from heartbeats + polling, the canvas
  // hammered /workspaces/<id>/activity?type=delegation 5×N requests/sec
  // until edge rate-limit kicked in with HTTP 429. The recursive React
  // render trace in the original bug report (uE → ux → uE → ux ...) is
  // the symptom of this re-render storm.
  //
  // The fix is purely the dependency-stability change here; the fetch
  // logic is unchanged. Post-#61 the polling-driven fetch is gone, but
  // the visibleIdsKey gate is still required so a peer-discovery write
  // doesn't trigger a wasteful re-bootstrap.
  const visibleIdsKey = useCanvasStore((s) =>
    s.nodes
      .filter((n) => !n.hidden)
      .map((n) => n.id)
      .sort()
      .join(",")
  );

  const visibleIds = useMemo(
    () => (visibleIdsKey ? visibleIdsKey.split(",") : []),
    [visibleIdsKey]
  );

  // Local rolling buffer of delegation rows. Pruned by A2A_WINDOW_MS on
  // each rebuild so a long-lived session doesn't accumulate unbounded
  // history. The buffer's high-water mark is approximately:
  //    visibleIds.length × bootstrap-fetch-limit (500) + WS arrivals
  // Real-world ceiling: ~3000 entries at the 60-min boundary, all of
  // which buildA2AEdges aggregates into at most N² edges.
  const bufferRef = useRef<ActivityEntry[]>([]);
  // visibleIdsRef gives the WS handler the latest visible-ID set without
  // re-subscribing on every render. The bus listener is registered
  // exactly once per mount; subscriber-side filtering reads from this ref.
  const visibleIdsRef = useRef(visibleIds);
  visibleIdsRef.current = visibleIds;

  // Re-derive overlay edges from the current buffer + push to store.
  // Prunes by A2A_WINDOW_MS first so memory stays bounded across long
  // sessions and the aggregation cost stays O(window-size).
  const recomputeAndPush = useCallback(() => {
    const cutoff = Date.now() - A2A_WINDOW_MS;
    bufferRef.current = bufferRef.current.filter(
      (r) => new Date(r.created_at).getTime() > cutoff
    );
    setA2AEdges(buildA2AEdges(bufferRef.current));
  }, [setA2AEdges]);

  // Bootstrap fan-out — one HTTP per visible workspace. Replaces the
  // 60s polling loop entirely. Race-aware: any WS arrivals that landed
  // in the buffer DURING the fetch (between the await and resume) are
  // preserved by id-dedup-with-fetched-first ordering.
  const bootstrap = useCallback(async () => {
    if (visibleIds.length === 0) {
      bufferRef.current = [];
      setA2AEdges([]);
      return;
    }
    try {
      const fetchedRows = (
        await Promise.all(
          visibleIds.map((id) =>
            api
              .get<ActivityEntry[]>(
                `/workspaces/${id}/activity?type=delegation&limit=500&source=agent`
              )
              .catch(() => [] as ActivityEntry[])
          )
        )
      ).flat();

      // Merge: fetched rows first, then any in-flight WS arrivals that
      // accumulated during the await. Dedup by id so rows that appear
      // in both paths are not double-counted in the aggregation.
      const merged = [...fetchedRows, ...bufferRef.current];
      const seen = new Set<string>();
      bufferRef.current = merged.filter((r) => {
        if (seen.has(r.id)) return false;
        seen.add(r.id);
        return true;
      });
      recomputeAndPush();
    } catch {
      // Overlay failure is non-critical — canvas remains functional
    }
  }, [visibleIds, setA2AEdges, recomputeAndPush]);

  useEffect(() => {
    if (!showA2AEdges) {
      // Clear edges + buffer immediately when toggled off
      bufferRef.current = [];
      setA2AEdges([]);
      return;
    }
    void bootstrap();
  }, [showA2AEdges, bootstrap, setA2AEdges]);

  // Live-update path. Filters server-side ACTIVITY_LOGGED events down
  // to delegation initiations from visible workspaces and appends each
  // into the rolling buffer, re-deriving edges via buildA2AEdges.
  //
  // Only `method === "delegate"` rows count — the same filter
  // buildA2AEdges applies — so delegate_result rows arriving over the
  // wire don't double-count.
  useSocketEvent((msg) => {
    if (!showA2AEdges) return;
    if (msg.event !== "ACTIVITY_LOGGED") return;

    const p = (msg.payload || {}) as Record<string, unknown>;
    if (p.activity_type !== "delegation") return;
    if (p.method !== "delegate") return;

    const wsId = msg.workspace_id;
    if (!visibleIdsRef.current.includes(wsId)) return;

    // Synthesise an ActivityEntry from the WS payload so buildA2AEdges
    // (which the bootstrap path also feeds) handles it identically.
    const entry: ActivityEntry = {
      id:
        (p.id as string) ||
        `ws-push-${msg.timestamp || Date.now()}-${wsId}`,
      workspace_id: wsId,
      activity_type: "delegation",
      source_id: (p.source_id as string | null) ?? null,
      target_id: (p.target_id as string | null) ?? null,
      method: "delegate",
      summary: (p.summary as string | null) ?? null,
      request_body: null,
      response_body: null,
      duration_ms: (p.duration_ms as number | null) ?? null,
      status: (p.status as string) || "ok",
      error_detail: null,
      created_at:
        (p.created_at as string) ||
        msg.timestamp ||
        new Date().toISOString(),
    };

    bufferRef.current = [...bufferRef.current, entry];
    recomputeAndPush();
  });

  // Pure side-effect — renders nothing
  return null;
}
