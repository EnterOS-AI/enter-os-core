"use client";

// Screen-shared composites: TabBar, WorkspacePill, AgentCard, FilterChips.
// Mirrors molecules-ai-mobile-app/project/screens-shared.jsx but reads
// from the live canvas store rather than the prototype's mock AGENTS.

import type { Node } from "@xyflow/react";

import { type WorkspaceNodeData, summarizeWorkspaceCapabilities } from "@/store/canvas";

import {
  MOBILE_FONT_MONO,
  type MobilePalette,
  type MobileStatus,
  normalizeStatus,
  tierCode,
  usePalette,
} from "./palette";
import { Icons, StatusDot, TierChip } from "./primitives";
import { isExternalLikeRuntime } from "@/lib/externalRuntimes";

// Derived view-model the mobile screens consume. Built once per render
// from the store's Node<WorkspaceNodeData>.
export interface MobileAgent {
  id: string;
  name: string;
  tag: string;
  tier: "T1" | "T2" | "T3" | "T4";
  status: MobileStatus;
  remote: boolean;
  runtime: string;
  skills: number;
  calls: number;
  desc: string;
  parentId: string | null;
}

export function toMobileAgent(node: Node<WorkspaceNodeData>): MobileAgent {
  const cap = summarizeWorkspaceCapabilities(node.data);
  const runtime = cap.runtime ?? "unknown";
  const remote = isExternalLikeRuntime(runtime);
  return {
    id: node.id,
    name: node.data.name || node.id,
    tag: runtime,
    tier: tierCode(node.data.tier),
    status: normalizeStatus(node.data.status),
    remote,
    runtime,
    skills: cap.skillCount,
    calls: typeof node.data.activeTasks === "number" ? node.data.activeTasks : 0,
    desc: node.data.role || cap.currentTask || "",
    parentId: node.data.parentId ?? null,
  };
}

// ── Tab bar ────────────────────────────────────────────────────
export type MobileTabId = "agents" | "canvas" | "comms" | "me";

export function TabBar({
  active,
  onChange,
  dark,
}: {
  active: MobileTabId;
  onChange: (id: MobileTabId) => void;
  dark: boolean;
}) {
  const p = usePalette(dark);
  const tabs: { id: MobileTabId; label: string; icon: keyof typeof Icons }[] = [
    { id: "agents", label: "Agents", icon: "list" },
    { id: "canvas", label: "Canvas", icon: "graph" },
    { id: "comms", label: "Comms", icon: "pulse" },
    { id: "me", label: "Me", icon: "user" },
  ];

  const handleKeyDown = (e: React.KeyboardEvent, idx: number) => {
    let nextIdx: number | null = null;
    if (e.key === "ArrowRight" || e.key === "ArrowDown") {
      nextIdx = (idx + 1) % tabs.length;
    } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      nextIdx = (idx - 1 + tabs.length) % tabs.length;
    } else if (e.key === "Home") {
      nextIdx = 0;
    } else if (e.key === "End") {
      nextIdx = tabs.length - 1;
    }
    if (nextIdx !== null) {
      e.preventDefault();
      onChange(tabs[nextIdx]!.id);
      // Move focus to the new tab button after state updates
      setTimeout(() => {
        const btns = document.querySelectorAll('[role="tab"]');
        (btns[nextIdx!] as HTMLButtonElement | null)?.focus();
      }, 0);
    }
  };

  return (
    <div
      role="tablist"
      aria-label="Mobile navigation"
      style={{
        position: "absolute",
        left: 14,
        right: 14,
        bottom: 16,
        height: 64,
        borderRadius: 26,
        zIndex: 30,
        background: dark ? "rgba(34,33,28,0.78)" : "rgba(255,253,247,0.82)",
        backdropFilter: "blur(24px) saturate(160%)",
        WebkitBackdropFilter: "blur(24px) saturate(160%)",
        border: `0.5px solid ${p.border}`,
        boxShadow: dark
          ? "0 8px 28px rgba(0,0,0,0.4), inset 0 0.5px 0 rgba(255,255,255,0.05)"
          : "0 6px 20px rgba(40,30,20,0.07), 0 1px 0 rgba(255,255,255,0.6) inset",
        display: "flex",
        alignItems: "center",
        justifyContent: "space-around",
        padding: "0 10px",
      }}
    >
      {tabs.map((t, idx) => {
        const on = active === t.id;
        return (
          <button
            key={t.id}
            role="tab"
            type="button"
            tabIndex={on ? 0 : -1}
            aria-selected={on}
            aria-label={t.label}
            onClick={() => onChange(t.id)}
            onKeyDown={(e) => handleKeyDown(e, idx)}
            style={{
              background: "none",
              border: "none",
              cursor: "pointer",
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: 3,
              padding: "6px 10px",
              minWidth: 56,
              color: on ? p.accent : p.text3,
            }}
          >
            <span
              aria-hidden="true"
              style={{
                width: 36,
                height: 28,
                borderRadius: 10,
                background: on ? `${p.accent}1a` : "transparent",
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              {Icons[t.icon]({ size: 18 })}
            </span>
            <span
              style={{
                fontSize: 10,
                letterSpacing: "0.02em",
                fontWeight: on ? 600 : 500,
              }}
            >
              {t.label}
            </span>
          </button>
        );
      })}
    </div>
  );
}

// ── Workspace pill (header) ────────────────────────────────────
export function WorkspacePill({
  dark,
  count,
  live = true,
}: {
  dark: boolean;
  count: number | string;
  live?: boolean;
}) {
  const p = usePalette(dark);
  return (
    <div
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 0,
        borderRadius: 999,
        padding: 4,
        background: dark ? "rgba(34,33,28,0.6)" : "rgba(255,255,255,0.7)",
        border: `0.5px solid ${p.border}`,
        backdropFilter: "blur(12px)",
      }}
    >
      <span
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          padding: "6px 12px 6px 8px",
          borderRight: `0.5px solid ${p.divider}`,
        }}
      >
        <span
          style={{
            width: 22,
            height: 22,
            borderRadius: 6,
            background: `linear-gradient(135deg, ${p.accent}, ${p.greenInk})`,
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            color: "white",
            fontSize: 11,
            fontWeight: 700,
          }}
        >
          M
        </span>
        <span style={{ fontSize: 13.5, fontWeight: 600, color: p.text }}>Molecule AI</span>
      </span>
      <span
        style={{
          display: "flex",
          alignItems: "center",
          gap: 6,
          padding: "6px 10px",
          fontFamily: MOBILE_FONT_MONO,
          fontSize: 11,
          color: p.text2,
        }}
      >
        <StatusDot status="online" size={6} dark={dark} />
        <span>{count}</span>
      </span>
      {live && (
        <span
          style={{
            display: "flex",
            alignItems: "center",
            gap: 5,
            padding: "6px 10px 6px 8px",
            fontSize: 11,
            color: p.greenInk,
            fontWeight: 600,
            fontFamily: MOBILE_FONT_MONO,
          }}
        >
          <span
            style={{
              width: 6,
              height: 6,
              borderRadius: 999,
              background: p.online,
              boxShadow: `0 0 0 3px ${p.online}26`,
            }}
          />
          LIVE
        </span>
      )}
    </div>
  );
}

// ── Agent row card ─────────────────────────────────────────────
export function AgentCard({
  agent,
  dark,
  onClick,
  compact = false,
}: {
  agent: MobileAgent;
  dark: boolean;
  onClick?: () => void;
  compact?: boolean;
}) {
  const p = usePalette(dark);
  const isOnline = agent.status === "online";
  const isT4Soft = agent.tier === "T4" && isOnline;
  return (
    <button
      type="button"
      data-testid="workspace-card"
      aria-label={`${agent.name}, status: ${agent.status}, tier ${agent.tier}${agent.remote ? ", remote" : ""}`}
      onClick={onClick}
      style={{
        display: "block",
        width: "100%",
        textAlign: "left",
        cursor: "pointer",
        background: isT4Soft ? p.t4SoftCard : isOnline ? p.greenSoft : p.surface,
        border: `0.5px solid ${p.border}`,
        borderRadius: 18,
        padding: compact ? "12px 14px" : "14px 16px",
        boxShadow: dark
          ? "none"
          : "0 1px 0 rgba(255,255,255,0.5) inset, 0 1px 2px rgba(40,30,20,0.03)",
        transition: "transform .12s",
      }}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <StatusDot status={agent.status} size={9} dark={dark} />
        <span
          style={{
            flex: 1,
            fontSize: 16,
            fontWeight: 600,
            color: p.text,
            letterSpacing: "-0.01em",
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
          }}
        >
          {agent.name}
        </span>
        <TierChip tier={agent.tier} dark={dark} />
      </div>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 6,
          marginTop: 8,
          flexWrap: "wrap",
        }}
      >
        {agent.remote && <RemoteBadge palette={p} />}
        <span
          style={{
            fontSize: 10.5,
            color: p.text3,
            fontFamily: MOBILE_FONT_MONO,
            letterSpacing: "0.02em",
          }}
        >
          {agent.tag}
        </span>
      </div>
      {!compact && agent.desc && (
        <p
          style={{
            margin: "8px 0 0",
            fontSize: 13,
            lineHeight: 1.45,
            color: p.text2,
          }}
        >
          {agent.desc}
        </p>
      )}
      {!compact && (
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: 14,
            marginTop: 10,
            fontSize: 10.5,
            color: p.text3,
            fontFamily: MOBILE_FONT_MONO,
          }}
        >
          <span>SKILLS {agent.skills}</span>
          <span>CALLS {agent.calls}</span>
          <span style={{ marginLeft: "auto" }}>{agent.runtime.toUpperCase()}</span>
        </div>
      )}
    </button>
  );
}

export function RemoteBadge({ palette }: { palette: MobilePalette }) {
  return (
    <span
      style={{
        padding: "2px 7px",
        borderRadius: 4,
        background: palette.remoteBg,
        color: palette.remote,
        fontSize: 10,
        fontWeight: 700,
        letterSpacing: "0.04em",
        fontFamily: MOBILE_FONT_MONO,
        display: "inline-flex",
        alignItems: "center",
        gap: 3,
      }}
    >
      ★ REMOTE
    </span>
  );
}

// ── Filter chips ───────────────────────────────────────────────
export type AgentFilter = "all" | "online" | "issue" | "paused";

export function FilterChips({
  value,
  onChange,
  dark,
  counts,
}: {
  value: AgentFilter;
  onChange: (v: AgentFilter) => void;
  dark: boolean;
  counts: { all: number; online: number; issue: number; paused: number };
}) {
  const p = usePalette(dark);
  const opts: { id: AgentFilter; label: string; n: number }[] = [
    { id: "all", label: "All", n: counts.all },
    { id: "online", label: "Online", n: counts.online },
    { id: "issue", label: "Issues", n: counts.issue },
    { id: "paused", label: "Paused", n: counts.paused },
  ];
  return (
    <div
      role="toolbar"
      aria-label="Filter agents"
      aria-activedescendant={value ? `filter-${value}` : undefined}
      style={{
        display: "flex",
        gap: 6,
        padding: "0 16px 10px",
        overflowX: "auto",
        scrollbarWidth: "none",
      }}
    >
      {opts.map((o) => {
        const on = value === o.id;
        return (
          <button
            key={o.id}
            id={`filter-${o.id}`}
            role="radio"
            type="button"
            aria-checked={on}
            onClick={() => onChange(o.id)}
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
              whiteSpace: "nowrap",
              flexShrink: 0,
            }}
          >
            {o.label}
            <span
              aria-hidden="true"
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
  );
}

export function classifyForFilter(status: MobileStatus): AgentFilter {
  if (status === "online") return "online";
  if (status === "failed" || status === "degraded") return "issue";
  return "paused"; // starting / paused / offline
}
