"use client";

import { useCallback, useMemo } from "react";
import { Handle, NodeResizer, Position, type NodeProps, type Node } from "@xyflow/react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { showToast } from "@/components/Toaster";
import { Tooltip } from "@/components/Tooltip";
import { STATUS_CONFIG, TIER_CONFIG } from "@/lib/design-tokens";
import { useOrgDeployState } from "@/components/canvas/useOrgDeployState";
import { OrgCancelButton } from "@/components/canvas/OrgCancelButton";

/** Descendant count for the "N sub" badge — children are first-class nodes
 *  rendered as full cards inside this one via React Flow's native parentId,
 *  so we don't need to subscribe to the actual child list here. */
function useDescendantCount(nodeId: string): number {
  return useCanvasStore(
    useCallback((s) => countDescendants(nodeId, s.nodes), [nodeId])
  );
}

function useHasChildren(nodeId: string): boolean {
  return useCanvasStore(
    useCallback((s) => s.nodes.some((n) => n.data.parentId === nodeId), [nodeId])
  );
}

/** Eject/extract arrow icon — visually distinct from delete ✕ */
function EjectIcon(props: React.SVGProps<SVGSVGElement>) {
  return (
    <svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" {...props}>
      <path d="M3 7L7 3" />
      <path d="M4 3H7V6" />
    </svg>
  );
}

export function WorkspaceNode({ id, data }: NodeProps<Node<WorkspaceNodeData>>) {
  const statusCfg = STATUS_CONFIG[data.status] || STATUS_CONFIG.offline;
  const tierCfg = TIER_CONFIG[data.tier] || { label: `T${data.tier}`, color: "text-ink-soft bg-surface-card" };
  // Org-deploy context — four derived flags off one store subscription.
  // Drives the shimmer while provisioning, the dimmed/non-draggable
  // treatment on locked descendants, and the Cancel pill on the root.
  const deploy = useOrgDeployState(id);
  const selectedNodeId = useCanvasStore((s) => s.selectedNodeId);
  const selectNode = useCanvasStore((s) => s.selectNode);
  const openContextMenu = useCanvasStore((s) => s.openContextMenu);
  const nestNode = useCanvasStore((s) => s.nestNode);
  const isDragTarget = useCanvasStore((s) => s.dragOverNodeId === id);
  const isSelected = selectedNodeId === id;
  // Batch selection (Phase 20.3)
  const isBatchSelected = useCanvasStore((s) => s.selectedNodeIds.has(id));
  const toggleNodeSelection = useCanvasStore((s) => s.toggleNodeSelection);
  const isOnline = data.status === "online";

  // Children are first-class RF nodes now (rendered inside this one via
  // React Flow's native parentId). We only need the count for the badge
  // and a boolean so parent cards default to a larger size.
  const hasChildren = useHasChildren(id);
  const descendantCount = useDescendantCount(id);

  const skills = getSkillNames(data.agentCard);

  return (
    <>
      {/* NodeResizer — visible only on the selected card. Lets the user
       *  drag any edge/corner to grow or shrink the workspace, which is
       *  useful on cards that contain nested child workspaces. */}
      <NodeResizer
        isVisible={isSelected}
        minWidth={hasChildren ? 360 : 210}
        minHeight={hasChildren ? 200 : 110}
        lineClassName="!border-accent/40"
        handleClassName="!w-2 !h-2 !bg-accent !border !border-blue-300"
      />
    <div
      role="button"
      tabIndex={0}
      aria-label={`${data.name} workspace — ${data.status}`}
      aria-pressed={isSelected}
      onClick={(e) => {
        e.stopPropagation();
        if (e.shiftKey) {
          toggleNodeSelection(id);
        } else {
          selectNode(isSelected ? null : id);
        }
      }}
      onDoubleClick={(e) => {
        e.stopPropagation();
        if (!hasChildren) return;
        // A collapsed parent double-click EXPANDS first (flipping the
        // collapsed flag + persisting it via the API). Once expanded,
        // subsequent double-clicks zoom-to-team so the user can see
        // the hierarchy fit in the viewport. Matches the user's ask:
        // default-collapsed for clean first paint, one gesture reveals
        // the subtree.
        if (data.collapsed) {
          const state = useCanvasStore.getState();
          state.setCollapsed(id, false);
          // Fire-and-forget persist so reload retains the expansion.
          import("@/lib/api").then(({ api }) => {
            api.patch(`/workspaces/${id}`, { collapsed: false }).catch(() => {});
          });
          return;
        }
        window.dispatchEvent(new CustomEvent("molecule:zoom-to-team", { detail: { nodeId: id } }));
      }}
      onContextMenu={(e) => {
        e.preventDefault();
        e.stopPropagation();
        openContextMenu({ x: e.clientX, y: e.clientY, nodeId: id, nodeData: data });
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          if (e.shiftKey) {
            toggleNodeSelection(id);
          } else {
            selectNode(isSelected ? null : id);
          }
        } else if (e.key === "ContextMenu") {
          e.preventDefault();
          const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
          openContextMenu({
            x: rect.left + rect.width / 2,
            y: rect.top + rect.height / 2,
            nodeId: id,
            nodeData: data,
          });
        }
      }}
      className={`
        group relative rounded-xl h-full w-full
        ${hasChildren && !data.collapsed ? "min-w-[360px] min-h-[200px]" : "min-w-[210px]"}
        cursor-pointer overflow-hidden
        transition-all duration-200 ease-out
        ${isDragTarget
          ? "bg-emerald-950/40 border-2 border-emerald-400/60 ring-2 ring-emerald-400/20 scale-[1.03]"
          : isBatchSelected
          ? "bg-surface-sunken/95 border-2 border-accent/80 ring-2 ring-accent/30 shadow-lg shadow-blue-500/15"
          : isSelected
          ? "bg-surface-sunken/95 border border-accent/70 ring-1 ring-accent/30 shadow-lg shadow-blue-500/10"
          : "bg-surface-sunken/90 border border-line/80 hover:border-zinc-500/60 shadow-lg shadow-black/30 hover:shadow-xl hover:shadow-black/40"
        }
        backdrop-blur-sm
        focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-950
        ${deploy.isActivelyProvisioning ? "mol-deploy-shimmer" : ""}
        ${deploy.isLockedChild ? "mol-deploy-locked" : ""}
      `}
    >
      {/* Cancel-deployment pill — rendered on the root of a deploying
          org only. Positioned absolute inside the card so it moves
          with drag; class="nodrag" on the button stops React Flow
          from treating clicks as a drag start. */}
      {deploy.isDeployingRoot && (
        <OrgCancelButton
          rootId={id}
          rootName={data.name}
          workspaceCount={deploy.descendantProvisioningCount}
        />
      )}
      {/* Status gradient bar at top */}
      <div className={`absolute inset-x-0 top-0 h-8 bg-gradient-to-b ${statusCfg.bar} pointer-events-none`} />

      <Handle
        type="target"
        position={Position.Top}
        className="!w-2.5 !h-1 !rounded-full !bg-surface-card/80 !border-0 !-top-0.5 hover:!bg-blue-400 hover:!h-1.5 transition-all"
      />

      <div className="relative px-3.5 py-2.5">
        {/* Header row */}
        <div className="flex items-center justify-between gap-2 mb-1">
          <div className="flex items-center gap-2 min-w-0">
            <div className={`w-2 h-2 rounded-full shrink-0 ${statusCfg.dot} ${statusCfg.glow} shadow-sm`} />
            <span className="text-[13px] font-semibold text-ink truncate leading-tight">
              {data.name}
            </span>
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            {hasChildren && (
              <span className="text-[10px] font-mono text-violet-300 bg-violet-900/40 border border-violet-700/30 px-1.5 py-0.5 rounded-md">
                {descendantCount} sub
              </span>
            )}
            <span className={`text-[10px] font-mono px-1.5 py-0.5 rounded-md ${tierCfg.color}`}>
              {tierCfg.label}
            </span>
          </div>
        </div>

        {/* Runtime badge — prefers workspace.runtime (DB column) over
            agent_card.runtime (agent-reported). Phase 30 remote agents
            (runtime='external') get a distinct purple "REMOTE" pill.
            We treat empty-string DB values as "missing" so an unbackfilled
            row falls through to the agent-card value rather than rendering
            a blank pill. */}
        {(() => {
          const dbRuntime = typeof data.runtime === "string" && data.runtime !== ""
            ? data.runtime : null;
          const cardRuntime = data.agentCard && typeof (data.agentCard as Record<string, unknown>).runtime === "string"
            ? (data.agentCard as Record<string, string>).runtime
            : null;
          const runtime = dbRuntime ?? cardRuntime;
          if (!runtime) return null;
          return (
            <div className="mb-1 flex items-center gap-1">
              {runtime === "external" ? (
                <span
                  className="text-[7px] font-mono px-1.5 py-0.5 rounded-md text-violet-200 bg-violet-900/50 border border-violet-500/40"
                  title="Phase 30 remote agent — runs outside this platform's Docker network. Lifecycle managed via heartbeat-based polling, not Docker exec."
                >
                  ★ REMOTE
                </span>
              ) : (
                <span className="text-[7px] font-mono px-1.5 py-0.5 rounded-md text-ink-mid bg-surface-card/60 border border-line/30">
                  {runtime}
                </span>
              )}
            </div>
          );
        })()}

        {/* Role — clamp to 2 lines. Without this, a verbose role
         *  description (common on org-template imports) lets the card
         *  grow arbitrarily tall, which wrecks the grid-slot layout
         *  because siblings all plan for the same CHILD_DEFAULT_HEIGHT. */}
        {data.role && (
          <div className="text-[10px] text-ink-mid mb-1.5 leading-tight line-clamp-2">{data.role}</div>
        )}

        {/* Skills */}
        {skills.length > 0 && (
          <div className="flex flex-wrap gap-1 mb-1.5">
            {skills.slice(0, 4).map((skill) => (
              <span
                key={skill}
                className={`text-[10px] px-1.5 py-0.5 rounded-md border ${
                  isOnline
                    ? "text-good/80 bg-emerald-950/30 border-emerald-800/30"
                    : "text-ink-mid bg-surface-card/60 border-line/40"
                }`}
              >
                {skill}
              </span>
            ))}
            {skills.length > 4 && (
              <span className="text-[10px] text-ink-soft self-center">
                +{skills.length - 4}
              </span>
            )}
          </div>
        )}

        {/* Children render as first-class React Flow nodes inside this
         *  card (parentId binding). No embedded TEAM MEMBERS list here —
         *  just keep visual breathing room via the min-height above. */}

        {/* Current task */}
        {data.currentTask && (
          <Tooltip text={String(data.currentTask)}>
            <div className="flex items-center gap-1.5 mt-1 bg-amber-950/20 px-2 py-1 rounded-md border border-amber-800/20 cursor-default">
              <div className="w-1.5 h-1.5 rounded-full bg-amber-400 motion-safe:animate-pulse shrink-0" />
              <span className="text-[10px] text-warm/80 truncate">{data.currentTask}</span>
            </div>
          </Tooltip>
        )}

        {/* Needs restart banner */}
        {data.needsRestart && !data.currentTask && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              useCanvasStore.getState().restartWorkspace(id).catch(() => showToast("Restart failed", "error"));
            }}
            className="flex items-center gap-1.5 mt-1 w-full bg-sky-950/30 px-2 py-1 rounded-md border border-sky-800/30 hover:bg-sky-900/40 transition-colors text-left focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:outline-none"
          >
            <span className="text-[10px]">↻</span>
            <span className="text-[10px] text-sky-300/80">Restart to apply changes</span>
          </button>
        )}

        {/* Bottom row: status / active tasks */}
        <div className="flex items-center justify-between mt-0.5">
          {data.status !== "online" ? (
            <div className={`text-[10px] uppercase tracking-widest font-medium ${
              data.status === "failed" ? "text-bad" :
              data.status === "degraded" ? "text-warm" :
              data.status === "provisioning" ? "text-sky-400" :
              "text-ink-soft"
            }`}>
              {statusCfg.label}
            </div>
          ) : <div />}

          {data.activeTasks > 0 && (
            <div className="flex items-center gap-1">
              <div className="w-1 h-1 rounded-full bg-amber-400 motion-safe:animate-pulse" />
              <span className="text-[10px] text-warm/80 tabular-nums">
                {data.activeTasks} task{data.activeTasks > 1 ? "s" : ""}
              </span>
            </div>
          )}
        </div>

        {/* Degraded error preview */}
        {data.status === "degraded" && data.lastSampleError && (
          <div
            className="text-[10px] text-warm/60 truncate mt-1 bg-amber-950/20 px-1.5 py-0.5 rounded border border-amber-800/20"
            title={data.lastSampleError}
          >
            {data.lastSampleError}
          </div>
        )}
      </div>

      <Handle
        type="source"
        position={Position.Bottom}
        className="!w-2.5 !h-1 !rounded-full !bg-surface-card/80 !border-0 !-bottom-0.5 hover:!bg-blue-400 hover:!h-1.5 transition-all"
      />
    </div>
    </>
  );
}

/** Count all descendants (children + grandchildren + ...) */
function countDescendants(nodeId: string, allNodes: Node<WorkspaceNodeData>[], visited = new Set<string>()): number {
  if (visited.has(nodeId)) return 0;
  visited.add(nodeId);
  const directChildren = allNodes.filter((n) => n.data.parentId === nodeId);
  let count = directChildren.length;
  for (const child of directChildren) {
    count += countDescendants(child.id, allNodes, visited);
  }
  return count;
}

/** Maximum nesting depth for recursive TeamMemberChip rendering — prevents
 *  infinite recursion on circular parentId references and keeps the UI readable. */
const MAX_NESTING_DEPTH = 3;

/** Recursive mini-card — mirrors parent card layout at smaller scale */
function TeamMemberChip({
  node,
  allNodes,
  depth,
  onSelect,
  onExtract,
}: {
  node: Node<WorkspaceNodeData>;
  allNodes: Node<WorkspaceNodeData>[];
  depth: number;
  onSelect: (id: string) => void;
  onExtract: (id: string) => void;
}) {
  const { data } = node;
  const statusCfg = STATUS_CONFIG[data.status] || STATUS_CONFIG.offline;
  const tierCfg = TIER_CONFIG[data.tier] || { label: `T${data.tier}`, color: "text-ink-soft bg-surface-card" };
  const isOnline = data.status === "online";
  const skills = getSkillNames(data.agentCard);

  const subChildren = useMemo(
    () => allNodes.filter((n) => n.data.parentId === node.id),
    [allNodes, node.id]
  );
  const hasSubChildren = subChildren.length > 0;
  const descendantCount = useMemo(
    () => hasSubChildren ? countDescendants(node.id, allNodes) : 0,
    [allNodes, node.id, hasSubChildren]
  );

  return (
    <div
      role="button"
      tabIndex={0}
      aria-label={`Select ${data.name}`}
      className="group/child relative rounded-lg bg-surface-card/60 hover:bg-surface-card/70 border border-line/30 hover:border-line/40 overflow-hidden transition-colors cursor-pointer focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70"
      onClick={(e) => {
        e.stopPropagation();
        onSelect(node.id);
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          e.stopPropagation();
          onSelect(node.id);
        }
      }}
      onContextMenu={(e) => {
        e.preventDefault();
        e.stopPropagation();
        useCanvasStore.getState().openContextMenu({ x: e.clientX, y: e.clientY, nodeId: node.id, nodeData: data });
      }}
    >
      {/* Status gradient bar */}
      <div className={`absolute inset-x-0 top-0 h-5 bg-gradient-to-b ${statusCfg.bar} pointer-events-none`} />

      <div className="relative px-2 py-1.5">
        {/* Header: name + badges + extract */}
        <div className="flex items-center justify-between gap-1 mb-0.5">
          <div className="flex items-center gap-1.5 min-w-0">
            <div className={`w-1.5 h-1.5 rounded-full shrink-0 ${statusCfg.dot}`} />
            <span className="text-[10px] font-semibold text-ink truncate leading-tight">
              {data.name}
            </span>
          </div>
          <div className="flex items-center gap-1 shrink-0">
            {hasSubChildren && (
              <span className="text-[7px] font-mono text-violet-300 bg-violet-900/40 border border-violet-700/30 px-1 py-0.5 rounded">
                {descendantCount}
              </span>
            )}
            <span className={`text-[7px] font-mono px-1 py-0.5 rounded ${tierCfg.color}`}>
              {tierCfg.label}
            </span>
            <button
              type="button"
              aria-label={`Extract ${data.name} from team`}
              title={`Extract ${data.name} from team`}
              onClick={(e) => {
                e.stopPropagation();
                onExtract(node.id);
              }}
              className="opacity-0 group-hover/child:opacity-100 text-ink-soft hover:text-sky-400 transition-all focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:outline-none rounded"
            >
              <EjectIcon aria-hidden="true" />
            </button>
          </div>
        </div>

        {/* Role */}
        {data.role && (
          <div className="text-[10px] text-ink-soft mb-1 leading-tight truncate">{data.role}</div>
        )}

        {/* Skills */}
        {skills.length > 0 && (
          <div className="flex flex-wrap gap-0.5 mb-1">
            {skills.slice(0, 3).map((skill) => (
              <span
                key={skill}
                className={`text-[10px] px-1 py-0.5 rounded border ${
                  isOnline
                    ? "text-good/70 bg-emerald-950/20 border-emerald-800/20"
                    : "text-ink-soft bg-surface-card/40 border-line/30"
                }`}
              >
                {skill}
              </span>
            ))}
            {skills.length > 3 && (
              <span className="text-[10px] text-ink-mid self-center">+{skills.length - 3}</span>
            )}
          </div>
        )}

        {/* Status + active tasks row */}
        <div className="flex items-center justify-between">
          {data.status !== "online" ? (
            <span className={`text-[10px] uppercase tracking-widest font-medium ${
              data.status === "failed" ? "text-bad" :
              data.status === "degraded" ? "text-warm" :
              data.status === "provisioning" ? "text-sky-400" :
              "text-ink-soft"
            }`}>
              {statusCfg.label}
            </span>
          ) : <div />}
          {data.activeTasks > 0 && (
            <div className="flex items-center gap-0.5">
              <div className="w-1 h-1 rounded-full bg-amber-400 motion-safe:animate-pulse" />
              <span className="text-[10px] text-warm tabular-nums">
                {data.activeTasks}
              </span>
            </div>
          )}
        </div>

        {/* Current task banner for sub-agents */}
        {data.currentTask && (
          <Tooltip text={String(data.currentTask)}>
            <div className="flex items-center gap-1 mt-0.5 px-1.5 py-0.5 bg-amber-950/20 rounded border border-amber-800/20 cursor-default">
              <div className="w-1 h-1 rounded-full bg-amber-400 motion-safe:animate-pulse shrink-0" />
              <span className="text-[10px] text-warm truncate">{data.currentTask}</span>
            </div>
          </Tooltip>
        )}

        {/* Recursive sub-children rendered inside this card */}
        {hasSubChildren && depth < MAX_NESTING_DEPTH && (
          <div className="mt-1.5 pt-1.5 border-t border-line/20">
            <div className="text-[10px] text-ink-mid uppercase tracking-widest mb-1">Team</div>
            <div className={subChildren.length >= 2 ? "grid grid-cols-2 gap-1" : "space-y-1"}>
              {subChildren.map((sub) => (
                <TeamMemberChip key={sub.id} node={sub} allNodes={allNodes} depth={depth + 1} onSelect={onSelect} onExtract={onExtract} />
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function getSkillNames(agentCard: Record<string, unknown> | null): string[] {
  if (!agentCard) return [];
  const skills = agentCard.skills;
  if (!Array.isArray(skills)) return [];
  return skills.map((s: Record<string, unknown>) =>
    String(s.name || s.id || "")
  ).filter(Boolean);
}
