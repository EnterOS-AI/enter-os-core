"use client";

import { type TreeNode, getIcon } from "./tree";

interface TreeCallbacks {
  selectedPath: string | null;
  onSelect: (path: string) => void;
  onDelete: (path: string) => void;
  expandedDirs: Set<string>;
  onToggleDir: (path: string) => void;
  loadingDir: string | null;
}

export function FileTree({
  nodes,
  selectedPath,
  onSelect,
  onDelete,
  expandedDirs,
  onToggleDir,
  loadingDir,
  depth = 0,
}: TreeCallbacks & { nodes: TreeNode[]; depth?: number }) {
  return (
    <div>
      {nodes.map((node) => (
        <TreeItem
          key={`${node.path}:${node.isDir ? "dir" : "file"}`}
          node={node}
          selectedPath={selectedPath}
          onSelect={onSelect}
          onDelete={onDelete}
          expandedDirs={expandedDirs}
          onToggleDir={onToggleDir}
          loadingDir={loadingDir}
          depth={depth}
        />
      ))}
    </div>
  );
}

function TreeItem({
  node,
  selectedPath,
  onSelect,
  onDelete,
  expandedDirs,
  onToggleDir,
  loadingDir,
  depth,
}: TreeCallbacks & { node: TreeNode; depth: number }) {
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
