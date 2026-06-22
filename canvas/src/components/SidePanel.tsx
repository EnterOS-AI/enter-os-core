"use client";

import { useState, useCallback, useRef, useEffect } from "react";
import { useCanvasStore } from "@/store/canvas";
import { StatusDot } from "./StatusDot";
import { WorkspacePanelTabs } from "./WorkspacePanelTabs";
import { summarizeWorkspaceCapabilities } from "@/store/canvas";

const SIDEPANEL_WIDTH_KEY = "molecule:sidepanel-width";
const SIDEPANEL_DEFAULT_WIDTH = 480;
const SIDEPANEL_MIN_WIDTH = 320;
const SIDEPANEL_MAX_WIDTH = 800;

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
  // On mobile (< 640px viewport) the configured width exceeds the screen,
  // so the panel renders off-canvas-left. Force full-viewport width and
  // disable resize on small screens; restore configured width on desktop.
  const [isMobile, setIsMobile] = useState(false);
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia("(max-width: 639px)");
    const update = () => setIsMobile(mq.matches);
    update();
    mq.addEventListener("change", update);
    return () => mq.removeEventListener("change", update);
  }, []);
  useEffect(() => {
    setSidePanelWidth(isMobile ? 0 : width);
  }, [width, isMobile, setSidePanelWidth]);
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
      className={`fixed top-0 right-0 h-full bg-surface/95 backdrop-blur-xl border-line/50 flex flex-col z-50 shadow-2xl shadow-black/50 animate-in slide-in-from-right duration-200 ${
        isMobile ? "left-0 w-screen" : "border-l"
      }`}
      style={isMobile ? undefined : { width }}
    >
      {/* Resize handle — desktop only (no point resizing a full-screen mobile panel) */}
      {!isMobile && (
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
      )}
      {/* Header */}
      <div className="flex items-center justify-between px-4 sm:px-5 py-4 border-b border-line/40 bg-surface-sunken/30">
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
                <span className="text-[10px] text-ink-mid truncate">
                  {node.data.role}
                </span>
              )}
              <span className={`text-[9px] px-1.5 py-0.5 rounded-md font-mono ${
                isOnline ? "text-good bg-emerald-950/30" : "text-ink-mid bg-surface-card/50"
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
          className="w-7 h-7 flex items-center justify-center rounded-lg text-ink-mid hover:text-ink hover:bg-surface-card/60 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1"
        >
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
            <path d="M1 1l10 10M11 1L1 11" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
        </button>
      </div>

      {/* Capability summary */}
      <div className="px-4 sm:px-5 py-3 border-b border-line/40 bg-surface-sunken/20">
        <div className="flex flex-wrap gap-2">
          <MetaPill label="Tier" value={`T${node.data.tier}`} />
          <MetaPill label="Runtime" value={capability.runtime || "unknown"} />
          <MetaPill label="Skills" value={capability.skillCount > 0 ? `${capability.skillCount}` : "none"} />
          <MetaPill label="Status" value={node.data.status} tone={isOnline ? "emerald" : "zinc"} />
        </div>
      </div>

      {/* Tabs + tab content — extracted into WorkspacePanelTabs so the same
          tab bar/body is reused verbatim by the concierge Settings page. The
          map drawer stays store-driven: we thread the global panelTab /
          setPanelTab through as the controlled active-tab pair, preserving the
          existing selection + keyboard behaviour. */}
      <WorkspacePanelTabs node={node} activeTab={panelTab} onTabChange={setPanelTab} />

      {/* Footer — workspace ID */}
      <div className="px-4 sm:px-5 py-2 border-t border-line/40 bg-surface-sunken/20">
        <span className="text-[9px] font-mono text-ink-mid select-all block truncate">
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
