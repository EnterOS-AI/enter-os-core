/** Canonical workspace `kind` values — the TS mirror of Go's models.Kind*
 *  constants (`models.KindPlatform` / `models.KindWorkspace`).
 *
 *  Single source of truth for the `kind` magic strings used across the canvas
 *  (topology, map strip, toolbar, concierge shell). Kept in a leaf module so
 *  both `@/store/canvas` and `@/store/canvas-topology` can import it without a
 *  circular dependency. `WorkspaceNodeData.kind` stays a plain `string` — these
 *  are the well-known values to compare against, not an exhaustive enum.
 *
 *  - `Platform`  = the org-level concierge (the undeletable org root, hidden
 *                  from the map graph, surfaced as the shell's org root).
 *  - `Workspace` = an ordinary agent. Also the fallback for older ws-server
 *                  builds that predate the `kind` column. */
export const WORKSPACE_KIND = {
  Platform: "platform",
  Workspace: "workspace",
} as const;
