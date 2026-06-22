"use client";

// 05 · Comms feed — workspace-wide A2A traffic.
// Bootstraps from /workspaces/:id/activity for the first few online
// workspaces, then prepends ACTIVITY_LOGGED events from the live socket.

import { useCallback, useEffect, useMemo, useState } from "react";

import { api } from "@/lib/api";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { useCanvasStore } from "@/store/canvas";

import { WorkspacePill } from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, usePalette } from "./palette";
import { SectionLabel } from "./primitives";

interface CommItem {
  id: string;
  from: string;
  to: string;
  kind: string;
  status: "ok" | "err";
  summary: string;
  durationMs: number | null;
  ago: string;
  ts: number;
}

interface ActivityRecord {
  id: string;
  workspace_id: string;
  activity_type: string;
  source_id: string | null;
  target_id: string | null;
  summary: string | null;
  status: string;
  duration_ms: number | null;
  created_at: string;
}

const FAN_OUT_CAP = 4;
const RENDER_CAP = 30;

type FilterId = "all" | "errors";

function relativeAgo(iso: string): string {
  const t = Date.parse(iso);
  if (isNaN(t)) return "";
  const seconds = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.round(hours / 24);
  return `${days}d`;
}

export function MobileComms({ dark }: { dark: boolean }) {
  const p = usePalette(dark);
  const nodes = useCanvasStore((s) => s.nodes);
  const [items, setItems] = useState<CommItem[]>([]);
  const [filter, setFilter] = useState<FilterId>("all");
  const [loading, setLoading] = useState(true);

  const nameOf = useCallback(
    (id: string | null | undefined): string => {
      if (!id) return "Unknown";
      const n = nodes.find((x) => x.id === id);
      return n?.data.name ?? id.slice(0, 8);
    },
    [nodes],
  );

  const toItem = useCallback(
    (a: ActivityRecord): CommItem => ({
      id: a.id,
      from: nameOf(a.source_id ?? a.workspace_id),
      to: nameOf(a.target_id),
      kind: a.activity_type,
      status: a.status === "error" || a.status === "err" ? "err" : "ok",
      summary: a.summary ?? "",
      durationMs: a.duration_ms,
      ago: relativeAgo(a.created_at),
      ts: Date.parse(a.created_at) || Date.now(),
    }),
    [nameOf],
  );

  // Stable signature of the online-workspace set. Re-runs the bootstrap
  // only when which workspaces are online changes — not on every node
  // position update or unrelated data churn.
  const onlineWorkspaceIds = useMemo(
    () =>
      nodes
        .filter((n) => n.data.status === "online")
        .slice(0, FAN_OUT_CAP)
        .map((n) => n.id),
    [nodes],
  );
  const onlineSignature = onlineWorkspaceIds.join("|");

  // Bootstrap: pull the most recent activity from the first few online
  // workspaces. Identical fan-out cap to CommunicationOverlay to keep
  // the load profile predictable on big tenants.
  useEffect(() => {
    let cancelled = false;
    if (onlineWorkspaceIds.length === 0) {
      setLoading(false);
      return;
    }
    Promise.all(
      onlineWorkspaceIds.map((id) =>
        api.get<ActivityRecord[]>(`/workspaces/${id}/activity?limit=8`).catch(() => []),
      ),
    ).then((batches) => {
      if (cancelled) return;
      const flat = batches.flat().map(toItem);
      flat.sort((a, b) => b.ts - a.ts);
      setItems(flat.slice(0, RENDER_CAP));
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
    // Effect depends on the signature string (stable when the id set
    // doesn't change) + toItem (memoized via useCallback). Listing the
    // id-array directly would re-run on every render because the array
    // identity changes even when the contents don't.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [onlineSignature, toItem]);

  // Live: prepend ACTIVITY_LOGGED events as they arrive.
  useSocketEvent((msg) => {
    if (msg.event !== "ACTIVITY_LOGGED") return;
    const payload = msg.payload as Partial<ActivityRecord> | undefined;
    if (!payload || !payload.id) return;
    const rec: ActivityRecord = {
      id: payload.id,
      workspace_id: payload.workspace_id ?? msg.workspace_id ?? "",
      activity_type: payload.activity_type ?? "a2a",
      source_id: payload.source_id ?? null,
      target_id: payload.target_id ?? null,
      summary: payload.summary ?? null,
      status: payload.status ?? "ok",
      duration_ms: payload.duration_ms ?? null,
      created_at: payload.created_at ?? new Date().toISOString(),
    };
    setItems((prev) => [toItem(rec), ...prev.filter((x) => x.id !== rec.id)].slice(0, RENDER_CAP));
  });

  const filtered = useMemo(
    () => items.filter((c) => filter === "all" || c.status === "err"),
    [items, filter],
  );
  const errCount = useMemo(() => items.filter((c) => c.status === "err").length, [items]);

  return (
    <div
      style={{
        height: "100%",
        overflow: "auto",
        background: p.bg,
        paddingBottom: 96,
        fontFamily: MOBILE_FONT_SANS,
      }}
    >
      <div style={{ padding: "max(env(safe-area-inset-top), 44px) 16px 8px" }}>
        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            marginBottom: 14,
          }}
        >
          <WorkspacePill dark={dark} count={nodes.length} />
          {/* Header filter button reserved — the All/Errors chips below
              already cover the v1 filter axis. */}
        </div>
        <div style={{ display: "flex", alignItems: "baseline", justifyContent: "space-between" }}>
          <h1
            style={{
              margin: 0,
              fontSize: 32,
              fontWeight: 700,
              color: p.text,
              letterSpacing: "-0.025em",
            }}
          >
            Comms
          </h1>
          <span
            style={{
              fontFamily: MOBILE_FONT_MONO,
              fontSize: 11,
              color: p.text3,
            }}
          >
            {items.length} events
          </span>
        </div>
        <p style={{ margin: "4px 0 0", fontSize: 13.5, color: p.text2 }}>
          Live A2A traffic across the workspace.
        </p>
      </div>

      <div style={{ display: "flex", gap: 6, padding: "12px 16px 8px" }}>
        {(
          [
            { id: "all", label: "All", n: items.length },
            { id: "errors", label: "Errors", n: errCount },
          ] as const
        ).map((o) => {
          const on = filter === o.id;
          return (
            <button
              key={o.id}
              type="button"
              onClick={() => setFilter(o.id)}
              style={{
                display: "inline-flex",
                alignItems: "center",
                gap: 6,
                padding: "7px 12px",
                borderRadius: 999,
                cursor: "pointer",
                background: on ? p.text : dark ? "#22211c" : "#fff",
                color: on ? (dark ? p.bg : "#fff") : p.text,
                border: `0.5px solid ${on ? "transparent" : p.border}`,
                fontSize: 13,
                fontWeight: 500,
              }}
              className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
            >
              {o.label}
              <span
                style={{
                  fontSize: 10.5,
                  opacity: 0.7,
                  fontFamily: MOBILE_FONT_MONO,
                }}
              >
                {o.n}
              </span>
            </button>
          );
        })}
      </div>

      <SectionLabel dark={dark}>Communications</SectionLabel>

      <div style={{ padding: "0 14px", display: "flex", flexDirection: "column", gap: 8 }}>
        {loading && items.length === 0 ? (
          <div role="status" aria-live="polite" style={{ padding: "30px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
            Loading recent comms…
          </div>
        ) : filtered.length === 0 ? (
          <div role="status" aria-live="polite" style={{ padding: "30px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
            No A2A traffic yet.
          </div>
        ) : (
          filtered.map((c) => <CommRow key={c.id} c={c} dark={dark} />)
        )}
      </div>
    </div>
  );
}

function CommRow({ c, dark }: { c: CommItem; dark: boolean }) {
  const p = usePalette(dark);
  const isErr = c.status === "err";
  return (
    <div
      style={{
        background: p.surface,
        borderRadius: 14,
        border: `0.5px solid ${p.border}`,
        padding: "12px 14px",
        display: "flex",
        flexDirection: "column",
        gap: 6,
      }}
    >
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          fontSize: 12,
          fontWeight: 600,
          color: p.text,
        }}
      >
        <span
          style={{
            padding: "1px 6px",
            borderRadius: 4,
            background: isErr ? "#f5dad2" : "#dde9e1",
            color: isErr ? "#a8341a" : p.greenInk,
            fontFamily: MOBILE_FONT_MONO,
            fontSize: 9,
            fontWeight: 700,
            letterSpacing: "0.06em",
          }}
        >
          {isErr ? "ERR" : "OK"}
        </span>
        <span
          style={{
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            maxWidth: 110,
          }}
        >
          {c.from}
        </span>
        <span style={{ color: p.text3, fontWeight: 500 }}>→</span>
        <span
          style={{
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            maxWidth: 110,
          }}
        >
          {c.to}
        </span>
        <span
          style={{
            marginLeft: "auto",
            fontSize: 10.5,
            color: p.text3,
            fontFamily: MOBILE_FONT_MONO,
          }}
        >
          {c.ago}
        </span>
      </div>
      <div
        style={{
          fontSize: 11,
          color: p.text3,
          fontWeight: 600,
          fontFamily: MOBILE_FONT_MONO,
          letterSpacing: "0.02em",
        }}
      >
        {c.kind}
        {c.durationMs != null && (
          <span style={{ marginLeft: 8, color: isErr ? "#a8341a" : p.text3 }}>{c.durationMs}ms</span>
        )}
      </div>
      {c.summary && (
        <div
          style={{
            fontSize: 12.5,
            color: p.text2,
            lineHeight: 1.4,
            overflowWrap: "anywhere",
          }}
        >
          {c.summary}
        </div>
      )}
    </div>
  );
}
