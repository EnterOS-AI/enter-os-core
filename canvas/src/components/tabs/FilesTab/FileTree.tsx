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
  expandedDirs: Set<string>;
  onToggleDir: (path: string) => void;
  loadingDir: string | null;
}

/**
 * FileTree renders the workspace tree + owns the right-click
 * context-menu state. Lifting the menu state to the tree (vs each
 * row) means only one menu is open at a time — opening a new row's
 * menu auto-closes the prior one. Same UX as VSCode / Theia.
 */
export function FileTree({
  nodes,
  selectedPath,
  onSelect,
  onDelete,
  onDownload,
  canDelete,
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

  const openContextMenu = (e: React.MouseEvent, node: TreeNode) => {
    e.preventDefault();
    // Items composed per-row so the available actions reflect the
    // node type (files get Download; directories don't have a
    // useful per-tree download — the Export toolbar covers bulk).
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
  // their own menus — they call the SAME openContextMenu via prop
  // drilling. This keeps "only one menu open" the structural
  // invariant rather than a render-order coincidence.
  const childCallbacks: TreeCallbacks = {
    selectedPath, onSelect, onDelete, onDownload, canDelete,
    expandedDirs, onToggleDir, loadingDir,
  };

  return (
    <div>
      {nodes.map((node) => (
        <TreeItem
          key={`${node.path}:${node.isDir ? "dir" : "file"}`}
          node={node}
          openContextMenu={openContextMenu}
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
  expandedDirs,
  onToggleDir,
  loadingDir,
  depth,
  openContextMenu,
}: TreeCallbacks & {
  node: TreeNode;
  depth: number;
  openContextMenu: (e: React.MouseEvent, node: TreeNode) => void;
}) {
  const isSelected = selectedPath === node.path;
  const expanded = expandedDirs.has(node.path);
  const isLoading = loadingDir === node.path;

  if (node.isDir) {
    return (
      <div>
        <div
          className="group w-full flex items-center gap-1 px-2 py-0.5 text-left hover:bg-surface-card/40 transition-colors cursor-pointer"
          style={{ paddingLeft: `${depth * 12 + 8}px` }}
          onClick={() => onToggleDir(node.path)}
          onContextMenu={(e) => openContextMenu(e, node)}
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
