"use client";

import { useState } from "react";
import type { Node } from "@xyflow/react";
import {
  useCanvasStore,
  type PanelTab,
  type WorkspaceNodeData,
} from "@/store/canvas";
import { showToast } from "@/components/Toaster";
import { Tooltip } from "./Tooltip";
import { DetailsTab } from "./tabs/DetailsTab";
import { SkillsTab } from "./tabs/SkillsTab";
import { ChatTab } from "./tabs/ChatTab";
import { ConfigTab } from "./tabs/ConfigTab";
import { ContainerConfigTab } from "./tabs/ContainerConfigTab";
import { DisplayTab } from "./tabs/DisplayTab";
import { TerminalTab } from "./tabs/TerminalTab";
import { FilesTab } from "./tabs/FilesTab";
import { MemoryInspectorPanel } from "./MemoryInspectorPanel";
import { AuditTrailPanel } from "./AuditTrailPanel";
import { TracesTab } from "./tabs/TracesTab";
import { EventsTab } from "./tabs/EventsTab";
import { ActivityTab } from "./tabs/ActivityTab";
import { ScheduleTab } from "./tabs/ScheduleTab";
import { ChannelsTab } from "./tabs/ChannelsTab";

/**
 * Canonical workspace tab set — the SAME ids/labels/icons the map's
 * SidePanel has always rendered. Single source of truth so the map drawer
 * and any other host (the concierge Settings page) can't drift.
 */
export const WORKSPACE_PANEL_TABS: { id: PanelTab; label: string; icon: string }[] = [
  { id: "chat", label: "Chat", icon: "◈" },
  { id: "activity", label: "Activity", icon: "⊙" },
  { id: "details", label: "Details", icon: "◉" },
  { id: "skills", label: "Plugins", icon: "✦" },
  { id: "terminal", label: "Terminal", icon: "▸" },
  { id: "display", label: "Display", icon: "▣" },
  { id: "container-config", label: "Container", icon: "▤" },
  { id: "config", label: "Config", icon: "⚙" },
  { id: "schedule", label: "Schedule", icon: "⏲" },
  { id: "channels", label: "Channels", icon: "⇌" },
  { id: "files", label: "Files", icon: "⊞" },
  { id: "memory", label: "Memory", icon: "◇" },
  { id: "traces", label: "Traces", icon: "◎" },
  { id: "events", label: "Events", icon: "◊" },
  { id: "audit", label: "Audit", icon: "⊟" },
];

interface Props {
  /** The workspace node whose tabs to render (id + data blob). */
  node: Node<WorkspaceNodeData>;
  /**
   * Controlled active tab. When provided together with `onTabChange`, the
   * caller owns the active-tab state (the map's SidePanel threads the global
   * `panelTab`/`setPanelTab` here so the store stays the source of truth and
   * the existing keyboard/selection behaviour is preserved verbatim).
   * When omitted, the component manages its OWN local active-tab state —
   * which is what the concierge Settings page uses so the embedded tabs
   * don't fight the map's selection.
   */
  activeTab?: PanelTab;
  onTabChange?: (tab: PanelTab) => void;
  /** Initial tab for the uncontrolled (local-state) mode. Defaults to "chat". */
  defaultTab?: PanelTab;
  /**
   * Namespace for the tab/panel element ids (`{idPrefix}tab-chat`,
   * `{idPrefix}panel-chat`). The primary map SidePanel keeps the default ""
   * (stable `#tab-chat` hooks for tests/tools); any EMBEDDED instance MUST
   * pass a unique prefix — two instances with the default collide on
   * duplicate DOM ids, which is invalid HTML, breaks aria-controls, and
   * broke the chat E2E with a Playwright strict-mode violation
   * (`locator('#tab-chat') resolved to 2 elements`).
   */
  idPrefix?: string;
}

/**
 * The workspace tab bar + tab body, extracted from SidePanel so it can be
 * reused verbatim outside the map (e.g. the concierge Settings "Platform
 * agent configuration" section). Renders the canonical ARIA tablist and the
 * exact same tab content components keyed on the active tab.
 *
 * Does NOT render the workspace header / meta pills / resize handle / footer —
 * those are host chrome and stay in the host (SidePanel for the map).
 */
export function WorkspacePanelTabs({ node, activeTab, onTabChange, defaultTab = "chat", idPrefix = "" }: Props) {
  const restartWorkspace = useCanvasStore((s) => s.restartWorkspace);

  // Controlled when both props are present; otherwise own the state locally.
  const controlled = activeTab !== undefined && onTabChange !== undefined;
  const [localTab, setLocalTab] = useState<PanelTab>(defaultTab);
  const tab = controlled ? (activeTab as PanelTab) : localTab;
  const setTab = (next: PanelTab) => {
    if (controlled) onTabChange!(next);
    else setLocalTab(next);
  };

  const workspaceId = node.id;
  const data = node.data;

  return (
    <>
      {/* Tabs — relative wrapper lets the fade gradient position against the scroll container */}
      <div className="relative border-b border-line/40">
        {/* Right-edge fade: signals more tabs are hidden off-screen when the bar overflows */}
        <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-surface to-transparent z-10" aria-hidden="true" />
        <div
          role="tablist"
          aria-label="Workspace panel tabs"
          className="flex overflow-x-auto bg-surface-sunken/20 px-1"
          onKeyDown={(e) => {
            const idx = WORKSPACE_PANEL_TABS.findIndex((t) => t.id === tab);
            let next: number | null = null;
            if (e.key === "ArrowRight") { e.preventDefault(); next = (idx + 1) % WORKSPACE_PANEL_TABS.length; }
            else if (e.key === "ArrowLeft") { e.preventDefault(); next = (idx - 1 + WORKSPACE_PANEL_TABS.length) % WORKSPACE_PANEL_TABS.length; }
            else if (e.key === "Home") { e.preventDefault(); next = 0; }
            else if (e.key === "End") { e.preventDefault(); next = WORKSPACE_PANEL_TABS.length - 1; }
            if (next !== null) {
              setTab(WORKSPACE_PANEL_TABS[next].id);
              requestAnimationFrame(() => { const el = document.getElementById(`${idPrefix}tab-${WORKSPACE_PANEL_TABS[next!].id}`); el?.focus(); el?.scrollIntoView({ block: "nearest", inline: "nearest" }); });
            }
          }}
        >
          {WORKSPACE_PANEL_TABS.map((t) => (
            <button
              type="button"
              key={t.id}
              id={`${idPrefix}tab-${t.id}`}
              role="tab"
              aria-selected={tab === t.id}
              aria-controls={`${idPrefix}panel-${t.id}`}
              tabIndex={tab === t.id ? 0 : -1}
              onClick={() => setTab(t.id)}
              className={`shrink-0 px-3 py-2.5 text-[10px] font-medium tracking-wide transition-all rounded-t-lg mx-0.5 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70 ${
                tab === t.id
                  ? "text-ink bg-surface-card border-b-2 border-accent"
                  : "text-ink-mid hover:text-ink hover:bg-surface-card/60"
              }`}
            >
              <span className="mr-1 opacity-50" aria-hidden="true">{t.icon}</span>
              {t.label}
            </button>
          ))}
        </div>
      </div>

      {/* Needs Restart Banner */}
      {data.needsRestart && !data.currentTask && (
        <div className="px-4 py-2 bg-sky-950/20 border-b border-sky-800/20 flex items-center justify-between">
          <span className="text-[10px] text-sky-300/90">Config changed — restart to apply</span>
          <button
            type="button"
            onClick={() => {
              restartWorkspace(workspaceId).catch(() => showToast("Restart failed", "error"));
            }}
            className="text-[11px] px-2 py-1 bg-sky-800/40 hover:bg-sky-700/50 text-sky-200 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1"
          >
            Restart Now
          </button>
        </div>
      )}

      {/* Current Task Banner */}
      {data.currentTask && (
        <Tooltip text={data.currentTask as string}>
          <div className="px-4 py-2 bg-amber-950/20 border-b border-amber-800/20 flex items-center gap-2 cursor-default">
            <div className="w-1.5 h-1.5 rounded-full bg-amber-400 motion-safe:animate-pulse shrink-0" />
            <span className="text-[10px] text-warm/90 truncate">
              {data.currentTask}
            </span>
          </div>
        </Tooltip>
      )}

      {/* Tab Content */}
      <div
        role="tabpanel"
        id={`${idPrefix}panel-${tab}`}
        aria-labelledby={`${idPrefix}tab-${tab}`}
        tabIndex={0}
        className="flex-1 overflow-y-auto focus:outline-none"
      >
        {tab === "details" && <DetailsTab key={workspaceId} workspaceId={workspaceId} data={data} />}
        {tab === "skills" && <SkillsTab key={workspaceId} workspaceId={workspaceId} data={data} />}
        {tab === "activity" && <ActivityTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "chat" && <ChatTab key={workspaceId} workspaceId={workspaceId} data={data} />}
        {tab === "terminal" && <TerminalTab key={workspaceId} workspaceId={workspaceId} data={data} />}
        {tab === "display" && <DisplayTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "container-config" && (
          <ContainerConfigTab key={workspaceId} workspaceId={workspaceId} data={data} />
        )}
        {tab === "config" && <ConfigTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "schedule" && <ScheduleTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "channels" && <ChannelsTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "files" && <FilesTab key={workspaceId} workspaceId={workspaceId} data={data} />}
        {tab === "memory" && <MemoryInspectorPanel key={workspaceId} workspaceId={workspaceId} />}
        {tab === "traces" && <TracesTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "events" && <EventsTab key={workspaceId} workspaceId={workspaceId} />}
        {tab === "audit" && <AuditTrailPanel key={workspaceId} workspaceId={workspaceId} />}
      </div>
    </>
  );
}
