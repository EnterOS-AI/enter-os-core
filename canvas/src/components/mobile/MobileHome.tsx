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

  // Agent HIERARCHY (core#2697 Phase 2): the desktop home is a parent→child
  // tree (ConciergeShell); mobile was a flat list, hiding org structure +
  // queue depth. Build the tree from parentId; render it for the default
  // ("all") view with expand/collapse + a queue-count badge. Active filters
  // keep the flat list (filtering a tree drops context).
  const { childrenOf, roots } = useMemo(() => {
    const ids = new Set(agents.map((a) => a.id));
    const childrenOf = new Map<string, typeof agents>();
    const roots: typeof agents = [];
    for (const a of agents) {
      const pid = a.parentId && ids.has(a.parentId) ? a.parentId : null;
      if (pid) {
        const arr = childrenOf.get(pid) ?? [];
        arr.push(a);
        childrenOf.set(pid, arr);
      } else {
        roots.push(a);
      }
    }
    return { childrenOf, roots };
  }, [agents]);

  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const toggle = (id: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const treeRows = useMemo(() => {
    const out: { agent: (typeof agents)[number]; depth: number; hasChildren: boolean }[] = [];
    const walk = (a: (typeof agents)[number], depth: number) => {
      const kids = childrenOf.get(a.id) ?? [];
      out.push({ agent: a, depth, hasChildren: kids.length > 0 });
      if (kids.length && !collapsed.has(a.id)) for (const k of kids) walk(k, depth + 1);
    };
    for (const r of roots) walk(r, 0);
    return out;
  }, [roots, childrenOf, collapsed]);

  const rootCount = roots.length;

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
        {filter === "all" ? (
          treeRows.length === 0 ? (
            <div style={{ padding: "40px 8px", textAlign: "center", color: p.text3, fontSize: 13 }}>
              No agents yet.
            </div>
          ) : (
            treeRows.map(({ agent, depth, hasChildren }) => (
              <div
                key={agent.id}
                data-testid="agent-tree-row"
                style={{ display: "flex", alignItems: "center", gap: 4, paddingLeft: depth * 16 }}
              >
                {hasChildren ? (
                  <button
                    type="button"
                    onClick={() => toggle(agent.id)}
                    aria-label={collapsed.has(agent.id) ? "Expand" : "Collapse"}
                    style={{
                      width: 18,
                      height: 18,
                      flexShrink: 0,
                      border: "none",
                      background: "none",
                      color: p.text3,
                      cursor: "pointer",
                      fontSize: 11,
                      lineHeight: 1,
                    }}
                  >
                    {collapsed.has(agent.id) ? "▸" : "▾"}
                  </button>
                ) : (
                  <span style={{ width: 18, flexShrink: 0 }} aria-hidden="true" />
                )}
                <div style={{ flex: 1, minWidth: 0, position: "relative" }}>
                  <AgentCard agent={agent} dark={dark} compact={compact} onClick={() => onOpen(agent.id)} />
                  {agent.calls > 0 && (
                    <span
                      aria-label={`${agent.calls} queued`}
                      style={{
                        position: "absolute",
                        top: 8,
                        right: 10,
                        minWidth: 18,
                        height: 18,
                        padding: "0 5px",
                        borderRadius: 9,
                        background: p.accent,
                        color: "#fff",
                        fontSize: 10,
                        fontWeight: 700,
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "center",
                      }}
                    >
                      {agent.calls}
                    </span>
                  )}
                </div>
              </div>
            ))
          )
        ) : filtered.length === 0 ? (
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
        className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
      >
        {Icons.plus({ size: 22 })}
      </button>
    </div>
  );
}
