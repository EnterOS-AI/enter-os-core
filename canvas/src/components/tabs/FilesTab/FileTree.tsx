"use client";

import { useState } from "react";
import { type TreeNode, getIcon } from "./tree";
import { FileTreeContextMenu, type MenuItem } from "./FileTreeContextMenu";

interface TreeCallbacks {
  selectedPath: string | null;
  onSelect: (path: string) => void;
  onDelete: (path: string) => void;
  /** PR-C: right-click → Download. Files only — directories ignore. */
  onDownload: (path: string) => void;
  /** Whether the active root permits delete. Wire into the Delete
   *  context-menu item's `disabled` flag so the user gets the same
   *  affordance as the toolbar (which gates Clear/New on /configs). */
  canDelete: boolean;
  /** PR-D: drop files/folders from the OS onto this row. targetDir
   *  is the directory path (relative to the active root) under which
   *  the dropped contents should land; "" means root. */
  onDropToTarget?: (targetDir: string, items: DataTransferItemList) => void;
  expandedDirs: Set<string>;
  onToggleDir: (path: string) => void;
  loadingDir: string | null;
}

/**
 * FileTree renders the workspace tree + owns the right-click context
 * menu (PR-C) and the drop-target hover state (PR-D). Lifting the
 * menu state here (vs each row) means only one menu open at a time —
 * opening a new row's menu auto-closes the prior one. Same UX as
 * VSCode / Theia.
 */
export function FileTree({
  nodes,
  selectedPath,
  onSelect,
  onDelete,
  onDownload,
  canDelete,
  onDropToTarget,
  expandedDirs,
  onToggleDir,
  loadingDir,
  depth = 0,
}: TreeCallbacks & { nodes: TreeNode[]; depth?: number }) {
  const [menu, setMenu] = useState<{
    x: number;
    y: number;
    items: MenuItem[];
  } | null>(null);
  // PR-D: hover-target highlight state for drag-drop. Lifted next to
  // the menu state so both shared-across-rows interactions live in
  // one place.
  const [hoverDir, setHoverDir] = useState<string | null>(null);

  const openContextMenu = (e: React.MouseEvent, node: TreeNode) => {
    e.preventDefault();
    // Items composed per-row so the available actions reflect the
    // node type (files get Open + Download; directories get Delete
    // only since "open a directory in the editor" doesn't apply
    // and "Export folder" is the toolbar's job).
    const items: MenuItem[] = [];
    if (!node.isDir) {
      items.push({
        id: "open",
        label: "Open",
        icon: "⤴",
        onClick: () => onSelect(node.path),
      });
      items.push({
        id: "download",
        label: "Download",
        icon: "↓",
        onClick: () => onDownload(node.path),
      });
    }
    items.push({
      id: "delete",
      label: "Delete",
      icon: "✕",
      destructive: true,
      disabled: !canDelete,
      onClick: () => onDelete(node.path),
    });
    setMenu({ x: e.clientX, y: e.clientY, items });
  };

  // Single state lifted to the top-level tree; nested <FileTree>s
  // (rendered for expanded directories below) do NOT instantiate
  // their own menus or drop-targets — they call back via prop
  // drilling. This keeps "only one menu open" + "only one drop
  // target highlighted" as structural invariants rather than
  // render-order coincidences.
  const childCallbacks: TreeCallbacks = {
    selectedPath,
    onSelect,
    onDelete,
    onDownload,
    canDelete,
    onDropToTarget,
    expandedDirs,
    onToggleDir,
    loadingDir,
  };

  return (
    <div>
      {nodes.map((node) => (
        <TreeItem
          key={`${node.path}:${node.isDir ? "dir" : "file"}`}
          node={node}
          openContextMenu={openContextMenu}
          hoverDir={hoverDir}
          setHoverDir={setHoverDir}
          depth={depth}
          {...childCallbacks}
        />
      ))}
      {menu && (
        <FileTreeContextMenu
          x={menu.x}
          y={menu.y}
          items={menu.items}
          onClose={() => setMenu(null)}
        />
      )}
    </div>
  );
}

function TreeItem({
  node,
  selectedPath,
  onSelect,
  onDelete,
  onDownload,
  canDelete,
  onDropToTarget,
  expandedDirs,
  onToggleDir,
  loadingDir,
  depth,
  openContextMenu,
  hoverDir,
  setHoverDir,
}: TreeCallbacks & {
  node: TreeNode;
  depth: number;
  openContextMenu: (e: React.MouseEvent, node: TreeNode) => void;
  hoverDir: string | null;
  setHoverDir: (p: string | null) => void;
}) {
  const isSelected = selectedPath === node.path;
  const expanded = expandedDirs.has(node.path);
  const isLoading = loadingDir === node.path;
  const isDropTarget = node.isDir && hoverDir === node.path;

  // PR-D drag handlers — only directory rows are valid drop targets
  // (dropping a file ON another file is ambiguous; treat it as
  // dropping in the parent dir, which the root area handles). When a
  // drag enters a directory row, mark it the hover target. When the
  // cursor leaves to a non-child element, clear it. drop fires the
  // upload callback with the row's path.
  const dragProps = node.isDir && onDropToTarget
    ? {
        onDragOver: (e: React.DragEvent) => {
          // preventDefault is REQUIRED to opt this element into the
          // drop target list — without it, browsers refuse to fire
          // the drop event regardless of the drop handler.
          e.preventDefault();
          e.dataTransfer.dropEffect = "copy";
        },
        onDragEnter: (e: React.DragEvent) => {
          e.preventDefault();
          setHoverDir(node.path);
        },
        onDragLeave: (e: React.DragEvent) => {
          // Only clear hover when leaving to an element OUTSIDE this
          // row — bare leave-events fire for every child crossed
          // (the icon, the label, the ✕ button). Without the
          // contains() check the highlight flickers.
          const next = e.relatedTarget as Node | null;
          if (!next || !(e.currentTarget as HTMLElement).contains(next)) {
            setHoverDir(null);
          }
        },
        onDrop: (e: React.DragEvent) => {
          e.preventDefault();
          e.stopPropagation();
          setHoverDir(null);
          if (e.dataTransfer.items?.length) {
            onDropToTarget(node.path, e.dataTransfer.items);
          }
        },
      }
    : {};

  if (node.isDir) {
    return (
      <div>
        <div
          className={`group w-full flex items-center gap-1 px-2 py-0.5 text-left transition-colors cursor-pointer ${
            isDropTarget
              ? "bg-accent/20 outline outline-1 outline-accent/60"
              : "hover:bg-surface-card/40"
          }`}
          style={{ paddingLeft: `${depth * 12 + 8}px` }}
          onClick={() => onToggleDir(node.path)}
          onContextMenu={(e) => openContextMenu(e, node)}
          {...dragProps}
        >
          <span className="text-[9px] text-ink-soft w-3">{isLoading ? "…" : expanded ? "▼" : "▶"}</span>
          <span className="text-[10px]">📁</span>
          <span className="text-[10px] text-ink-mid flex-1">{node.name}</span>
          <button
            aria-label={`Delete ${node.name}`}
            onClick={(e) => {
              e.stopPropagation();
              onDelete(node.path);
            }}
            className="text-[9px] text-bad/0 group-hover:text-bad/60 hover:!text-bad transition-colors"
          >
            ✕
          </button>
        </div>
        {expanded && (
          <FileTree
            nodes={node.children}
            selectedPath={selectedPath}
            onSelect={onSelect}
            onDelete={onDelete}
            onDownload={onDownload}
            canDelete={canDelete}
            onDropToTarget={onDropToTarget}
            expandedDirs={expandedDirs}
            onToggleDir={onToggleDir}
            loadingDir={loadingDir}
            depth={depth + 1}
          />
        )}
      </div>
    );
  }

  return (
    <div
      className={`group flex items-center gap-1 px-2 py-0.5 cursor-pointer transition-colors ${
        isSelected ? "bg-blue-900/30 text-ink" : "hover:bg-surface-card/40 text-ink-mid"
      }`}
      style={{ paddingLeft: `${depth * 12 + 20}px` }}
      onClick={() => onSelect(node.path)}
      onContextMenu={(e) => openContextMenu(e, node)}
    >
      <span className="text-[9px]">{getIcon(node.name, false)}</span>
      <span className="text-[10px] flex-1 truncate font-mono">{node.name}</span>
      <button
        aria-label={`Delete ${node.name}`}
        onClick={(e) => {
          e.stopPropagation();
          onDelete(node.path);
        }}
        className="text-[9px] text-bad/0 group-hover:text-bad/60 hover:!text-bad transition-colors"
      >
        ✕
      </button>
    </div>
  );
}
