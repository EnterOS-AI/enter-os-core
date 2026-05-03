"use client";

import { useState, useCallback, useRef, useEffect } from "react";
import { useCanvasStore, type PanelTab } from "@/store/canvas";
import { showToast } from "@/components/Toaster";
import { StatusDot } from "./StatusDot";
import { Tooltip } from "./Tooltip";
import { DetailsTab } from "./tabs/DetailsTab";
import { SkillsTab } from "./tabs/SkillsTab";
import { ChatTab } from "./tabs/ChatTab";
import { ConfigTab } from "./tabs/ConfigTab";
import { TerminalTab } from "./tabs/TerminalTab";
import { FilesTab } from "./tabs/FilesTab";
import { MemoryInspectorPanel } from "./MemoryInspectorPanel";
import { AuditTrailPanel } from "./AuditTrailPanel";
import { TracesTab } from "./tabs/TracesTab";
import { EventsTab } from "./tabs/EventsTab";
import { ActivityTab } from "./tabs/ActivityTab";
import { ScheduleTab } from "./tabs/ScheduleTab";
import { ChannelsTab } from "./tabs/ChannelsTab";
import { summarizeWorkspaceCapabilities } from "@/store/canvas";

const SIDEPANEL_WIDTH_KEY = "molecule:sidepanel-width";
const SIDEPANEL_DEFAULT_WIDTH = 480;
const SIDEPANEL_MIN_WIDTH = 320;
const SIDEPANEL_MAX_WIDTH = 800;

const TABS: { id: PanelTab; label: string; icon: string }[] = [
  { id: "chat", label: "Chat", icon: "◈" },
  { id: "activity", label: "Activity", icon: "⊙" },
  { id: "details", label: "Details", icon: "◉" },
  { id: "skills", label: "Plugins", icon: "✦" },
  { id: "terminal", label: "Terminal", icon: "▸" },
  { id: "config", label: "Config", icon: "⚙" },
  { id: "schedule", label: "Schedule", icon: "⏲" },
  { id: "channels", label: "Channels", icon: "⇌" },
  { id: "files", label: "Files", icon: "⊞" },
  { id: "memory", label: "Memory", icon: "◇" },
  { id: "traces", label: "Traces", icon: "◎" },
  { id: "events", label: "Events", icon: "◊" },
  { id: "audit",  label: "Audit",  icon: "⊟" },
];

export function SidePanel() {
  const selectedNodeId = useCanvasStore((s) => s.selectedNodeId);
  const panelTab = useCanvasStore((s) => s.panelTab);
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);
  const selectNode = useCanvasStore((s) => s.selectNode);
  const setSidePanelWidth = useCanvasStore((s) => s.setSidePanelWidth);
  const node = useCanvasStore((s) =>
    s.nodes.find((n) => n.id === s.selectedNodeId)
  );

  // Resizable panel width — persisted across node selections via localStorage.
  // Also published to the canvas store on every change so the centered
  // Toolbar can re-centre itself on the remaining canvas area (avoids the
  // Audit / Search / Settings buttons hiding under the panel).
  const [width, setWidth] = useState<number>(() => {
    if (typeof window === "undefined") return SIDEPANEL_DEFAULT_WIDTH;
    const saved = localStorage.getItem(SIDEPANEL_WIDTH_KEY);
    const parsed = saved ? parseInt(saved, 10) : NaN;
    return Number.isFinite(parsed) && parsed >= SIDEPANEL_MIN_WIDTH
      ? parsed
      : SIDEPANEL_DEFAULT_WIDTH;
  });
  useEffect(() => {
    setSidePanelWidth(width);
  }, [width, setSidePanelWidth]);
  const widthRef = useRef(width); // tracks live drag value for the mouseup handler
  const dragging = useRef(false);
  const startX = useRef(0);
  const startWidth = useRef(SIDEPANEL_DEFAULT_WIDTH);

  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    dragging.current = true;
    startX.current = e.clientX;
    startWidth.current = width;
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
  }, [width]);

  const onResizeKeyDown = useCallback((e: React.KeyboardEvent) => {
    const STEP = 16;
    let newWidth: number | null = null;
    if (e.key === "ArrowLeft") {
      e.preventDefault();
      newWidth = Math.min(width + STEP, SIDEPANEL_MAX_WIDTH);
    } else if (e.key === "ArrowRight") {
      e.preventDefault();
      newWidth = Math.max(width - STEP, SIDEPANEL_MIN_WIDTH);
    } else if (e.key === "Home") {
      e.preventDefault();
      newWidth = SIDEPANEL_MIN_WIDTH;
    } else if (e.key === "End") {
      e.preventDefault();
      newWidth = SIDEPANEL_MAX_WIDTH;
    }
    if (newWidth !== null) {
      setWidth(newWidth);
      widthRef.current = newWidth;
      localStorage.setItem(SIDEPANEL_WIDTH_KEY, String(newWidth));
    }
  }, [width]);

  useEffect(() => {
    const onMouseMove = (e: MouseEvent) => {
      if (!dragging.current) return;
      const delta = startX.current - e.clientX;
      const newWidth = Math.min(
        Math.max(startWidth.current + delta, SIDEPANEL_MIN_WIDTH),
        window.innerWidth * 0.8,
      );
      setWidth(newWidth);
      widthRef.current = newWidth; // keep ref in sync so mouseUp can persist it
    };
    const onMouseUp = () => {
      if (!dragging.current) return;
      dragging.current = false;
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      // Persist the final dragged width so it survives node re-selection
      localStorage.setItem(SIDEPANEL_WIDTH_KEY, String(widthRef.current));
    };
    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", onMouseUp);
    return () => {
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", onMouseUp);
    };
  }, []);

  if (!selectedNodeId || !node) return null;

  const isOnline = node.data.status === "online";
  const capability = summarizeWorkspaceCapabilities(node.data);

  return (
    <div
      className="fixed top-0 right-0 h-full bg-surface/95 backdrop-blur-xl border-l border-line/50 flex flex-col z-50 shadow-2xl shadow-black/50 animate-in slide-in-from-right duration-200"
      style={{ width }}
    >
      {/* Resize handle */}
      <div
        role="separator"
        aria-label="Resize workspace panel"
        aria-valuenow={width}
        aria-valuemin={SIDEPANEL_MIN_WIDTH}
        aria-valuemax={SIDEPANEL_MAX_WIDTH}
        aria-orientation="vertical"
        tabIndex={0}
        onMouseDown={onMouseDown}
        onKeyDown={onResizeKeyDown}
        className="absolute left-0 top-0 bottom-0 w-1.5 cursor-col-resize hover:bg-accent/30 active:bg-accent/50 transition-colors z-10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-inset"
      />
      {/* Header */}
      <div className="flex items-center justify-between px-5 py-4 border-b border-line/40 bg-surface-sunken/30">
        <div className="flex items-center gap-3 min-w-0">
          <div className="relative">
            <StatusDot status={node.data.status} size="md" />
          </div>
          <div className="min-w-0">
            <h2 className="text-[14px] font-semibold text-ink truncate leading-tight">
              {node.data.name}
            </h2>
            <div className="flex items-center gap-2 mt-0.5">
              {node.data.role && (
                <span className="text-[10px] text-ink-soft truncate">
                  {node.data.role}
                </span>
              )}
              <span className={`text-[9px] px-1.5 py-0.5 rounded-md font-mono ${
                isOnline ? "text-good bg-emerald-950/30" : "text-ink-soft bg-surface-card/50"
              }`}>
                T{node.data.tier}
              </span>
            </div>
          </div>
        </div>
        <button
          type="button"
          onClick={() => selectNode(null)}
          aria-label="Close workspace panel"
          className="w-7 h-7 flex items-center justify-center rounded-lg text-ink-soft hover:text-ink hover:bg-surface-card/60 transition-colors"
        >
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
            <path d="M1 1l10 10M11 1L1 11" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
        </button>
      </div>

      {/* Capability summary */}
      <div className="px-5 py-3 border-b border-line/40 bg-surface-sunken/20">
        <div className="flex flex-wrap gap-2">
          <MetaPill label="Tier" value={`T${node.data.tier}`} />
          <MetaPill label="Runtime" value={capability.runtime || "unknown"} />
          <MetaPill label="Skills" value={capability.skillCount > 0 ? `${capability.skillCount}` : "none"} />
          <MetaPill label="Status" value={node.data.status} tone={isOnline ? "emerald" : "zinc"} />
        </div>
      </div>

      {/* Tabs — relative wrapper lets the fade gradient position against the scroll container */}
      <div className="relative border-b border-line/40">
        {/* Right-edge fade: signals more tabs are hidden off-screen when the bar overflows */}
        <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-zinc-950 to-transparent z-10" aria-hidden="true" />
      <div
        role="tablist"
        aria-label="Workspace panel tabs"
        className="flex overflow-x-auto bg-surface-sunken/20 px-1"
        onKeyDown={(e) => {
          const idx = TABS.findIndex((t) => t.id === panelTab);
          let next: number | null = null;
          if (e.key === "ArrowRight") { e.preventDefault(); next = (idx + 1) % TABS.length; }
          else if (e.key === "ArrowLeft") { e.preventDefault(); next = (idx - 1 + TABS.length) % TABS.length; }
          else if (e.key === "Home") { e.preventDefault(); next = 0; }
          else if (e.key === "End") { e.preventDefault(); next = TABS.length - 1; }
          if (next !== null) {
            setPanelTab(TABS[next].id);
            requestAnimationFrame(() => { const el = document.getElementById(`tab-${TABS[next!].id}`); el?.focus(); el?.scrollIntoView({ block: "nearest", inline: "nearest" }); });
          }
        }}
      >
        {TABS.map((tab) => (
          <button
            type="button"
            key={tab.id}
            id={`tab-${tab.id}`}
            role="tab"
            aria-selected={panelTab === tab.id}
            aria-controls={`panel-${tab.id}`}
            tabIndex={panelTab === tab.id ? 0 : -1}
            onClick={() => setPanelTab(tab.id)}
            className={`shrink-0 px-3 py-2.5 text-[10px] font-medium tracking-wide transition-all rounded-t-lg mx-0.5 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70 ${
              panelTab === tab.id
                ? "text-ink bg-surface-card/40 border-b-2 border-accent"
                : "text-ink-soft hover:text-ink hover:bg-surface-card/40"
            }`}
          >
            <span className="mr-1 opacity-50" aria-hidden="true">{tab.icon}</span>
            {tab.label}
          </button>
        ))}
      </div>
      </div>

      {/* Needs Restart Banner */}
      {node.data.needsRestart && !node.data.currentTask && selectedNodeId && (
        <div className="px-4 py-2 bg-sky-950/20 border-b border-sky-800/20 flex items-center justify-between">
          <span className="text-[10px] text-sky-300/90">Config changed — restart to apply</span>
          <button
            type="button"
            onClick={() => {
              useCanvasStore.getState().restartWorkspace(selectedNodeId).catch(() => showToast("Restart failed", "error"));
            }}
            className="text-[11px] px-2 py-1 bg-sky-800/40 hover:bg-sky-700/50 text-sky-200 rounded transition-colors"
          >
            Restart Now
          </button>
        </div>
      )}

      {/* Current Task Banner */}
      {node.data.currentTask && (
        <Tooltip text={node.data.currentTask as string}>
          <div className="px-4 py-2 bg-amber-950/20 border-b border-amber-800/20 flex items-center gap-2 cursor-default">
            <div className="w-1.5 h-1.5 rounded-full bg-amber-400 motion-safe:animate-pulse shrink-0" />
            <span className="text-[10px] text-warm/90 truncate">
              {node.data.currentTask}
            </span>
          </div>
        </Tooltip>
      )}

      {/* Tab Content */}
      <div
        role="tabpanel"
        id={`panel-${panelTab}`}
        aria-labelledby={`tab-${panelTab}`}
        tabIndex={0}
        className="flex-1 overflow-y-auto focus:outline-none"
      >
        {panelTab === "details" && <DetailsTab key={selectedNodeId} workspaceId={selectedNodeId} data={node.data} />}
        {panelTab === "skills" && <SkillsTab key={selectedNodeId} workspaceId={selectedNodeId} data={node.data} />}
        {panelTab === "activity" && <ActivityTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "chat" && <ChatTab key={selectedNodeId} workspaceId={selectedNodeId} data={node.data} />}
        {panelTab === "terminal" && <TerminalTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "config" && <ConfigTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "schedule" && <ScheduleTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "channels" && <ChannelsTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "files" && <FilesTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "memory" && <MemoryInspectorPanel key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "traces" && <TracesTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "events" && <EventsTab key={selectedNodeId} workspaceId={selectedNodeId} />}
        {panelTab === "audit" && <AuditTrailPanel key={selectedNodeId} workspaceId={selectedNodeId} />}
      </div>

      {/* Footer — workspace ID */}
      <div className="px-5 py-2 border-t border-line/40 bg-surface-sunken/20">
        <span className="text-[9px] font-mono text-ink-soft select-all">
          {selectedNodeId}
        </span>
      </div>
    </div>
  );
}

function MetaPill({ label, value, tone = "zinc" }: { label: string; value: string; tone?: "zinc" | "emerald" | "amber" }) {
  const toneClasses = {
    zinc: "border-line/50 bg-surface-sunken/70 text-ink-mid",
    emerald: "border-emerald-500/20 bg-emerald-950/20 text-good",
    amber: "border-amber-500/20 bg-amber-950/20 text-warm",
  }[tone];

  return (
    <span className={`inline-flex items-center gap-1 rounded-full border px-2 py-1 text-[9px] ${toneClasses}`}>
      <span className="uppercase tracking-[0.18em] text-[8px] opacity-70">{label}</span>
      <span className="font-medium">{value}</span>
    </span>
  );
}
