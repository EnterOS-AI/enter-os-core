"use client";

// 03 · Agent detail — pills + tabbed content (Overview/Activity/Config/Memory).

import { useEffect, useMemo, useState } from "react";

import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";

import { RemoteBadge, toMobileAgent } from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, type MobilePalette, usePalette } from "./palette";
import { Icons, StatusDot, TierChip } from "./primitives";

type TabId = "overview" | "activity" | "config" | "memory";

const TABS: { id: TabId; label: string }[] = [
  { id: "overview", label: "Overview" },
  { id: "activity", label: "Activity" },
  { id: "config", label: "Config" },
  { id: "memory", label: "Memory" },
];

export function MobileDetail({
  agentId,
  dark,
  onBack,
  onChat,
}: {
  agentId: string;
  dark: boolean;
  onBack: () => void;
  onChat: () => void;
}) {
  const p = usePalette(dark);
  // Selecting `nodes` stably avoids the `.find()` anti-pattern that
  // creates a new return value on every store update (React error #185).
  const nodes = useCanvasStore((s) => s.nodes);
  const node = useMemo(() => nodes.find((n) => n.id === agentId), [nodes, agentId]);
  const [tab, setTab] = useState<TabId>("overview");

  if (!node) {
    return (
      <div
        style={{
          height: "100%",
          background: p.bg,
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          color: p.text3,
          fontSize: 13,
          fontFamily: MOBILE_FONT_SANS,
        }}
      >
        Agent not found.
      </div>
    );
  }
  const a = toMobileAgent(node);

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
      {/* Top bar */}
      <div
        style={{
          position: "sticky",
          top: 0,
          zIndex: 10,
          padding: "max(env(safe-area-inset-top), 44px) 14px 0",
          background: p.bg,
        }}
      >
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
          <button
            type="button"
            onClick={onBack}
            aria-label="Back"
            style={iconButtonStyle(p, dark)}
          >
            {Icons.back({ size: 18 })}
          </button>
          <button type="button" aria-label="More" style={iconButtonStyle(p, dark)}>
            {Icons.more({ size: 18 })}
          </button>
        </div>
      </div>

      {/* Hero */}
      <div style={{ padding: "20px 20px 16px" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 8 }}>
          <StatusDot status={a.status} size={10} dark={dark} />
          <span
            style={{
              fontFamily: MOBILE_FONT_MONO,
              fontSize: 11,
              color: p.greenInk,
              fontWeight: 600,
              letterSpacing: "0.04em",
              textTransform: "uppercase",
            }}
          >
            {a.status}
          </span>
          {a.remote && <RemoteBadge palette={p} />}
        </div>
        <h1
          style={{
            margin: 0,
            fontSize: 28,
            fontWeight: 700,
            color: p.text,
            letterSpacing: "-0.02em",
          }}
        >
          {a.name}
        </h1>
        <p
          style={{
            margin: "6px 0 0",
            fontSize: 14,
            color: p.text2,
            fontFamily: MOBILE_FONT_MONO,
          }}
        >
          {a.tag}
        </p>
      </div>

      {/* Stat pills */}
      <div
        style={{
          display: "flex",
          gap: 6,
          padding: "0 16px 16px",
          overflowX: "auto",
          scrollbarWidth: "none",
        }}
      >
        <PillStat label="TIER" value={a.tier} accent={p.t4Ink} dark={dark} chip="tier" />
        <PillStat label="RUNTIME" value={a.runtime} dark={dark} />
        <PillStat label="SKILLS" value={a.skills} dark={dark} />
        <PillStat label="STATUS" value={a.status} accent={p.online} dark={dark} dot />
      </div>

      {/* Description card */}
      {a.desc && (
        <div style={{ padding: "0 14px" }}>
          <div
            style={{
              background: p.surface,
              borderRadius: 16,
              border: `0.5px solid ${p.border}`,
              padding: "14px 16px",
            }}
          >
            <p style={{ margin: 0, fontSize: 14.5, lineHeight: 1.5, color: p.text }}>{a.desc}</p>
          </div>
        </div>
      )}

      {/* Tabs */}
      <div
        style={{
          display: "flex",
          gap: 4,
          padding: "20px 14px 10px",
          overflowX: "auto",
          scrollbarWidth: "none",
        }}
      >
        {TABS.map((t) => {
          const on = tab === t.id;
          return (
            <button
              key={t.id}
              type="button"
              onClick={() => setTab(t.id)}
              style={{
                padding: "8px 14px",
                borderRadius: 999,
                border: "none",
                cursor: "pointer",
                background: on ? p.text : "transparent",
                color: on ? (dark ? p.bg : "#fff") : p.text2,
                fontSize: 13,
                fontWeight: 600,
                whiteSpace: "nowrap",
              }}
            >
              {t.label}
            </button>
          );
        })}
      </div>

      {/* Tab content */}
      <div style={{ padding: "0 14px" }}>
        {tab === "overview" && <DetailOverview a={a} dark={dark} />}
        {tab === "activity" && <DetailActivity workspaceId={a.id} dark={dark} />}
        {tab === "config" && <DetailConfig a={a} dark={dark} />}
        {tab === "memory" && <DetailMemory dark={dark} />}
      </div>

      {/* Chat CTA */}
      <div style={{ position: "absolute", left: 14, right: 14, bottom: 92, zIndex: 28 }}>
        <button
          type="button"
          onClick={onChat}
          data-testid="mobile-chat-cta"
          style={{
            width: "100%",
            height: 52,
            borderRadius: 16,
            cursor: "pointer",
            background: p.text,
            color: dark ? p.bg : "#fff",
            border: "none",
            fontSize: 15,
            fontWeight: 600,
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            gap: 10,
            boxShadow: "0 8px 22px rgba(40,30,20,0.22)",
          }}
        >
          {Icons.chat({ size: 18 })} Open chat
        </button>
      </div>
    </div>
  );
}

function iconButtonStyle(p: MobilePalette, dark: boolean) {
  return {
    width: 36,
    height: 36,
    borderRadius: 999,
    cursor: "pointer",
    background: dark ? "#22211c" : "#fff",
    border: `0.5px solid ${p.border}`,
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    color: p.text2,
  } as const;
}

function PillStat({
  label,
  value,
  accent,
  dark,
  dot,
  chip,
}: {
  label: string;
  value: string | number;
  accent?: string;
  dark: boolean;
  dot?: boolean;
  chip?: "tier";
}) {
  const p = usePalette(dark);
  const active = !!accent;
  return (
    <div
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 7,
        padding: "7px 12px",
        borderRadius: 999,
        flexShrink: 0,
        background: active ? `${accent}1a` : dark ? "#22211c" : "#fff",
        border: `0.5px solid ${active ? `${accent}40` : p.border}`,
      }}
    >
      <span
        style={{
          fontSize: 9.5,
          color: active ? accent : p.text3,
          fontFamily: MOBILE_FONT_MONO,
          letterSpacing: "0.06em",
          textTransform: "uppercase",
          fontWeight: 600,
        }}
      >
        {label}
      </span>
      {dot && <StatusDot status="online" size={6} dark={dark} halo={false} />}
      {chip === "tier" ? (
        <TierChip tier={value as "T1" | "T2" | "T3" | "T4"} dark={dark} />
      ) : (
        <span
          style={{
            fontSize: 12,
            color: active ? accent : p.text,
            fontWeight: 600,
            textTransform: label === "STATUS" ? "capitalize" : "none",
          }}
        >
          {value}
        </span>
      )}
    </div>
  );
}

function DetailOverview({
  a,
  dark,
}: {
  a: ReturnType<typeof toMobileAgent>;
  dark: boolean;
}) {
  const p = usePalette(dark);
  const Row = ({ k, v, mono = true }: { k: string; v: string; mono?: boolean }) => (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        padding: "10px 0",
        borderBottom: `0.5px solid ${p.divider}`,
      }}
    >
      <span
        style={{
          fontSize: 11.5,
          color: p.text3,
          letterSpacing: "0.04em",
          fontFamily: MOBILE_FONT_MONO,
          textTransform: "uppercase",
        }}
      >
        {k}
      </span>
      <span
        style={{
          fontSize: 13,
          color: p.text,
          fontWeight: 500,
          fontFamily: mono ? MOBILE_FONT_MONO : "inherit",
          maxWidth: "60%",
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
        }}
      >
        {v}
      </span>
    </div>
  );
  return (
    <div
      style={{
        background: p.surface,
        borderRadius: 16,
        padding: "4px 16px",
        border: `0.5px solid ${p.border}`,
      }}
    >
      <Row k="ID" v={a.id} />
      <Row k="Tier" v={a.tier} />
      <Row k="Runtime" v={a.runtime} />
      <Row k="Active tasks" v={String(a.calls)} />
      <Row k="Skills" v={`${a.skills} loaded`} />
      <Row k="Origin" v={a.remote ? "remote" : "platform"} />
    </div>
  );
}

interface ActivityRecord {
  id: string;
  activity_type: string;
  status: string;
  summary: string | null;
  duration_ms: number | null;
  created_at: string;
}

function DetailActivity({ workspaceId, dark }: { workspaceId: string; dark: boolean }) {
  const p = usePalette(dark);
  const [items, setItems] = useState<ActivityRecord[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    setItems(null);
    api
      .get<ActivityRecord[]>(`/workspaces/${workspaceId}/activity?limit=12`)
      .then((rows) => {
        if (!cancelled) setItems(rows);
      })
      .catch((e: unknown) => {
        if (!cancelled) {
          setError(e instanceof Error ? e.message : "Failed to load activity");
          setItems([]);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);

  if (items === null) {
    return (
      <div
        role="status"
        aria-live="polite"
        style={{
          background: p.surface,
          borderRadius: 16,
          padding: "20px 16px",
          border: `0.5px solid ${p.border}`,
          color: p.text3,
          fontSize: 13,
        }}
      >
        Loading activity…
      </div>
    );
  }

  if (items.length === 0) {
    return (
      <div
        style={{
          background: p.surface,
          borderRadius: 16,
          padding: "20px 16px",
          border: `0.5px solid ${p.border}`,
          color: p.text3,
          fontSize: 13,
        }}
      >
        {error ?? "No recent activity. New events appear here as the agent reports them."}
      </div>
    );
  }

  return (
    <div
      style={{
        background: p.surface,
        borderRadius: 16,
        padding: "6px 16px",
        border: `0.5px solid ${p.border}`,
      }}
    >
      {items.map((it, i) => {
        const ts = new Date(it.created_at);
        const label = isNaN(ts.getTime())
          ? ""
          : ts.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
        const isErr = it.status === "error" || it.status === "err";
        return (
          <div
            key={it.id}
            style={{
              display: "flex",
              gap: 12,
              padding: "12px 0",
              borderBottom: i < items.length - 1 ? `0.5px solid ${p.divider}` : "none",
            }}
          >
            <span
              style={{
                fontSize: 11,
                color: p.text3,
                paddingTop: 2,
                width: 48,
                fontFamily: MOBILE_FONT_MONO,
                flexShrink: 0,
              }}
            >
              {label}
            </span>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 6,
                  fontSize: 11,
                  color: p.text3,
                  fontFamily: MOBILE_FONT_MONO,
                  letterSpacing: "0.02em",
                  marginBottom: 2,
                }}
              >
                <span
                  style={{
                    padding: "1px 5px",
                    borderRadius: 4,
                    background: isErr ? "#f5dad2" : "#dde9e1",
                    color: isErr ? "#a8341a" : p.greenInk,
                    fontSize: 9,
                    fontWeight: 700,
                    letterSpacing: "0.06em",
                  }}
                >
                  {isErr ? "ERR" : "OK"}
                </span>
                <span>{it.activity_type}</span>
                {it.duration_ms != null && <span>· {it.duration_ms}ms</span>}
              </div>
              {it.summary && (
                <span
                  style={{
                    fontSize: 13.5,
                    color: p.text,
                    lineHeight: 1.45,
                    overflowWrap: "anywhere",
                  }}
                >
                  {it.summary}
                </span>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function DetailConfig({
  a,
  dark,
}: {
  a: ReturnType<typeof toMobileAgent>;
  dark: boolean;
}) {
  const p = usePalette(dark);
  const cfg = JSON.stringify(
    {
      tier: a.tier,
      runtime: a.runtime,
      skills: a.skills,
      remote: a.remote,
    },
    null,
    2,
  );
  return (
    <pre
      style={{
        background: dark ? "#0f0e0a" : "#fff",
        borderRadius: 16,
        padding: "14px 16px",
        border: `0.5px solid ${p.border}`,
        fontFamily: MOBILE_FONT_MONO,
        fontSize: 11.5,
        lineHeight: 1.55,
        color: p.text2,
        margin: 0,
        overflow: "auto",
        whiteSpace: "pre-wrap",
      }}
    >
      {cfg}
    </pre>
  );
}

function DetailMemory({ dark }: { dark: boolean }) {
  const p = usePalette(dark);
  return (
    <div
      style={{
        background: p.surface,
        borderRadius: 16,
        padding: "14px 16px",
        border: `0.5px solid ${p.border}`,
        fontSize: 13,
        color: p.text2,
        lineHeight: 1.5,
      }}
    >
      <span style={{ color: p.text }}>Ephemeral session.</span> Memory clears on workspace
      restart. Open the desktop canvas for the full memory inspector.
    </div>
  );
}
