import { useCallback } from "react";
import { useCanvasStore } from "@/store/canvas";

/** Resolve workspace ID to human-readable name */
export function useWorkspaceName() {
  const nodes = useCanvasStore((s) => s.nodes);
  return useCallback(
    (id: string | null) => {
      if (!id) return "";
      const node = nodes.find((n) => n.id === id);
      const name = (node?.data as Record<string, unknown>)?.name as string;
      return name || id.slice(0, 8);
    },
    [nodes]
  );
}
