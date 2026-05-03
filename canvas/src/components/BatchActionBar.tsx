"use client";

import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { useCanvasStore } from "@/store/canvas";
import { ConfirmDialog } from "./ConfirmDialog";
import { showToast } from "./Toaster";

type BatchAction = "restart" | "pause" | "delete" | null;

export function BatchActionBar() {
  const selectedNodeIds = useCanvasStore((s) => s.selectedNodeIds);
  const clearSelection = useCanvasStore((s) => s.clearSelection);
  const batchRestart = useCanvasStore((s) => s.batchRestart);
  const batchPause = useCanvasStore((s) => s.batchPause);
  const batchDelete = useCanvasStore((s) => s.batchDelete);

  const [pending, setPending] = useState<BatchAction>(null);
  const [busy, setBusy] = useState(false);
  // Retry survivorship (QA pr-949 follow-up): when a batch action partial-fails
  // and leaves a single survivor id, the default `count < 2` gate unmounts the
  // bar and forces per-node context-menu retry. Track "active failure" so the
  // bar stays mounted with a single item and the user can click the same action
  // button to retry without re-selecting. Resets on success or Escape/clear.
  const [hasFailedBatch, setHasFailedBatch] = useState(false);

  const count = selectedNodeIds.size;
  // Reset failure flag when the user clears selection (Escape / ✕ button).
  useEffect(() => {
    if (count === 0 && hasFailedBatch) setHasFailedBatch(false);
  }, [count, hasFailedBatch]);

  // Hide when nothing is selected. Hide for single-node selection UNLESS a
  // partial-failure left a survivor awaiting retry.
  if (count === 0) return null;
  if (count < 2 && !hasFailedBatch) return null;

  // Message copy must handle both multi (count >= 2) and single-survivor retry
  // (count === 1 && hasFailedBatch). Use a helper so we render singular form
  // only when there is exactly one survivor to act on.
  const plural = (n: number) => (n === 1 ? "workspace" : "workspaces");
  const confirmMessages: Record<NonNullable<BatchAction>, string> = {
    restart: `Restart ${count} ${plural(count)}? Each will briefly go offline while it restarts.`,
    pause:   `Pause ${count} ${plural(count)}? Their containers will be stopped.`,
    delete:  `Permanently delete ${count} ${plural(count)}? This cannot be undone.`,
  };

  const confirmLabels: Record<NonNullable<BatchAction>, string> = {
    restart: "Restart All",
    pause:   "Pause All",
    delete:  "Delete All",
  };

  async function execute() {
    if (!pending) return;
    setBusy(true);
    try {
      if (pending === "restart") await batchRestart();
      if (pending === "pause")   await batchPause();
      if (pending === "delete")  await batchDelete();
      // Reaching here means every store call fulfilled (the store throws on
      // any partial failure), so `count` is the actual success count.
      showToast(`${pending.charAt(0).toUpperCase() + pending.slice(1)} applied to ${count} ${plural(count)}`, "success");
      setHasFailedBatch(false);
      clearSelection();
    } catch (e) {
      const msg = e instanceof Error && e.message ? e.message : `Batch ${pending} failed`;
      showToast(msg, "error");
      // Leave the failed IDs selected (the store preserved them) so the user
      // can retry without re-selecting, and set hasFailedBatch so the bar
      // stays mounted even if a single survivor remains.
      setHasFailedBatch(true);
    } finally {
      setBusy(false);
      setPending(null);
    }
  }

  const bar = (
    <div
      role="toolbar"
      aria-label="Batch workspace actions"
      className="fixed bottom-6 left-1/2 -translate-x-1/2 z-[200] flex items-center gap-3 px-4 py-2.5 rounded-2xl bg-surface-sunken/95 border border-line/70 shadow-2xl shadow-black/50 backdrop-blur-md"
    >
      {/* Selection count badge */}
      <span className="text-[12px] font-semibold text-white bg-accent-strong/80 px-2.5 py-0.5 rounded-full tabular-nums">
        {count} selected
      </span>

      <div className="w-px h-5 bg-surface-card/60" aria-hidden="true" />

      {/* Action buttons */}
      <button
        type="button"
        disabled={busy}
        onClick={() => setPending("restart")}
        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-[12px] font-medium text-sky-300 bg-sky-900/30 hover:bg-sky-800/50 border border-sky-700/30 hover:border-sky-600/50 transition-colors disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500/70"
      >
        <span aria-hidden="true">↻</span>
        Restart All
      </button>

      <button
        type="button"
        disabled={busy}
        onClick={() => setPending("pause")}
        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-[12px] font-medium text-warm bg-amber-900/30 hover:bg-amber-800/50 border border-amber-700/30 hover:border-amber-600/50 transition-colors disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-500/70"
      >
        <span aria-hidden="true">⏸</span>
        Pause All
      </button>

      <button
        type="button"
        disabled={busy}
        onClick={() => setPending("delete")}
        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-[12px] font-medium text-bad bg-red-900/30 hover:bg-red-800/50 border border-red-700/30 hover:border-red-600/50 transition-colors disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-red-500/70"
      >
        <span aria-hidden="true">✕</span>
        Delete All
      </button>

      <div className="w-px h-5 bg-surface-card/60" aria-hidden="true" />

      {/* Deselect */}
      <button
        type="button"
        disabled={busy}
        onClick={clearSelection}
        aria-label="Clear selection"
        title="Clear selection (Escape)"
        className="p-1.5 rounded-lg text-[12px] text-ink-mid hover:text-ink hover:bg-surface-card/50 transition-colors disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-500/70"
      >
        ✕
      </button>
    </div>
  );

  return (
    <>
      {typeof window !== "undefined" ? createPortal(bar, document.body) : null}

      <ConfirmDialog
        open={!!pending}
        title={pending ? confirmLabels[pending] : ""}
        message={pending ? confirmMessages[pending] : ""}
        confirmLabel={pending ? confirmLabels[pending] : "Confirm"}
        confirmVariant={pending === "delete" ? "danger" : pending === "pause" ? "warning" : "primary"}
        onConfirm={execute}
        onCancel={() => setPending(null)}
      />
    </>
  );
}
