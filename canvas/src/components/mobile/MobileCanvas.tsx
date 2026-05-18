"use client";

// 02 · Canvas graph — pan-friendly mini-graph with status-coloured nodes.
// Node positions come from the live store (the same x/y the desktop canvas
// uses). The screen normalizes them to a 0..1 viewport so the graph fits
// the phone frame regardless of where the user has the desktop pan/zoom.

import { useMemo, useRef, useState, type TouchEvent as ReactTouchEvent } from "react";

import { useCanvasStore } from "@/store/canvas";

import { type MobileAgent, WorkspacePill, toMobileAgent } from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, usePalette } from "./palette";
import { Icons, StatusDot, TierChip } from "./primitives";

const SCALE_MIN = 0.5;
const SCALE_MAX = 3;

interface Gesture {
  kind: "none" | "pinch" | "pan";
  startDist?: number;
  startScale?: number;
  startTouch?: { x: number; y: number };
  startPan?: { x: number; y: number };
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v));

export function MobileCanvas({
  dark,
  onOpen,
  onSpawn,
}: {
  dark: boolean;
  onOpen: (agentId: string) => void;
  onSpawn: () => void;
}) {
  const p = usePalette(dark);
  const nodes = useCanvasStore((s) => s.nodes);

  // Project store nodes into 0..100 (%) space, leaving 8% padding on each
  // edge so cards don't clip. Falls back to a uniform circular layout
  // when every node sits at (0,0) — common right after first hydrate.
  const layout = useMemo(() => {
    const items = nodes.map((n) => ({
      id: n.id,
      agent: toMobileAgent(n),
      x: n.position?.x ?? 0,
      y: n.position?.y ?? 0,
      parentId: n.data.parentId ?? null,
    }));
    if (items.length === 0) return [] as Array<{ agent: MobileAgent; x: number; y: number; parentId: string | null }>;

    const xs = items.map((i) => i.x);
    const ys = items.map((i) => i.y);
    const xMin = Math.min(...xs);
    const xMax = Math.max(...xs);
    const yMin = Math.min(...ys);
    const yMax = Math.max(...ys);
    const spread = (xMax - xMin) + (yMax - yMin);
    if (spread < 1) {
      // Degenerate (everything stacked) — fall back to a ring.
      const n = items.length;
      return items.map((it, idx) => {
        const angle = (idx / n) * Math.PI * 2;
        return {
          agent: it.agent,
          parentId: it.parentId,
          x: 50 + Math.cos(angle) * 32,
          y: 50 + Math.sin(angle) * 26,
        };
      });
    }

    const scaleX = (v: number) =>
      xMax === xMin ? 50 : 8 + ((v - xMin) / (xMax - xMin)) * 84;
    const scaleY = (v: number) =>
      yMax === yMin ? 50 : 14 + ((v - yMin) / (yMax - yMin)) * 70;
    return items.map((it) => ({
      agent: it.agent,
      parentId: it.parentId,
      x: scaleX(it.x),
      y: scaleY(it.y),
    }));
  }, [nodes]);

  // Edges = parent→child relations from the store.
  const edges = useMemo(() => {
    const byId = new Map(layout.map((l) => [l.agent.id, l]));
    return layout
      .filter((l) => l.parentId && byId.has(l.parentId))
      .map((l) => ({ from: byId.get(l.parentId!)!, to: l }));
  }, [layout]);

  // Pinch-to-zoom + single-finger pan over the graph layer. Header pill,
  // legend, and FAB stay anchored to the viewport (outside the transform
  // layer). Tap-to-open still works because a stationary touchend
  // dispatches a click on the underlying button.
  const [scale, setScale] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const gestureRef = useRef<Gesture>({ kind: "none" });

  const onTouchStart = (e: ReactTouchEvent<HTMLDivElement>) => {
    if (e.touches.length === 2) {
      const a = e.touches[0];
      const b = e.touches[1];
      gestureRef.current = {
        kind: "pinch",
        startDist: Math.hypot(b.clientX - a.clientX, b.clientY - a.clientY),
        startScale: scale,
      };
    } else if (e.touches.length === 1) {
      const t = e.touches[0];
      gestureRef.current = {
        kind: "pan",
        startTouch: { x: t.clientX, y: t.clientY },
        startPan: { ...pan },
      };
    }
  };

  const onTouchMove = (e: ReactTouchEvent<HTMLDivElement>) => {
    const g = gestureRef.current;
    if (g.kind === "pinch" && e.touches.length === 2 && g.startDist && g.startScale) {
      const a = e.touches[0];
      const b = e.touches[1];
      const dist = Math.hypot(b.clientX - a.clientX, b.clientY - a.clientY);
      setScale(clamp(g.startScale * (dist / g.startDist), SCALE_MIN, SCALE_MAX));
    } else if (g.kind === "pan" && e.touches.length === 1 && g.startTouch && g.startPan) {
      const t = e.touches[0];
      setPan({
        x: g.startPan.x + (t.clientX - g.startTouch.x),
        y: g.startPan.y + (t.clientY - g.startTouch.y),
      });
    }
  };

  const onTouchEnd = (e: ReactTouchEvent<HTMLDivElement>) => {
    if (e.touches.length === 0) gestureRef.current = { kind: "none" };
  };

  const resetView = () => {
    setScale(1);
    setPan({ x: 0, y: 0 });
  };

  const transformStyle = {
    transform: `translate(${pan.x}px, ${pan.y}px) scale(${scale})`,
    transformOrigin: "50% 50%",
    // Smooth out the pinch math without lagging the gesture; tighter
    // than a CSS animation so it doesn't feel rubber-bandy.
    willChange: "transform",
  };

  const zoomed = Math.abs(scale - 1) > 0.01 || pan.x !== 0 || pan.y !== 0;

  return (
    <div
      style={{
        position: "absolute",
        inset: 0,
        background: p.bg,
        overflow: "hidden",
        fontFamily: MOBILE_FONT_SANS,
        // Tell the browser we own touch gestures here — without this, the
        // browser performs default pinch-to-zoom on the page itself,
        // which would zoom the entire phone shell, not just our graph.
        touchAction: "none",
      }}
      onTouchStart={onTouchStart}
      onTouchMove={onTouchMove}
      onTouchEnd={onTouchEnd}
    >
      {/* Dotted grid background — fills the viewport, doesn't transform */}
      <div
        style={{
          position: "absolute",
          inset: 0,
          backgroundImage: `radial-gradient(${dark ? "rgba(255,255,255,0.05)" : "rgba(40,30,20,0.07)"} 1px, transparent 1px)`,
          backgroundSize: "18px 18px",
        }}
      />

      {/* Header pill */}
      <div
        style={{
          position: "absolute",
          top: "max(env(safe-area-inset-top), 44px)",
          left: 0,
          right: 0,
          zIndex: 20,
          display: "flex",
          justifyContent: "center",
          padding: "0 12px",
        }}
      >
        <WorkspacePill dark={dark} count={nodes.length} />
      </div>

      {/* Reset-view button — only shown after the user has zoomed or
          panned, so the corner stays clean by default. Sits next to the
          legend so it doesn't fight the spawn FAB. */}
      {zoomed && (
        <button
          type="button"
          onClick={resetView}
          aria-label="Reset zoom"
          style={{
            position: "absolute",
            right: 14,
            top: "calc(max(env(safe-area-inset-top), 44px) + 56px)",
            zIndex: 25,
            padding: "6px 12px",
            borderRadius: 999,
            cursor: "pointer",
            background: dark ? "rgba(34,33,28,0.78)" : "rgba(255,253,247,0.88)",
            backdropFilter: "blur(20px)",
            border: `0.5px solid ${p.border}`,
            color: p.text2,
            fontSize: 11,
            fontFamily: MOBILE_FONT_MONO,
            letterSpacing: "0.04em",
            textTransform: "uppercase",
            fontWeight: 600,
          }}
          className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
        >
          Reset
        </button>
      )}

      {/* Transform layer — pinch-zoom + pan apply here. Edges and nodes
          live inside so they scale together; everything outside this
          layer (header, legend, FAB) is anchored to the viewport. */}
      <div
        style={{
          position: "absolute",
          inset: 0,
          ...transformStyle,
        }}
      >
        {/* SVG edges */}
        <svg
          style={{
            position: "absolute",
            inset: 0,
            width: "100%",
            height: "100%",
            zIndex: 1,
            pointerEvents: "none",
          }}
          aria-hidden="true"
        >
          {edges.map((e, i) => (
            <line
              key={i}
              x1={`${e.from.x}%`}
              y1={`${e.from.y}%`}
              x2={`${e.to.x}%`}
              y2={`${e.to.y}%`}
              stroke={dark ? "rgba(255,255,255,0.12)" : "rgba(40,30,20,0.12)"}
              strokeWidth={1 / scale}
              strokeDasharray="2 4"
            />
          ))}
        </svg>

      {/* Nodes */}
      {layout.map((l) => {
        const isOnline = l.agent.status === "online";
        return (
          <button
            key={l.agent.id}
            type="button"
            onClick={() => onOpen(l.agent.id)}
            style={{
              position: "absolute",
              left: `${l.x}%`,
              top: `${l.y}%`,
              transform: "translate(-50%, -50%)",
              width: 130,
              maxWidth: "42%",
              background:
                l.agent.tier === "T4" && isOnline
                  ? p.t4SoftCard
                  : isOnline
                    ? p.greenSoft
                    : p.surface,
              border: `0.5px solid ${p.border}`,
              borderRadius: 12,
              padding: "8px 10px",
              display: "flex",
              flexDirection: "column",
              gap: 4,
              cursor: "pointer",
              textAlign: "left",
              boxShadow: dark
                ? "0 4px 14px rgba(0,0,0,0.3)"
                : "0 2px 8px rgba(40,30,20,0.06)",
              zIndex: 5,
            }}
          >
            <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
              <StatusDot status={l.agent.status} size={7} dark={dark} halo={false} />
              <span
                style={{
                  flex: 1,
                  fontSize: 12,
                  fontWeight: 600,
                  color: p.text,
                  whiteSpace: "nowrap",
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                }}
              >
                {l.agent.name}
              </span>
              <TierChip tier={l.agent.tier} dark={dark} />
            </div>
            <div
              style={{
                fontSize: 9,
                color: p.text3,
                letterSpacing: "0.04em",
                fontFamily: MOBILE_FONT_MONO,
              }}
            >
              {l.agent.tag}
            </div>
          </button>
        );
      })}
      </div>
      {/* End transform layer */}

      {/* Bottom legend */}
      <div
        style={{
          position: "absolute",
          left: 14,
          bottom: 96,
          zIndex: 25,
          background: dark ? "rgba(34,33,28,0.78)" : "rgba(255,253,247,0.88)",
          backdropFilter: "blur(20px)",
          border: `0.5px solid ${p.border}`,
          borderRadius: 14,
          padding: "10px 12px",
          boxShadow: "0 4px 14px rgba(40,30,20,0.08)",
          fontFamily: MOBILE_FONT_MONO,
          fontSize: 9.5,
          color: p.text2,
          letterSpacing: "0.04em",
        }}
      >
        <div
          style={{
            fontWeight: 600,
            color: p.text3,
            marginBottom: 6,
            textTransform: "uppercase",
          }}
        >
          Legend
        </div>
        <div style={{ display: "flex", gap: 10, flexWrap: "wrap", maxWidth: 180 }}>
          {(["online", "starting", "degraded", "failed", "paused"] as const).map((s) => (
            <span key={s} style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
              <StatusDot status={s} size={6} dark={dark} halo={false} />
              {s}
            </span>
          ))}
        </div>
      </div>

      {/* Spawn FAB */}
      <button
        type="button"
        onClick={onSpawn}
        aria-label="Spawn new agent"
        style={{
          position: "absolute",
          right: 24,
          bottom: 100,
          zIndex: 25,
          width: 54,
          height: 54,
          borderRadius: 999,
          border: "none",
          cursor: "pointer",
          background: p.text,
          color: dark ? p.bg : "#fff",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          boxShadow: "0 8px 24px rgba(40,30,20,0.25)",
        }}
      >
        {Icons.plus({ size: 22 })}
      </button>
    </div>
  );
}
