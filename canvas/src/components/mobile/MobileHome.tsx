"use client";

// 01 · Workspace home — agent list + filter chips + FAB.
// Mirrors design/screen-home.jsx, swapped to live store data.

import { useMemo, useState } from "react";

import { useCanvasStore } from "@/store/canvas";

import {
  type AgentFilter,
  AgentCard,
  FilterChips,
  WorkspacePill,
  classifyForFilter,
  toMobileAgent,
} from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, usePalette } from "./palette";
import { Icons, SectionLabel } from "./primitives";

export function MobileHome({
  dark,
  density,
  onOpen,
  onSpawn,
  workspaceLabel = "Default",
  username,
}: {
  dark: boolean;
  density: "compact" | "regular";
  onOpen: (agentId: string) => void;
  onSpawn: () => void;
  workspaceLabel?: string;
  username?: string;
}) {
  const p = usePalette(dark);
  const nodes = useCanvasStore((s) => s.nodes);
  const agents = useMemo(() => nodes.map(toMobileAgent), [nodes]);
  const [filter, setFilter] = useState<AgentFilter>("all");

  const counts = useMemo(() => {
    const c = { all: agents.length, online: 0, issue: 0, paused: 0 };
    for (const a of agents) {
      const bucket = classifyForFilter(a.status);
      if (bucket !== "all") c[bucket]++;
    }
    return c;
  }, [agents]);

  const filtered = useMemo(
    () => agents.filter((a) => filter === "all" || classifyForFilter(a.status) === filter),
    [agents, filter],
  );

  const compact = density === "compact";
  const rootCount = useMemo(
    () => agents.filter((a) => !a.parentId).length,
    [agents],
  );

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
      {/* Sticky header */}
      <div
        style={{
          position: "sticky",
          top: 0,
          zIndex: 10,
          background: `linear-gradient(${p.bg} 60%, ${p.bg}00)`,
          padding: "max(env(safe-area-inset-top), 44px) 16px 8px",
        }}
      >
        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            marginBottom: 14,
          }}
        >
          <WorkspacePill dark={dark} count={agents.length} />
          {/* Search button reserved — wire to a mobile SearchDialog in v1.1. */}
        </div>
        <div
          style={{
            display: "flex",
            alignItems: "baseline",
            justifyContent: "space-between",
            marginBottom: 4,
          }}
        >
          <h1
            style={{
              margin: 0,
              fontSize: 32,
              fontWeight: 700,
              color: p.text,
              letterSpacing: "-0.025em",
            }}
          >
            Agents
          </h1>
          {username && (
            <span
              style={{
                fontFamily: MOBILE_FONT_MONO,
                fontSize: 11,
                color: p.text3,
                letterSpacing: "0.04em",
              }}
            >
              {username}
            </span>
          )}
        </div>
        <p style={{ margin: "0 0 14px", fontSize: 13.5, color: p.text2 }}>
          {rootCount} workspace{rootCount === 1 ? "" : "s"} · live
        </p>
      </div>

      <FilterChips value={filter} onChange={setFilter} dark={dark} counts={counts} />

      <SectionLabel
        dark={dark}
        right={
          <span
            style={{
              color: p.text3,
              fontSize: 10.5,
              letterSpacing: "0.04em",
              textTransform: "none",
            }}
          >
            {filtered.length}/{agents.length}
          </span>
        }
      >
        Workspace · {workspaceLabel}
      </SectionLabel>

      <div
        style={{
          display: "flex",
          flexDirection: "column",
          gap: 8,
          padding: "0 14px",
        }}
      >
        {filtered.length === 0 ? (
          <div
            style={{
              padding: "40px 8px",
              textAlign: "center",
              color: p.text3,
              fontSize: 13,
            }}
          >
            No agents match this filter.
          </div>
        ) : (
          filtered.map((a) => (
            <AgentCard
              key={a.id}
              agent={a}
              dark={dark}
              compact={compact}
              onClick={() => onOpen(a.id)}
            />
          ))
        )}
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
          boxShadow: "0 8px 24px rgba(40,30,20,0.25), 0 2px 6px rgba(40,30,20,0.15)",
        }}
      >
        {Icons.plus({ size: 22 })}
      </button>
    </div>
  );
}
