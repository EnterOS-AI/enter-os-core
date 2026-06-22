"use client";

import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";

/** Resolve a workspace ID to its human-readable name.
 *  Falls back to the first 8 chars of the ID. */
export function resolveWorkspaceName(id: string): string {
  const nodes = useCanvasStore.getState().nodes;
  const node = nodes.find((n) => n.id === id);
  return (node?.data as WorkspaceNodeData)?.name || id.slice(0, 8);
}
