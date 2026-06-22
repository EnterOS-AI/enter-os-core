"use client";

import { useMemo, type KeyboardEvent } from "react";
import { Handle, Position, type NodeProps, type Node } from "@xyflow/react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { getConfigurationError, getConfigurationStatus } from "@/store/canvas-topology";
import { showToast } from "@/components/Toaster";
import { Tooltip } from "@/components/Tooltip";
import { STATUS_CONFIG, TIER_CONFIG } from "@/lib/design-tokens";
import { useOrgDeployState } from "@/components/canvas/useOrgDeployState";
import { OrgCancelButton } from "@/components/canvas/OrgCancelButton";
import { isExternalLikeRuntime } from "@/lib/externalRuntimes";

/** Descendant count for the "N sub" badge — children are first-class nodes
 *  rendered as full cards inside this one via React Flow's native parentId,
 *  so we don't need to subscribe to the actual child list here.
 *  Selecting `nodes` stably avoids a new selector reference on every store
 *  update (React error #185 / Zustand + React 19 Object.is strictness). */
function useDescendantCount(nodeId: string): number {
  const nodes = useCanvasStore((s) => s.nodes);
  return useMemo(() => countDescendants(nodeId, nodes), [nodeId, nodes]);
}

/** Boolean flag used to drive the container's system-controlled size
 *  (leaves render fixed-size; parents grow to fit children).
 *  Selecting `nodes` stably avoids re-render loops (same issue as
 *  useDescendantCount). */
function useHasChildren(nodeId: string): boolean {
  const nodes = useCanvasStore((s) => s.nodes);
  return useMemo(() => nodes.some((n) => n.data.parentId === nodeId), [nodes, nodeId]);
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
  // Configuration-status overlay (PR #2756 / #467 chain). When the
  // workspace is reachable but adapter.setup() failed (typically a
  // missing/rotated LLM credential), the agent_card carries
  // configuration_status: "not_configured". Surface this as a distinct
  // tile state so the operator sees a useful error instead of an
  // ambiguous "online but silent" workspace.
  //
  // The override only applies when the underlying status is "online" —
  // a workspace that's actually offline / failed / provisioning gets
  // its own treatment. "online + not_configured" is the gap PR #2756
  // introduced; everything else was already covered.
  const isMisconfigured =
    data.status === "online" &&
    getConfigurationStatus(data.agentCard) === "not_configured";
  const configurationError = getConfigurationError(data.agentCard);
  const effectiveStatus = isMisconfigured ? "not_configured" : data.status;
  const statusCfg = STATUS_CONFIG[effectiveStatus] || STATUS_CONFIG.offline;
  const tierCfg = TIER_CONFIG[data.tier] || { label: `T${data.tier}`, color: "text-ink-mid bg-surface-card border border-line" };
  const tooltipExtra = isMisconfigured && configurationError
    ? `Agent not configured: ${configurationError}`
    : null;
  void tooltipExtra; // wired in via aria-label below; reserved here for future tooltip surface.
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
      {/* Free-resize removed (was NodeResizer). Container size + shape are now
       *  system-controlled: leaf workspaces render at a fixed width; parent
       *  workspaces grow to fit their nested children (store grow logic). */}
    <div
      role="button"
      tabIndex={0}
      data-testid={`workspace-node-${data.name}`}
      // core#2721: E2E staging-tabs.spec.ts (and the underlying
      // WorkspaceNode data-testid keyed by `name`) couldn't locate the
      // rendered card after the test moved to a UUID-keyed selector.
      // `data-testid` collides on name (e.g. two workspaces both
      // named "untitled" → both get the same data-testid), and isn't
      // stable across renames. `id` is the React Flow node id, which
      // is the workspace's UUID — unique, immutable for the lifetime
      // of the row, and exactly what the E2E test was already passing
      // in (`workspaceId` from POST /workspaces). Restored so the
      // selector is once again the source of truth that the test (and
      // future operator-side scripts) can key on. Test for the
      // presence: canvas/src/components/__tests__/WorkspaceNode.test.tsx
      // (TestWorkspaceNode_HasDataWorkspaceIdAttribute).
      data-workspace-id={id}
      aria-label={
        isMisconfigured && configurationError
          ? `${data.name} workspace — agent not configured: ${configurationError}`
          : `${data.name} workspace — ${data.status}`
      }
      title={isMisconfigured && configurationError ? `Agent not configured: ${configurationError}` : undefined}
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
        group relative rounded-xl
        ${hasChildren && !data.collapsed
          ? "h-full w-full min-w-[420px] min-h-[240px]"
          : "w-[300px] min-h-[176px]"}
        cursor-pointer overflow-hidden
        transition-all duration-200 ease-out
        ${isDragTarget
          ? "bg-emerald-950/40 border-2 border-emerald-400/60 ring-2 ring-emerald-400/20 scale-[1.03]"
          : isBatchSelected
          ? "bg-surface-sunken/95 border-2 border-accent/80 ring-2 ring-accent/30 shadow-lg shadow-accent/15"
          : isSelected
          ? "bg-surface-sunken/95 border border-accent/70 ring-1 ring-accent/30 shadow-lg shadow-accent/10"
          : "bg-surface-sunken/90 border border-line/80 hover:border-ink-soft/60 shadow-lg shadow-black/30 hover:shadow-xl hover:shadow-black/40"
        }
        backdrop-blur-sm
        focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:ring-offset-1 focus-visible:ring-offset-surface
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
        tabIndex={0}
        role="button"
        aria-label={`Extract ${data.name} from its parent (Enter or Space)`}
        onKeyDown={(e: KeyboardEvent<HTMLDivElement>) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            e.stopPropagation();
            // Keyboard accessibility for edge anchors: pressing Enter/Space on
            // the top handle extracts this node from its current parent,
            // moving it to the root level. Mirrors the Figma/Excalidraw
            // pattern of using the connector dot as a keyboard affordance.
            if (data.parentId) {
              void nestNode(id, null);
            }
          }
        }}
        className="!w-2.5 !h-1 !rounded-full !bg-surface-card/80 !border-0 !-top-0.5 hover:!bg-accent hover:!h-1.5 focus-visible:!bg-accent focus-visible:!h-1.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface transition-all"
      />

      <div className="relative px-4 py-3.5">
        {/* Header row */}
        <div className="flex items-center justify-between gap-2 mb-2.5">
          <div className="flex items-center gap-2.5 min-w-0">
            <div data-flight-anchor className={`w-2.5 h-2.5 rounded-full shrink-0 ${statusCfg.dot} ${statusCfg.glow} shadow-sm`} />
            <span className="text-[15px] font-semibold text-ink truncate leading-tight">
              {data.name}
            </span>
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            {/* Model pill (concept top-right). Shortens the agent_card model to
                a family label (Opus/Sonnet/Haiku/Kimi); falls back to the raw
                last segment, then to the tier badge when no model is known. */}
            {(() => {
              const m = (data.agentCard as Record<string, unknown> | null)?.model;
              const model = typeof m === "string" && m ? m : null;
              if (!model) {
                return (
                  <span className={`text-[11px] font-mono px-2 py-1 rounded-md ${tierCfg.color}`}>
                    {tierCfg.label}
                  </span>
                );
              }
              const label = /opus/i.test(model) ? "Opus"
                : /sonnet/i.test(model) ? "Sonnet"
                : /haiku/i.test(model) ? "Haiku"
                : /kimi/i.test(model) ? "Kimi"
                : /gpt|openai/i.test(model) ? "GPT"
                : /gemini/i.test(model) ? "Gemini"
                : (model.split(/[/:]/).pop() || model);
              return (
                <span className="text-[11px] font-mono px-2 py-1 rounded-md text-white bg-accent" title={model}>
                  {label}
                </span>
              );
            })()}
          </div>
        </div>

        {/* Runtime badge — prefers workspace.runtime (DB column) over
            agent_card.runtime (agent-reported). Phase 30 remote agents
            (runtime='external') get a distinct purple "REMOTE" pill.
            We treat empty-string DB values as "missing" so an unbackfilled
            row falls through to the agent-card value rather than rendering
            a blank pill. */}
        {/* Role pill (concept) — uppercase, accent-bordered. Platform root
            shows "PLATFORM · ROOT"; Phase 30 external-runtime agents get the
            REMOTE marker alongside. */}
        {(() => {
          const dbRuntime = typeof data.runtime === "string" && data.runtime !== ""
            ? data.runtime : null;
          const cardRuntime = data.agentCard && typeof (data.agentCard as Record<string, unknown>).runtime === "string"
            ? (data.agentCard as Record<string, string>).runtime
            : null;
          const runtime = dbRuntime ?? cardRuntime;
          const isRemote = !!runtime && isExternalLikeRuntime(runtime);
          const isPlatformRoot = !data.parentId && hasChildren;
          const roleLabel = isPlatformRoot ? "PLATFORM · ROOT" : (data.role || null);
          if (!roleLabel && !isRemote) return null;
          return (
            <div className="mb-2.5 flex items-center gap-1.5">
              {roleLabel && (
                <span className="max-w-[220px] truncate text-[10px] font-mono uppercase tracking-[0.04em] px-2 py-1 rounded-md text-accent bg-accent/12 border border-accent/35">
                  {roleLabel}
                </span>
              )}
              {isRemote && (
                <span
                  className="text-[10px] font-mono uppercase px-2 py-1 rounded-md text-white bg-violet-800 border border-violet-900"
                  title="Phase 30 remote agent — runs outside this platform's Docker network. Lifecycle managed via heartbeat-based polling, not Docker exec."
                >
                  ★ REMOTE
                </span>
              )}
            </div>
          );
        })()}

        {/* Status line (concept) — uppercase status, "· N AGENTS" for parents,
            with a queued pill on the right. */}
        <div className="mb-2 flex items-center justify-between gap-2">
          <span className={`text-[11px] font-mono uppercase tracking-[0.04em] ${
            isOnline ? "text-good"
              : effectiveStatus === "failed" ? "text-bad"
              : (effectiveStatus === "provisioning" || effectiveStatus === "degraded") ? "text-warm"
              : "text-ink-soft"
          }`}>
            {statusCfg.label}{hasChildren ? ` · ${descendantCount} agents` : ""}
          </span>
          {data.activeTasks > 0 && (
            <span className="shrink-0 text-[11px] font-mono px-2 py-1 rounded-md text-ink-mid bg-surface-card border border-line">
              ≡ {data.activeTasks} queued
            </span>
          )}
        </div>

        {/* Skills */}
        {skills.length > 0 && (
          <div className="flex flex-wrap gap-1 mb-1.5">
            {skills.slice(0, 4).map((skill) => (
              <span
                key={skill}
                className={`text-[10px] px-1.5 py-0.5 rounded-md border ${
                  isOnline
                    ? "text-good bg-good/15 border-good/40"
                    : "text-ink-mid bg-surface-card border-line"
                }`}
              >
                {skill}
              </span>
            ))}
            {skills.length > 4 && (
              <span className="text-[10px] text-ink-mid self-center">
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
            className="flex items-center gap-1.5 mt-1 w-full bg-accent/10 px-2 py-1 rounded-md border border-accent/40 hover:bg-accent/20 transition-colors text-left focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:outline-none"
          >
            <span className="text-[10px] text-accent">↻</span>
            <span className="text-[10px] text-accent">Restart to apply changes</span>
          </button>
        )}

        {/* (status + queued now rendered above, concept-style) */}

        {/* Degraded error preview */}
        {data.status === "degraded" && data.lastSampleError && (
          <div
            className="text-[10px] text-warm truncate mt-1 bg-warm/10 px-1.5 py-0.5 rounded border border-warm/40"
            title={data.lastSampleError}
          >
            {data.lastSampleError}
          </div>
        )}

        {/* Configuration error preview — same visual as the degraded
         *  error preview but keyed off the agent_card's configuration_status.
         *  Tells the operator which env var is missing so they can fix it
         *  without having to dig into the workspace logs. */}
        {isMisconfigured && configurationError && (
          <div
            className="text-[10px] text-warm truncate mt-1 bg-warm/10 px-1.5 py-0.5 rounded border border-warm/40"
            title={configurationError}
          >
            {configurationError}
          </div>
        )}
      </div>

      <Handle
        type="source"
        position={Position.Bottom}
        tabIndex={0}
        role="button"
        aria-label={`Nest selected workspace inside ${data.name} (Enter or Space)`}
        onKeyDown={(e: KeyboardEvent<HTMLDivElement>) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            e.stopPropagation();
            // Keyboard accessibility for edge anchors: pressing Enter/Space on
            // the bottom handle nests the currently-selected node as a child
            // of this node. Requires another node to be selected first.
            const selected = selectedNodeId;
            if (selected && selected !== id) {
              void nestNode(selected, id);
            }
          }
        }}
        className="!w-2.5 !h-1 !rounded-full !bg-surface-card/80 !border-0 !-bottom-0.5 hover:!bg-accent hover:!h-1.5 focus-visible:!bg-accent focus-visible:!h-1.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface transition-all"
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
  const tierCfg = TIER_CONFIG[data.tier] || { label: `T${data.tier}`, color: "text-ink-mid bg-surface-card border border-line" };
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
              <span className="text-[7px] font-mono text-accent bg-accent/15 border border-accent/40 px-1 py-0.5 rounded">
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
              className="opacity-0 group-hover/child:opacity-100 text-ink-mid hover:text-accent transition-all focus-visible:ring-2 focus-visible:ring-accent/70 focus-visible:outline-none rounded"
            >
              <EjectIcon aria-hidden="true" />
            </button>
          </div>
        </div>

        {/* Role */}
        {data.role && (
          <div className="text-[10px] text-ink-mid mb-1 leading-tight truncate">{data.role}</div>
        )}

        {/* Skills */}
        {skills.length > 0 && (
          <div className="flex flex-wrap gap-0.5 mb-1">
            {skills.slice(0, 3).map((skill) => (
              <span
                key={skill}
                className={`text-[10px] px-1 py-0.5 rounded border ${
                  isOnline
                    ? "text-good bg-good/15 border-good/40"
                    : "text-ink-mid bg-surface-card border-line"
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
              data.status === "provisioning" ? "text-accent" :
              "text-ink-mid"
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
