"use client";

import { useState, useEffect, useCallback, useRef, useMemo } from "react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { pruneStaleKeys } from "./canvas/useCanvasViewport";
import { api } from "@/lib/api";
import { showToast } from "./Toaster";
import { ConsoleModal } from "./ConsoleModal";

import {
  DEFAULT_RUNTIME_PROFILE,
  provisionTimeoutForRuntime,
} from "@/lib/runtimeProfiles";

/** Re-export for backward compatibility with tests and other importers
 *  that previously imported DEFAULT_PROVISION_TIMEOUT_MS from this file.
 *  New code should read via getRuntimeProfile() from @/lib/runtimeProfiles. */
export const DEFAULT_PROVISION_TIMEOUT_MS =
  DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs;

/** The server provisions up to `PROVISION_CONCURRENCY` containers at
 *  once and paces the rest in a queue (`workspaceCreatePacingMs` =
 *  2s). Mirrors the Go constants — if those change, bump these. */
const PROVISION_CONCURRENCY = 3;
const PER_QUEUE_SLOT_EXTRA_MS = 45_000; // ~45s head-room per queued workspace

/** Scale the base timeout by how many workspaces are provisioning at
 *  once. A 30-workspace org import has tail items that legitimately
 *  wait minutes before Docker even starts on them — flagging each as
 *  "stuck" after 2m creates a wall of 27 yellow banners that buries
 *  the canvas. */
function effectiveTimeoutMs(base: number, concurrentCount: number): number {
  const overflow = Math.max(0, concurrentCount - PROVISION_CONCURRENCY);
  return base + overflow * PER_QUEUE_SLOT_EXTRA_MS;
}

interface TimeoutEntry {
  workspaceId: string;
  workspaceName: string;
  startedAt: number;
}

/**
 * Monitors workspaces in "provisioning" status and shows a timeout banner
 * with recovery actions (Retry, Cancel, View Logs) when provisioning takes
 * too long.
 *
 * Rendered at the top of the canvas (inside Canvas component). Watches the
 * Zustand store for nodes with status === "provisioning" and tracks elapsed
 * time per node.
 */
export function ProvisioningTimeout({
  timeoutMs,
}: {
  // If undefined (the default when mounted without a prop), each workspace's
  // threshold is resolved from its runtime via timeoutForRuntime().
  // Pass an explicit number to force a single threshold for every workspace
  // (used by tests that want deterministic behavior regardless of runtime).
  timeoutMs?: number;
}) {
  const [timedOut, setTimedOut] = useState<TimeoutEntry[]>([]);
  const [retrying, setRetrying] = useState<Set<string>>(new Set());
  const [cancelling, setCancelling] = useState<Set<string>>(new Set());
  const trackingRef = useRef<Map<string, number>>(new Map());
  // Workspaces the user explicitly dismissed — don't re-show their
  // banner even if they stay in provisioning. Cleared when the
  // workspace leaves provisioning (status changes).
  const [dismissed, setDismissed] = useState<Set<string>>(new Set());
  // Watch the live WS health. While it's not "connected", local node
  // status reflects the last event we received before the drop —
  // workspaces may have actually transitioned to online minutes ago.
  // Suppress the banner until WS recovers + rehydrate confirms each
  // workspace is genuinely still provisioning.
  const wsStatus = useCanvasStore((s) => s.wsStatus);

  // Subscribe to provisioning nodes — use shallow compare to avoid infinite re-render
  // (filter+map creates new array reference on every store update).
  // Runtime included so the timeout threshold can be resolved per-node
  // (hermes cold-boot legitimately takes 8-13 min vs 30-90s for docker
  //  runtimes — a single threshold would false-alarm on one or the other).
  // provisionTimeoutMs added by #2054 — server-declared per-workspace
  // override that wins over the runtime profile when present.
  // Separator: `|` between fields, `,` between nodes. Only `name` is
  // user-typed (gets sanitized below); the other fields are
  // primitive-typed (id is a UUID, runtime is a [a-z-]+ slug,
  // provisionTimeoutMs is numeric). If a future field is string-typed,
  // extend the sanitize step to strip `|` + `,` from it too.
  // Empty-string sentinels for missing values so split/index stays positional.
  const provisioningNodes = useCanvasStore((s) => {
    const result = s.nodes
      .filter((n) => n.data.status === "provisioning")
      .map((n) => {
        const safeName = (n.data.name ?? "").replace(/[|,]/g, " ");
        const runtime = n.data.runtime ?? "";
        const provisionTimeoutMs = n.data.provisionTimeoutMs ?? "";
        return `${n.id}|${safeName}|${runtime}|${provisionTimeoutMs}`;
      });
    return result.join(",");
  });
  const parsedProvisioningNodes = useMemo(
    () =>
      provisioningNodes
        ? provisioningNodes.split(",").map((entry) => {
            const [id, name, runtime, provisionTimeoutMs] = entry.split("|");
            const ptms = provisionTimeoutMs ? Number(provisionTimeoutMs) : undefined;
            return {
              id,
              name,
              runtime,
              provisionTimeoutMs: Number.isFinite(ptms) ? ptms : undefined,
            };
          })
        : [],
    [provisioningNodes],
  );

  useEffect(() => {
    const tracking = trackingRef.current;

    // Start tracking new provisioning nodes
    for (const node of parsedProvisioningNodes) {
      if (!tracking.has(node.id)) {
        tracking.set(node.id, Date.now());
      }
    }

    // Remove tracking for nodes that are no longer provisioning
    const activeIds = new Set(parsedProvisioningNodes.map((n) => n.id));
    pruneStaleKeys(tracking, activeIds);

    // Also remove from timedOut list if no longer provisioning, and
    // clear `dismissed` entries for workspaces that finished so a
    // re-provision (e.g. retry) can surface a fresh banner.
    setTimedOut((prev) => prev.filter((e) => activeIds.has(e.workspaceId)));
    setDismissed((prev) => {
      let changed = false;
      const next = new Set(prev);
      for (const id of prev) {
        if (!activeIds.has(id)) {
          next.delete(id);
          changed = true;
        }
      }
      return changed ? next : prev;
    });

    // Interval to check for timeouts
    const interval = setInterval(() => {
      const now = Date.now();
      const newTimedOut: TimeoutEntry[] = [];

      // Per-node timeout: each workspace resolves its own base via
      // @/lib/runtimeProfiles (server-override → runtime profile →
      // default), then scales by concurrent-provisioning count. A
      // hermes workspace in a batch alongside two langgraph workspaces
      // gets hermes's 12-min base, not langgraph's 2-min base.
      //
      // Resolution priority (most specific wins):
      //   1. node.provisionTimeoutMs — server-declared per-workspace
      //      override (#2054, sourced from template manifest)
      //   2. timeoutMs prop — single-threshold test override
      //   3. runtime profile in @/lib/runtimeProfiles
      //   4. DEFAULT_RUNTIME_PROFILE
      for (const node of parsedProvisioningNodes) {
        const startedAt = tracking.get(node.id);
        if (!startedAt) continue;
        const base = provisionTimeoutForRuntime(node.runtime, {
          provisionTimeoutMs: node.provisionTimeoutMs ?? timeoutMs,
        });
        const effective = effectiveTimeoutMs(
          base,
          parsedProvisioningNodes.length,
        );
        if (now - startedAt >= effective) {
          newTimedOut.push({
            workspaceId: node.id,
            workspaceName: node.name,
            startedAt,
          });
        }
      }

      if (newTimedOut.length > 0) {
        setTimedOut((prev) => {
          const existingIds = new Set(prev.map((e) => e.workspaceId));
          const additions = newTimedOut.filter(
            (e) => !existingIds.has(e.workspaceId),
          );
          return additions.length > 0 ? [...prev, ...additions] : prev;
        });
      }
    }, 5_000); // check every 5s

    return () => clearInterval(interval);
  }, [parsedProvisioningNodes, timeoutMs]);

  const handleDismiss = useCallback((workspaceId: string) => {
    setDismissed((prev) => new Set(prev).add(workspaceId));
    setTimedOut((prev) => prev.filter((e) => e.workspaceId !== workspaceId));
  }, []);

  const RETRY_COOLDOWN_MS = 5_000;
  const [retryCooldown, setRetryCooldown] = useState<Set<string>>(new Set());

  const handleRetry = useCallback(async (workspaceId: string) => {
    setRetrying((prev) => new Set(prev).add(workspaceId));
    try {
      await api.post(`/workspaces/${workspaceId}/restart`);
      // Remove from timed-out list — tracking will restart when provisioning event comes in
      setTimedOut((prev) => prev.filter((e) => e.workspaceId !== workspaceId));
      trackingRef.current.delete(workspaceId);
      showToast("Retrying deployment...", "info");
    } catch (e) {
      showToast(
        e instanceof Error ? e.message : "Retry failed",
        "error",
      );
    } finally {
      setRetrying((prev) => {
        const next = new Set(prev);
        next.delete(workspaceId);
        return next;
      });
      // Start cooldown — disable retry button for 5s
      setRetryCooldown((prev) => new Set(prev).add(workspaceId));
      setTimeout(() => {
        setRetryCooldown((prev) => {
          const next = new Set(prev);
          next.delete(workspaceId);
          return next;
        });
      }, RETRY_COOLDOWN_MS);
    }
  }, []);

  const [confirmingCancel, setConfirmingCancel] = useState<string | null>(null);

  const handleCancelRequest = useCallback((workspaceId: string) => {
    setConfirmingCancel(workspaceId);
  }, []);

  const handleCancelConfirm = useCallback(async () => {
    if (!confirmingCancel) return;
    const workspaceId = confirmingCancel;
    setConfirmingCancel(null);
    setCancelling((prev) => new Set(prev).add(workspaceId));
    try {
      await api.del(`/workspaces/${workspaceId}`);
      setTimedOut((prev) => prev.filter((e) => e.workspaceId !== workspaceId));
      trackingRef.current.delete(workspaceId);
      showToast("Deployment cancelled", "info");
    } catch (e) {
      showToast(
        e instanceof Error ? e.message : "Cancel failed",
        "error",
      );
    } finally {
      setCancelling((prev) => {
        const next = new Set(prev);
        next.delete(workspaceId);
        return next;
      });
    }
  }, [confirmingCancel]);

  const [consoleFor, setConsoleFor] = useState<string | null>(null);
  const handleViewLogs = useCallback((workspaceId: string) => {
    // Open the EC2 console modal — this is the boot-trace log, which
    // is what the user actually wants to see when provisioning is
    // stuck (the terminal tab is post-boot, useless if the agent
    // runtime never started). The modal closes over itself if the
    // request returns 501 (self-hosted / docker-compose deploys) —
    // the user gets a clear "console output unavailable" message
    // instead of a broken button.
    setConsoleFor(workspaceId);
  }, []);

  const visibleTimedOut = useMemo(
    () =>
      wsStatus === "connected"
        ? timedOut.filter((e) => !dismissed.has(e.workspaceId))
        : [],
    [timedOut, dismissed, wsStatus],
  );

  if (visibleTimedOut.length === 0) return null;

  return (
    <div role="alert" aria-live="assertive" className="fixed top-14 left-1/2 -translate-x-1/2 z-40 flex flex-col gap-2 max-w-[480px] w-full px-4">
      {visibleTimedOut.map((entry) => {
        const elapsed = Math.round((Date.now() - entry.startedAt) / 1000);
        const isRetrying = retrying.has(entry.workspaceId);
        const isCancelling = cancelling.has(entry.workspaceId);

        return (
          <div
            key={entry.workspaceId}
            className="bg-amber-950/90 border border-amber-700/40 rounded-xl px-4 py-3 shadow-2xl shadow-black/40 backdrop-blur-md"
          >
            <div className="flex items-start gap-3">
              {/* Warning icon */}
              <div aria-hidden="true" className="w-8 h-8 rounded-lg bg-amber-600/20 border border-amber-500/30 flex items-center justify-center shrink-0 mt-0.5">
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
                  <path
                    d="M8 2L14 13H2L8 2Z"
                    stroke="#fbbf24"
                    strokeWidth="1.3"
                    strokeLinejoin="round"
                  />
                  <path d="M8 7V9.5" stroke="#fbbf24" strokeWidth="1.3" strokeLinecap="round" />
                  <circle cx="8" cy="11" r="0.6" fill="#fbbf24" />
                </svg>
              </div>

              <div className="flex-1 min-w-0">
                <div className="flex items-center justify-between mb-0.5 gap-2">
                  <div className="text-[12px] font-semibold text-amber-200">
                    Provisioning Timeout
                  </div>
                  <button
                    onClick={() => handleDismiss(entry.workspaceId)}
                    aria-label="Dismiss provisioning timeout warning"
                    title="Dismiss — keep this workspace running without the warning"
                    className="shrink-0 text-amber-400/60 hover:text-amber-200 transition-colors -mr-1"
                  >
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
                      <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
                    </svg>
                  </button>
                </div>
                <div className="text-[11px] text-amber-300/80 leading-relaxed">
                  <span className="font-medium text-amber-200">{entry.workspaceName}</span>{" "}
                  has been provisioning for{" "}
                  <span className="font-mono text-amber-300">{formatDuration(elapsed)}</span>.
                  It may have encountered an issue.
                </div>

                {/* Action buttons */}
                <div className="flex items-center gap-2 mt-2.5">
                  <button
                    type="button"
                    onClick={() => handleRetry(entry.workspaceId)}
                    disabled={isRetrying || isCancelling || retryCooldown.has(entry.workspaceId)}
                    className="px-3 py-1.5 bg-amber-600 hover:bg-amber-500 text-[11px] font-medium rounded-lg text-white disabled:opacity-40 transition-colors"
                  >
                    {isRetrying ? "Retrying..." : retryCooldown.has(entry.workspaceId) ? "Wait..." : "Retry"}
                  </button>
                  <button
                    type="button"
                    onClick={() => handleCancelRequest(entry.workspaceId)}
                    disabled={isRetrying || isCancelling}
                    className="px-3 py-1.5 bg-zinc-800 hover:bg-zinc-700 text-[11px] text-zinc-300 rounded-lg border border-zinc-600 disabled:opacity-40 transition-colors"
                  >
                    {isCancelling ? "Cancelling..." : "Cancel"}
                  </button>
                  <button
                    type="button"
                    onClick={() => handleViewLogs(entry.workspaceId)}
                    className="px-3 py-1.5 text-[11px] text-amber-400 hover:text-amber-300 transition-colors"
                  >
                    View Logs
                  </button>
                </div>
              </div>
            </div>
          </div>
        );
      })}

      {/* Cancel confirmation dialog */}
      {confirmingCancel && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div aria-hidden="true" className="absolute inset-0 bg-black/60" onClick={() => setConfirmingCancel(null)} />
          <div className="relative bg-zinc-900 border border-zinc-700 rounded-xl shadow-2xl p-5 max-w-[340px] w-full mx-4">
            <h3 className="text-sm font-semibold text-zinc-100 mb-2">
              Cancel deployment?
            </h3>
            <p className="text-[12px] text-zinc-400 mb-4 leading-relaxed">
              This will permanently remove the workspace. This action cannot be undone.
            </p>
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setConfirmingCancel(null)}
                className="px-3.5 py-1.5 text-[12px] text-zinc-400 hover:text-zinc-200 bg-zinc-800 hover:bg-zinc-700 border border-zinc-700 rounded-lg transition-colors"
              >
                Keep
              </button>
              <button
                type="button"
                onClick={handleCancelConfirm}
                className="px-3.5 py-1.5 text-[12px] bg-red-600 hover:bg-red-500 text-white rounded-lg transition-colors"
              >
                Remove Workspace
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Console output modal — opens when the user clicks "View Logs" on
          a stuck-provisioning banner. Fetches /workspaces/:id/console
          which proxies to CP's ec2:GetConsoleOutput. */}
      <ConsoleModal
        workspaceId={consoleFor || ""}
        open={consoleFor !== null}
        onClose={() => setConsoleFor(null)}
      />
    </div>
  );
}

/** Format seconds into a human-friendly string like "2m 30s" */
function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const mins = Math.floor(seconds / 60);
  const secs = seconds % 60;
  return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
}
