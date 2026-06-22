import { useCanvasStore } from "@/store/canvas";

/**
 * Mark every workspace as needing restart so the toolbar's "Restart Pending"
 * button appears. Use this after a global secret or any platform-wide config
 * change that requires containers to be restarted to pick up new env vars.
 *
 * For workspace-scoped changes, use `useCanvasStore.getState().updateNodeData(id, { needsRestart: true })` instead.
 */
export function markAllWorkspacesNeedRestart(): void {
  const canvas = useCanvasStore.getState();
  for (const node of canvas.nodes) {
    canvas.updateNodeData(node.id, { needsRestart: true });
  }
}

/**
 * Mark a single workspace as needing restart.
 */
export function markWorkspaceNeedsRestart(workspaceId: string): void {
  useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: true });
}
