"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { api } from "@/lib/api";
import { showToast } from "./Toaster";
import { statusDotClass } from "@/lib/design-tokens";

interface MenuItem {
  label: string;
  icon: string;
  action: () => void;
  danger?: boolean;
  disabled?: boolean;
  divider?: boolean;
}

export function ContextMenu() {
  const contextMenu = useCanvasStore((s) => s.contextMenu);
  const closeContextMenu = useCanvasStore((s) => s.closeContextMenu);
  const updateNodeData = useCanvasStore((s) => s.updateNodeData);
  const selectNode = useCanvasStore((s) => s.selectNode);
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);
  const nestNode = useCanvasStore((s) => s.nestNode);
  const contextNodeId = contextMenu?.nodeId ?? null;
  const hasChildren = useCanvasStore((s) =>
    contextNodeId ? s.nodes.some((n) => n.data.parentId === contextNodeId) : false
  );
  const setPendingDelete = useCanvasStore((s) => s.setPendingDelete);
  const ref = useRef<HTMLDivElement>(null);
  const [actionLoading, setActionLoading] = useState(false);
  // Clamped position — (left, top) from contextMenu may overflow when the
  // user right-clicks near the right/bottom viewport edge. We measure the
  // rendered menu and shift it back inside on the same frame the cursor
  // opens it, so it never visibly clips. Falls back to the raw cursor
  // coords until the rAF runs.
  const [clamped, setClamped] = useState<{ x: number; y: number } | null>(null);

  // Auto-focus first enabled item when menu opens, AND clamp position.
  // Both run together in a single rAF so we avoid two synchronous layout
  // reads + a paint between them.
  useEffect(() => {
    if (!contextMenu) return;
    setClamped(null);
    const raf = requestAnimationFrame(() => {
      const node = ref.current;
      if (!node) return;
      const first = node.querySelector<HTMLButtonElement>("button:not(:disabled)");
      first?.focus();
      // 8px viewport margin so the menu doesn't kiss the edge — matches
      // the floating-tooltip top-edge clamp in Tooltip.tsx.
      const margin = 8;
      const rect = node.getBoundingClientRect();
      const vw = window.innerWidth;
      const vh = window.innerHeight;
      let x = contextMenu.x;
      let y = contextMenu.y;
      if (x + rect.width + margin > vw) x = Math.max(margin, vw - rect.width - margin);
      if (y + rect.height + margin > vh) y = Math.max(margin, vh - rect.height - margin);
      if (x !== contextMenu.x || y !== contextMenu.y) setClamped({ x, y });
    });
    return () => cancelAnimationFrame(raf);
  }, [contextMenu?.nodeId, contextMenu?.x, contextMenu?.y]);

  // Close on click outside or Escape
  useEffect(() => {
    if (!contextMenu) return;
    const handleClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as HTMLElement)) {
        closeContextMenu();
      }
    };
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") closeContextMenu();
    };
    document.addEventListener("mousedown", handleClick);
    document.addEventListener("keydown", handleKey);
    return () => {
      document.removeEventListener("mousedown", handleClick);
      document.removeEventListener("keydown", handleKey);
    };
  }, [contextMenu, closeContextMenu]);

  // Arrow-key navigation within the menu
  const handleMenuKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      if (e.key === "Escape") {
        closeContextMenu();
        return;
      }
      if (e.key === "Tab") {
        e.preventDefault();
        closeContextMenu();
        return;
      }
      if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
      e.preventDefault();
      const buttons = Array.from(
        ref.current?.querySelectorAll<HTMLButtonElement>("button:not(:disabled)") ?? []
      );
      const active = document.activeElement as HTMLButtonElement;
      const idx = buttons.indexOf(active);
      const next =
        e.key === "ArrowDown"
          ? idx === -1
            ? 0
            : (idx + 1) % buttons.length
          : idx <= 0
          ? buttons.length - 1
          : idx - 1;
      buttons[next]?.focus();
    },
    [closeContextMenu]
  );

  const handleExportBundle = useCallback(async () => {
    if (!contextMenu || actionLoading) return;
    setActionLoading(true);
    try {
      const bundle = await api.get<Record<string, unknown>>(`/bundles/export/${contextMenu.nodeId}`);
      const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${(contextMenu.nodeData.name || "workspace").toLowerCase().replace(/\s+/g, "-")}.bundle.json`;
      a.click();
      URL.revokeObjectURL(url);
      showToast("Bundle exported", "success");
    } catch (e) {
      showToast("Export failed", "error");
    } finally {
      setActionLoading(false);
    }
    closeContextMenu();
  }, [contextMenu, closeContextMenu, actionLoading]);

  const handleDuplicate = useCallback(async () => {
    if (!contextMenu || actionLoading) return;
    setActionLoading(true);
    try {
      const bundle = await api.get<Record<string, unknown>>(`/bundles/export/${contextMenu.nodeId}`);
      await api.post("/bundles/import", bundle);
    } catch (e) {
      showToast("Duplicate failed", "error");
    } finally {
      setActionLoading(false);
    }
    closeContextMenu();
  }, [contextMenu, closeContextMenu, actionLoading]);

  const handleRestart = useCallback(async () => {
    if (!contextMenu) return;
    try {
      await api.post(`/workspaces/${contextMenu.nodeId}/restart`, {});
      updateNodeData(contextMenu.nodeId, { status: "provisioning" });
    } catch (e) {
      showToast("Restart failed", "error");
    }
    closeContextMenu();
  }, [contextMenu, updateNodeData, closeContextMenu]);

  const handlePause = useCallback(async () => {
    if (!contextMenu) return;
    const nodeId = contextMenu.nodeId;
    closeContextMenu();
    try {
      await api.post(`/workspaces/${nodeId}/pause`, {});
      updateNodeData(nodeId, { status: "paused" });
    } catch (e) {
      showToast("Pause failed", "error");
    }
  }, [contextMenu, updateNodeData, closeContextMenu]);

  const handleResume = useCallback(async () => {
    if (!contextMenu) return;
    const nodeId = contextMenu.nodeId;
    closeContextMenu();
    try {
      await api.post(`/workspaces/${nodeId}/resume`, {});
      updateNodeData(nodeId, { status: "provisioning" });
    } catch (e) {
      showToast("Resume failed", "error");
    }
  }, [contextMenu, updateNodeData, closeContextMenu]);

  const handleDelete = useCallback(() => {
    if (!contextMenu) return;
    // Hoist delete confirmation to the Canvas-level dialog (via store) so
    // it survives ContextMenu unmount. Closing the menu here avoids the
    // prior race where the portal dialog's Confirm click was treated as
    // "outside" by the menu's outside-click handler.
    const childNodes = useCanvasStore.getState().nodes.filter((n) => n.data.parentId === contextMenu.nodeId);
    setPendingDelete({ id: contextMenu.nodeId, name: contextMenu.nodeData.name, hasChildren, children: childNodes.map(c => ({ id: c.id, name: c.data.name })) });
    closeContextMenu();
  }, [contextMenu, setPendingDelete, closeContextMenu]);

  const handleViewDetails = useCallback(() => {
    if (!contextMenu) return;
    selectNode(contextMenu.nodeId);
    setPanelTab("details");
    closeContextMenu();
  }, [contextMenu, selectNode, setPanelTab, closeContextMenu]);

  const handleOpenChat = useCallback(() => {
    if (!contextMenu) return;
    selectNode(contextMenu.nodeId);
    setPanelTab("chat");
    closeContextMenu();
  }, [contextMenu, selectNode, setPanelTab, closeContextMenu]);

  const handleOpenTerminal = useCallback(() => {
    if (!contextMenu) return;
    selectNode(contextMenu.nodeId);
    setPanelTab("terminal");
    closeContextMenu();
  }, [contextMenu, selectNode, setPanelTab, closeContextMenu]);

  const setCollapsed = useCanvasStore((s) => s.setCollapsed);
  const handleCollapse = useCallback(async () => {
    if (!contextMenu) return;
    const nodeId = contextMenu.nodeId;
    const wasCollapsed = !!contextMenu.nodeData.collapsed;
    // Optimistic local flip so the card shrinks/expands immediately.
    // Descendants' hidden flags are toggled atomically by the store.
    setCollapsed(nodeId, !wasCollapsed);
    try {
      await api.patch(`/workspaces/${nodeId}`, { collapsed: !wasCollapsed });
    } catch (e) {
      setCollapsed(nodeId, wasCollapsed);
      showToast("Collapse failed", "error");
    }
    closeContextMenu();
  }, [contextMenu, setCollapsed, closeContextMenu]);

  const handleRemoveFromTeam = useCallback(async () => {
    if (!contextMenu) return;
    try {
      await nestNode(contextMenu.nodeId, null);
      showToast("Extracted from team", "success");
    } catch {
      showToast("Extract failed", "error");
    }
    closeContextMenu();
  }, [contextMenu, nestNode, closeContextMenu]);

  const arrangeChildren = useCanvasStore((s) => s.arrangeChildren);
  const handleArrangeChildren = useCallback(() => {
    if (!contextMenu) return;
    arrangeChildren(contextMenu.nodeId);
    closeContextMenu();
  }, [contextMenu, arrangeChildren, closeContextMenu]);

  const handleZoomToTeam = useCallback(() => {
    if (!contextMenu) return;
    window.dispatchEvent(
      new CustomEvent("molecule:zoom-to-team", { detail: { nodeId: contextMenu.nodeId } })
    );
    closeContextMenu();
  }, [contextMenu, closeContextMenu]);

  if (!contextMenu) return null;

  const isOfflineOrFailed = contextMenu.nodeData.status === "offline" || contextMenu.nodeData.status === "failed";
  const isOnline = contextMenu.nodeData.status === "online";
  const isPaused = contextMenu.nodeData.status === "paused";
  const isChild = !!contextMenu.nodeData.parentId;

  const items: MenuItem[] = [
    { label: "Details", icon: "i", action: handleViewDetails },
    { label: "Chat", icon: "💬", action: handleOpenChat, disabled: !isOnline },
    { label: "Terminal", icon: ">_", action: handleOpenTerminal, disabled: !isOnline },
    { label: "", icon: "", action: () => {}, divider: true },
    { label: "Export Bundle", icon: "📦", action: handleExportBundle },
    { label: "Duplicate", icon: "⧉", action: handleDuplicate },
    ...(isChild
      ? [{ label: "Extract from Team", icon: "⤴", action: handleRemoveFromTeam }]
      : []),
    ...(hasChildren
      ? [
          { label: "Arrange Children", icon: "▦", action: handleArrangeChildren },
          {
            label: contextMenu.nodeData.collapsed ? "Expand Team" : "Collapse Team",
            icon: contextMenu.nodeData.collapsed ? "▽" : "◁",
            action: handleCollapse,
          },
          { label: "Zoom to Team", icon: "⊕", action: handleZoomToTeam },
        ]
      : []),
    { label: "", icon: "", action: () => {}, divider: true },
    ...(isPaused
      ? [{ label: "Resume", icon: "▶", action: handleResume }]
      : [{ label: "Pause", icon: "⏸", action: handlePause, disabled: !isOnline }]),
    { label: "Restart", icon: "↻", action: handleRestart, disabled: !(isOfflineOrFailed || isPaused) },
    { label: "Delete", icon: "✕", action: handleDelete, danger: true },
  ];

  return (
    <div
      ref={ref}
      role="menu"
      aria-label={`Actions for ${contextMenu.nodeData.name}`}
      onKeyDown={handleMenuKeyDown}
      className="fixed z-[60] min-w-[200px] bg-surface/95 backdrop-blur-xl border border-line/60 rounded-xl shadow-2xl shadow-black/60 py-1 overflow-hidden"
      style={{ left: clamped?.x ?? contextMenu.x, top: clamped?.y ?? contextMenu.y }}
    >
      {/* Header */}
      <div className="px-3.5 py-2 border-b border-line/40 mb-0.5">
        <div className="text-[11px] font-semibold text-ink truncate">{contextMenu.nodeData.name}</div>
        <div className="flex items-center gap-1.5 mt-0.5">
          <div
            aria-hidden="true"
            className={`w-1.5 h-1.5 rounded-full ${statusDotClass(contextMenu.nodeData.status)}`}
          />
          <span className="text-[10px] text-ink-soft">{contextMenu.nodeData.status}</span>
        </div>
      </div>

      {items.map((item, i) => {
        if (item.divider) {
          return <div key={i} role="separator" className="h-px bg-surface-card/60 my-1" />;
        }
        return (
          <button
            type="button"
            key={i}
            role="menuitem"
            onClick={item.action}
            disabled={item.disabled}
            aria-disabled={item.disabled}
            className={`w-full px-3.5 py-1.5 flex items-center gap-2.5 text-left text-[11px] transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/50 disabled:opacity-25 disabled:cursor-not-allowed ${
              item.danger
                ? "text-bad hover:bg-red-950/40 hover:text-bad"
                : "text-ink-mid hover:bg-surface-card/40 hover:text-ink"
            }`}
          >
            <span aria-hidden="true" className="w-4 text-center text-[10px] shrink-0 opacity-50">{item.icon}</span>
            {item.label}
          </button>
        );
      })}
    </div>
  );
}
