"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { showToast } from "./Toaster";

interface PendingApproval {
  id: string;
  workspace_id: string;
  workspace_name: string;
  action: string;
  reason: string | null;
  status: string;
  created_at: string;
}

export function ApprovalBanner() {
  const [approvals, setApprovals] = useState<PendingApproval[]>([]);
  // Guards double-click / double-keypress during in-flight POST.
  const [pendingApprovalId, setPendingApprovalId] = useState<string | null>(null);

  // Single endpoint — no N+1 per-workspace polling
  const pollApprovals = useCallback(async () => {
    try {
      const res = await api.get<PendingApproval[]>("/approvals/pending");
      setApprovals(res);
    } catch {
      // Table may not exist yet, or no pending approvals
      setApprovals([]);
    }
  }, []);

  useEffect(() => {
    pollApprovals();
    const interval = setInterval(pollApprovals, 10000);
    return () => clearInterval(interval);
  }, [pollApprovals]);

  const handleDecide = async (approval: PendingApproval, decision: "approved" | "denied") => {
    if (pendingApprovalId !== null) return; // guard double-submit
    setPendingApprovalId(approval.id);
    try {
      await api.post(`/workspaces/${approval.workspace_id}/approvals/${approval.id}/decide`, {
        decision,
        decided_by: "human",
      });
      showToast(decision === "approved" ? "Approved" : "Denied", decision === "approved" ? "success" : "info");
      setApprovals((prev) => prev.filter((a) => a.id !== approval.id));
    } catch {
      showToast("Failed to submit decision", "error");
    } finally {
      setPendingApprovalId(null);
    }
  };

  if (approvals.length === 0) return null;

  return (
    <div className="fixed top-16 left-1/2 -translate-x-1/2 z-30 flex flex-col gap-2 items-center">
      {approvals.map((approval) => (
        <div
          key={approval.id}
          role="alert"
          aria-live="assertive"
          aria-atomic="true"
          className="bg-amber-950/90 backdrop-blur-md border border-amber-700/50 rounded-xl px-5 py-3 shadow-2xl shadow-black/40 max-w-md animate-in slide-in-from-top duration-300"
        >
          <div className="flex items-start gap-3">
            <div className="w-8 h-8 rounded-lg bg-amber-800/40 flex items-center justify-center shrink-0 mt-0.5">
              <span className="text-warm text-lg" aria-hidden="true">⚠</span>
            </div>
            <div className="flex-1 min-w-0">
              <div className="text-xs text-amber-200 font-semibold">{approval.workspace_name} needs approval</div>
              {/* Clamp action + reason: agents can author very long messages,
                  and unclamped these sprawled the banner into a full-canvas
                  text column. line-clamp works here (plain-text fields, not
                  markdown). break-words handles long unbroken tokens. */}
              <div className="text-sm text-amber-100 mt-0.5 font-medium line-clamp-2 break-words">{approval.action}</div>
              {approval.reason && (
                <div className="text-xs text-warm/70 mt-1 line-clamp-3 break-words">{approval.reason}</div>
              )}
              <div className="flex gap-2 mt-3">
                <button
                  type="button"
                  disabled={pendingApprovalId !== null}
                  onClick={() => handleDecide(approval, "approved")}
                  aria-disabled={pendingApprovalId !== null}
                  // Hover goes DARKER — emerald-600 on white text is 3.3:1 (WCAG AA FAIL).
                  // emerald-700 is 4.6:1 (WCAG AA PASS). Hover darkens to emerald-600.
                  className="px-3 py-1.5 bg-emerald-700 hover:bg-emerald-600 disabled:opacity-40 disabled:cursor-not-allowed text-xs rounded-lg text-white font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-amber-950 focus-visible:ring-emerald-400/70"
                >
                  {pendingApprovalId === approval.id ? "…" : "Approve"}
                </button>
                <button
                  type="button"
                  disabled={pendingApprovalId !== null}
                  onClick={() => handleDecide(approval, "denied")}
                  aria-disabled={pendingApprovalId !== null}
                  // `text-ink` (not text-ink-mid) for WCAG AA contrast on bg-surface-card.
                  // text-ink-mid on zinc-800 fails AA at ~3:1; text-ink passes at ~7:1.
                  className="px-3 py-1.5 bg-surface-card hover:bg-surface-elevated hover:text-ink text-ink disabled:opacity-40 disabled:cursor-not-allowed text-xs rounded-lg font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-amber-950 focus-visible:ring-amber-400/70"
                >
                  {pendingApprovalId === approval.id ? "…" : "Deny"}
                </button>
              </div>
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}
