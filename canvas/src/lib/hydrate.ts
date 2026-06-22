import { api, PLATFORM_URL } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import type { WorkspaceData } from "@/store/socket";

export interface HydrationResult {
  error: string | null;
}

/**
 * Fetches workspaces and viewport from the platform, then hydrates the canvas store.
 * Returns an error message string if the workspace fetch fails, or null on success.
 * The viewport fetch is non-fatal — if it fails, the canvas loads with default positioning.
 */
async function attemptHydration(): Promise<HydrationResult> {
  try {
    const [workspaces, viewport] = await Promise.all([
      api.get<WorkspaceData[]>("/workspaces"),
      api.get<{ x: number; y: number; zoom: number }>("/canvas/viewport").catch(() => null),
    ]);
    useCanvasStore.getState().hydrate(workspaces);
    if (viewport) {
      useCanvasStore.getState().setViewport(viewport);
    }
    return { error: null };
  } catch (err) {
    console.error("Initial hydration failed:", err);
    return {
      error: `Unable to connect to platform at ${PLATFORM_URL}. Check that the platform is running.`,
    };
  }
}

export const MAX_RETRIES = 3;
const BASE_DELAY_MS = 1000;

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Hydrates the canvas store with exponential backoff retry.
 * Calls onRetrying(attemptNumber) before each retry so the UI can update.
 * Returns { error: null } on success, or { error: message } after all retries exhausted.
 */
export async function hydrateCanvas(
  onRetrying?: (attempt: number) => void
): Promise<HydrationResult> {
  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    const result = await attemptHydration();
    if (result.error === null) {
      return result;
    }
    if (attempt < MAX_RETRIES) {
      onRetrying?.(attempt);
      await delay(BASE_DELAY_MS * Math.pow(2, attempt - 1));
    } else {
      return result;
    }
  }
  // Unreachable, but satisfies TypeScript
  return { error: "Hydration failed" };
}
