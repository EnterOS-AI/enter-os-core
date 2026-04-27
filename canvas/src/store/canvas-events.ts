import type { Node, Edge } from "@xyflow/react";
import type { WSMessage } from "./socket";
import type { WorkspaceNodeData } from "./canvas";
import { extractResponseText, extractFilesFromTask } from "@/components/tabs/chat/message-parser";

// ---------------------------------------------------------------------------
// Monotonically increasing counter used to assign grid positions.
//
// WHY NOT nodes.length?
// Using `nodes.length` as the placement index breaks after any deletion:
// handleCanvasEvent(WORKSPACE_REMOVED) shrinks the array, so the next
// provisioned node reuses a lower index and collides in space with an
// existing node.
//
//   Example (4-col grid, COL_SPACING=320):
//   Provision A → idx 0 → (100, 100)
//   Provision B → idx 1 → (420, 100)
//   Provision C → idx 2 → (740, 100)
//   Remove    A → nodes.length drops to 2
//   Provision D → idx 2 → (740, 100)  ← exact collision with C 🚨
//
// A monotonic counter is immune to deletions: it only ever increases.
// ---------------------------------------------------------------------------
import { appendClass, removeClass, scheduleNodeClassRemoval } from "./classNames";

let _provisioningSequence = 0;

/** Reset the sequence counter — exposed for test teardown only. */
export function resetProvisioningSequence(): void {
  _provisioningSequence = 0;
  _pendingOnline.clear();
}

/** WORKSPACE_ONLINE events that arrived BEFORE the matching
 *  WORKSPACE_PROVISIONING — buffered here so the late-arriving
 *  provision event can immediately flip to the correct status
 *  instead of leaving the node stuck as "provisioning" forever.
 *  Cleared when applied, or on module reset (tests). */
const _pendingOnline = new Set<string>();

/** Debounced parent-grow. Each child arrival schedules this; the
 *  timer keeps resetting as more siblings land, so the actual
 *  width/height update runs ONCE after arrivals go quiet. Avoids
 *  the visible size-pulse that happened when growParentsToFitChildren
 *  ran per event. */
let _growTimer: ReturnType<typeof setTimeout> | null = null;
function scheduleParentGrow(): void {
  if (typeof window === "undefined") return;
  if (_growTimer) clearTimeout(_growTimer);
  _growTimer = setTimeout(() => {
    _growTimer = null;
    import("./canvas").then(({ useCanvasStore }) => {
      useCanvasStore.getState().growParentsToFitChildren?.();
    });
  }, 300);
}

// (absoluteNodePosition was used by an earlier "spawn from parent"
// revision that subtracted parent absolute coords from server-sent
// absolute child coords. The server now ships parent-relative coords
// directly, so the walk is no longer needed. Deleted rather than
// kept as dead code.)

/**
 * Standalone event handler extracted from the canvas store.
 * Applies a single WebSocket event to the current node/edge state.
 */
export function handleCanvasEvent(
  msg: WSMessage,
  get: () => {
    nodes: Node<WorkspaceNodeData>[];
    edges: Edge[];
    selectedNodeId: string | null;
    agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string; attachments?: Array<{ name: string; uri: string; mimeType?: string; size?: number }> }>>;
  },
  set: (partial: Record<string, unknown>) => void,
): void {
  const { nodes, edges, selectedNodeId } = get();

  switch (msg.event) {
    case "WORKSPACE_ONLINE": {
      const existing = nodes.find((n) => n.id === msg.workspace_id);
      if (!existing) {
        // PROVISIONING event hasn't been applied yet (WS reorder or
        // this tab joined mid-deploy). Buffer so the later PROVISIONING
        // handler can flip status in one pass instead of leaving the
        // node stuck in "provisioning" forever.
        _pendingOnline.add(msg.workspace_id);
        break;
      }
      // Flip incoming edge from blueprint → laser so the link is
      // drawn solid the moment this child is live. The laser class
      // plays the stroke-dashoffset keyframe once; after ~500ms the
      // edge falls back to the default solid style (see
      // org-deploy.css and the follow-up setTimeout below).
      const updatedEdges = edges.map((e) =>
        e.target === msg.workspace_id && e.className?.includes("mol-deploy-edge-blueprint")
          ? { ...e, className: "mol-deploy-edge-laser" }
          : e,
      );
      set({
        edges: updatedEdges,
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? { ...n, data: { ...n.data, status: "online" } }
            : n,
        ),
      });
      // Remove the laser class after its keyframe ends so the edge
      // settles into the app's default solid styling. Fire-and-forget.
      if (typeof window !== "undefined") {
        const targetEdgeId = `${existing.data.parentId ?? ""}-${msg.workspace_id}`;
        window.setTimeout(() => {
          const s = get();
          set({
            edges: s.edges.map((e) =>
              e.id === targetEdgeId ? { ...e, className: undefined } : e,
            ),
          });
        }, 600);
      }
      break;
    }

    case "WORKSPACE_OFFLINE": {
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? { ...n, data: { ...n.data, status: "offline" } }
            : n
        ),
      });
      break;
    }

    case "WORKSPACE_PAUSED": {
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? { ...n, data: { ...n.data, status: "paused", currentTask: "" } }
            : n
        ),
      });
      break;
    }

    case "WORKSPACE_DEGRADED": {
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? {
                ...n,
                data: {
                  ...n.data,
                  status: "degraded",
                  lastErrorRate: (msg.payload.error_rate as number) ?? 0,
                  lastSampleError:
                    (msg.payload.sample_error as string) ?? "",
                },
              }
            : n
        ),
      });
      break;
    }

    case "WORKSPACE_PROVISIONING": {
      const exists = nodes.find((n) => n.id === msg.workspace_id);
      if (exists) {
        // Restart — update existing node to provisioning
        set({
          nodes: nodes.map((n) =>
            n.id === msg.workspace_id
              ? { ...n, data: { ...n.data, status: "provisioning", needsRestart: false, currentTask: "" } }
              : n
          ),
        });
      } else {
        // Payload may carry parent_id + final x/y (org import broadcasts
        // these so the canvas can animate the "spawn from parent" motion).
        // Standalone workspace creates still omit them — fall back to the
        // grid-slot behaviour that handled that case historically.
        const parentIdRaw = (msg.payload.parent_id as string | undefined) ?? null;
        const finalX = msg.payload.x as number | undefined;
        const finalY = msg.payload.y as number | undefined;

        let spawnX: number;
        let spawnY: number;
        let targetX: number;
        let targetY: number;
        let parentId: string | null = null;

        // Place the node at its final slot immediately — no
        // spring-from-parent motion. The earlier "materialize from
        // parent then tween to target" was expensive (two set()
        // calls + rAF) and produced wrong offsets because the
        // server sends absolute coords computed against the template's
        // own coord system while the client had placed the parent at
        // a grid slot, so the target math always landed off-grid.
        // Now: server coords are parent-relative (see org_import.go),
        // we trust them verbatim.
        const parentInStore = parentIdRaw
          ? nodes.find((n) => n.id === parentIdRaw)
          : undefined;
        if (parentIdRaw && parentInStore && finalX !== undefined && finalY !== undefined) {
          targetX = finalX;
          targetY = finalY;
          parentId = parentIdRaw;
        } else {
          // Standalone create OR org-child whose parent hasn't arrived
          // yet (rare WS reorder) — monotonic-grid placement. The
          // follow-up hydrate pass reconciles parent_id + the correct
          // nested position if parent lands later.
          const GRID_COLS = 4;
          const COL_SPACING = 320;
          const ROW_SPACING = 160;
          const GRID_ORIGIN_X = 100;
          const GRID_ORIGIN_Y = 100;
          const idx = _provisioningSequence++;
          targetX = GRID_ORIGIN_X + (idx % GRID_COLS) * COL_SPACING;
          targetY = GRID_ORIGIN_Y + Math.floor(idx / GRID_COLS) * ROW_SPACING;
        }
        spawnX = targetX;
        spawnY = targetY;

        // Parent→child relationship is already visible via React
        // Flow's nested rendering (the child card sits INSIDE the
        // parent container). An explicit edge on top of that was
        // visual double-counting and made the canvas look busy;
        // removed per demo feedback. A2A edges (showA2AEdges) still
        // render when enabled — those represent runtime traffic,
        // which nesting doesn't express.
        set({
          nodes: [
            ...nodes,
            {
              id: msg.workspace_id,
              type: "workspaceNode",
              position: { x: spawnX, y: spawnY },
              // React Flow's parentId (distinct from data.parentId)
              // triggers parent-relative positioning. Set it when the
              // server told us this is an org-import child so the
              // node renders nested inside the parent container.
              ...(parentId ? { parentId } : {}),
              className: "mol-deploy-spawn",
              data: {
                name: (msg.payload.name as string) ?? "New Workspace",
                status: "provisioning",
                tier: (msg.payload.tier as number) ?? 1,
                agentCard: null,
                activeTasks: 0,
                collapsed: false,
                role: "",
                lastErrorRate: 0,
                lastSampleError: "",
                url: "",
                parentId, // data.parentId mirrors React Flow's parentId
                currentTask: "",
                runtime: (msg.payload.runtime as string) ?? "",
                needsRestart: false,
              },
            },
          ],
        });

        // Grow the parent to fit the just-landed child. DEBOUNCED
        // across rapid sibling arrivals — firing width/height updates
        // on every child made the parent card visibly pulse in size
        // as each kid landed, which read as the parent "flashing
        // around". One grow pass ~300ms after the last arrival
        // coalesces the whole burst into a single layout change.
        if (parentId && typeof window !== "undefined") {
          scheduleParentGrow();
        }
        // Parent-border pulse removed per demo feedback — the soft
        // box-shadow ring on each arrival compounded with the size
        // grow to make the whole parent card look unstable. The
        // dim-light signal on the provisioning child is sufficient
        // acknowledgement that something is happening.

        // Remove the one-shot spawn class after the keyframe ends so
        // future re-renders don't replay it.
        scheduleNodeClassRemoval(msg.workspace_id, "mol-deploy-spawn", 400, get, set);

        // Auto-pan+zoom to the whole deploying org after each
        // arrival so the user always sees the full picture — unless
        // they've panned themselves (handled by the viewport hook,
        // which aborts the fit when the user moved after the last
        // auto-fit). Event name matches the existing handler in
        // useCanvasViewport that knows how to compute subtree bounds.
        //
        // Fire for roots too (not just children) so the canvas
        // centers on the just-landed root immediately instead of
        // waiting for the first child to arrive ~2s later. The
        // viewport hook walks UP to find the true root, so passing
        // the node's own id when there's no parent is equivalent
        // to passing the root.
        if (typeof window !== "undefined") {
          window.dispatchEvent(
            new CustomEvent("molecule:fit-deploying-org", {
              detail: { rootId: parentIdRaw ?? msg.workspace_id },
            }),
          );
        }

        // Race handling: if a WORKSPACE_ONLINE event beat the
        // matching PROVISIONING to this tab, the online flag was
        // buffered in _pendingOnline. Apply it now so the node
        // doesn't stay stuck as "provisioning" forever.
        //
        // Only flip to "online" if the current status is still
        // "provisioning" at drain time. Otherwise a WORKSPACE_DEGRADED
        // / FAILED / PAUSED that arrived between the set() above and
        // the scheduled drain would be silently clobbered — the
        // buffered ONLINE is stale by then.
        if (_pendingOnline.has(msg.workspace_id)) {
          _pendingOnline.delete(msg.workspace_id);
          if (typeof window !== "undefined") {
            window.setTimeout(() => {
              const s = get();
              set({
                nodes: s.nodes.map((n) =>
                  n.id === msg.workspace_id && n.data.status === "provisioning"
                    ? { ...n, data: { ...n.data, status: "online" } }
                    : n,
                ),
              });
            }, 0);
          }
        }

        // Pan the canvas to the new node (standalone create only —
        // during an org import, zooming to every child chases the
        // spawn animation around the viewport which is jarring).
        if (!parentIdRaw && typeof window !== "undefined") {
          window.dispatchEvent(
            new CustomEvent("molecule:pan-to-node", {
              detail: { nodeId: msg.workspace_id },
            })
          );
        }
      }
      break;
    }

    case "WORKSPACE_REMOVED": {
      const removedNode = nodes.find((n) => n.id === msg.workspace_id);
      const parentOfRemoved = removedNode?.data.parentId ?? null;
      set({
        nodes: nodes
          .filter((n) => n.id !== msg.workspace_id)
          .map((n) =>
            n.data.parentId === msg.workspace_id
              ? {
                  ...n,
                  parentId: parentOfRemoved ?? undefined,
                  data: { ...n.data, parentId: parentOfRemoved },
                }
              : n
          ),
        edges: edges.filter(
          (e) =>
            e.source !== msg.workspace_id && e.target !== msg.workspace_id
        ),
        selectedNodeId: selectedNodeId === msg.workspace_id ? null : selectedNodeId,
      });
      break;
    }

    case "AGENT_CARD_UPDATED": {
      const card = msg.payload.agent_card;
      const agentCard = (typeof card === "object" && card !== null ? card : null) as Record<string, unknown> | null;
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? { ...n, data: { ...n.data, agentCard } }
            : n
        ),
      });
      break;
    }

    case "TASK_UPDATED": {
      const currentTask = (msg.payload.current_task as string) ?? "";
      const activeTasks = (msg.payload.active_tasks as number) ?? 0;
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? { ...n, data: { ...n.data, currentTask, activeTasks } }
            : n
        ),
      });
      break;
    }

    case "AGENT_MESSAGE": {
      const content = (msg.payload.message as string) ?? "";
      // Attachments come straight through from the platform's Notify
      // handler when the agent's tool_send_message_to_user passes file
      // refs. Shape mirrors NotifyAttachment in activity.go and matches
      // ChatTab's createMessage(role, content, attachments) signature
      // exactly, so no adapter needed downstream.
      const rawAttachments = msg.payload.attachments;
      const attachments = Array.isArray(rawAttachments)
        ? (rawAttachments as Array<{ uri?: unknown; name?: unknown; mimeType?: unknown; size?: unknown }>)
            // Reject empty strings as well as non-strings — server-side
            // gin validation does NOT enforce binding:"required" on
            // slice-element struct fields without `dive` (which the
            // notify handler does not use), so a malformed broadcast
            // could carry uri:"" or name:"". Defence-in-depth: drop
            // those here so the chat doesn't render a blank/broken chip.
            .filter((a) =>
              typeof a?.uri === "string" && a.uri.length > 0 &&
              typeof a?.name === "string" && a.name.length > 0,
            )
            .map((a) => ({
              uri: a.uri as string,
              name: a.name as string,
              mimeType: typeof a.mimeType === "string" ? a.mimeType : undefined,
              size: typeof a.size === "number" ? a.size : undefined,
            }))
        : undefined;
      // Skip when both content and attachments are empty — pure-noise
      // event we don't want to render as a blank bubble.
      if (content || (attachments && attachments.length > 0)) {
        const { agentMessages } = get();
        const existing = agentMessages[msg.workspace_id] || [];
        set({
          agentMessages: {
            ...agentMessages,
            [msg.workspace_id]: [
              ...existing,
              {
                id: crypto.randomUUID(),
                content,
                timestamp: new Date().toISOString(),
                ...(attachments && attachments.length > 0 ? { attachments } : {}),
              },
            ],
          },
        });
      }
      break;
    }

    case "WORKSPACE_PROVISION_FAILED": {
      const errorMsg = (msg.payload.error as string) ?? "Unknown provisioning error";
      set({
        nodes: nodes.map((n) =>
          n.id === msg.workspace_id
            ? {
                ...n,
                data: {
                  ...n.data,
                  status: "failed",
                  lastSampleError: errorMsg,
                },
              }
            : n
        ),
      });
      break;
    }

    case "A2A_RESPONSE": {
      // A2A proxy completed — extract response text AND any `kind: file`
      // parts. Without the file extraction, agent-returned attachments
      // delivered via this WebSocket path would disappear (the canvas
      // would render a text-only message while the HTTP fallback
      // rendered the same reply with download chips, depending on
      // which delivery path raced to completion first).
      const responseBody = msg.payload.response_body as Record<string, unknown> | undefined;
      if (responseBody) {
        const text = extractResponseText(responseBody);
        const attachments = extractFilesFromTask(
          (responseBody.result ?? responseBody) as Record<string, unknown>,
        );
        if (text || attachments.length > 0) {
          const { agentMessages } = get();
          const existing = agentMessages[msg.workspace_id] || [];
          set({
            agentMessages: {
              ...agentMessages,
              [msg.workspace_id]: [
                ...existing,
                {
                  id: crypto.randomUUID(),
                  content: text,
                  timestamp: new Date().toISOString(),
                  attachments: attachments.length > 0 ? attachments : undefined,
                },
              ],
            },
          });
        }
      }
      break;
    }

    default:
      break;
  }
}
