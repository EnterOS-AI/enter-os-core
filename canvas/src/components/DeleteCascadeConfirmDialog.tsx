"use client";

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

interface Child {
  id: string;
  name: string;
}

interface Props {
  name: string;
  children: Child[];
  checked: boolean;
  onCheckedChange: (v: boolean) => void;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * Cascade-delete confirmation dialog.
 *
 * When a workspace has children, the operator must explicitly tick
 * "I understand this will cascade" before Delete All activates. This
 * prevents accidental mass-deletion when ?confirm=true is always sent.
 *
 * Per WCAG 2.1 SC 2.4.3: focus moves to dialog on open.
 * Per WCAG 2.1 SC 3.3.2: labels associated with inputs.
 */
export function DeleteCascadeConfirmDialog({
  name,
  children,
  checked,
  onCheckedChange,
  onConfirm,
  onCancel,
}: Props) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setMounted(true);
  }, []);

  // Focus first interactive element when dialog opens (WCAG 2.4.3)
  useEffect(() => {
    if (!mounted) return;
    const raf = requestAnimationFrame(() => {
      dialogRef.current?.querySelector<HTMLElement>("button")?.focus();
    });
    return () => cancelAnimationFrame(raf);
  }, [mounted]);

  // Keyboard: Escape cancels, Enter confirms (only when enabled), Tab trapped
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") { onCancel(); return; }
      if (e.key === "Enter" && checked) { onConfirm(); return; }
      if (e.key === "Tab" && dialogRef.current) {
        const focusable = Array.from(
          dialogRef.current.querySelectorAll<HTMLElement>(
            'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
          )
        ).filter((el) => !el.hasAttribute("disabled"));
        if (focusable.length === 0) { e.preventDefault(); return; }
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey) {
          if (document.activeElement === first) { e.preventDefault(); last.focus(); }
        } else {
          if (document.activeElement === last) { e.preventDefault(); first.focus(); }
        }
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [onCancel, onConfirm, checked]);

  if (!mounted) return null;

  return createPortal(
    <div className="fixed inset-0 z-[9999] flex items-center justify-center">
      {/* Backdrop */}
      <div aria-hidden="true" className="absolute inset-0 bg-black/60 backdrop-blur-sm" onClick={onCancel} />

      {/* Dialog */}
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="cascade-dialog-title"
        className="relative bg-surface-sunken border border-red-800/60 rounded-xl shadow-2xl shadow-black/50 max-w-[420px] w-full mx-4 overflow-hidden"
      >
        <div className="px-5 py-4 border-b border-line">
          <h3 id="cascade-dialog-title" className="text-sm font-semibold text-bad">
            Delete Workspace and Children
          </h3>
        </div>

        <div className="px-5 py-4">
          {/* Warning */}
          <div className="flex gap-3 mb-4">
            <div className="mt-0.5 shrink-0 w-8 h-8 rounded-full bg-red-900/30 flex items-center justify-center">
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none" className="text-bad" aria-hidden="true">
                <path d="M8 3L14 13H2L8 3Z" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round"/>
                <path d="M8 7v3M8 11.5v.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"/>
              </svg>
            </div>
            <p className="text-[13px] text-ink-mid leading-relaxed">
              <span className="font-medium text-bad">"{name}"</span> has{" "}
              <strong className="text-ink">{children.length}</strong> child{" "}
              {children.length === 1 ? "workspace" : "workspaces"}:
            </p>
          </div>

          {/* Child list */}
          <ul className="space-y-1.5 mb-4 ml-4 list-disc list-inside text-[12px] text-ink-mid max-h-32 overflow-y-auto">
            {children.map((c) => (
              <li key={c.id} className="truncate" title={c.name}>{c.name}</li>
            ))}
          </ul>

          {/* Cascade warning */}
          <div className="rounded border border-red-900/40 bg-red-950/20 px-3 py-2.5 mb-4">
            <p className="text-[12px] text-bad/80 leading-relaxed">
              Deleting will cascade — <strong className="text-red-200">all child workspaces and their data will be permanently removed.</strong> This cannot be undone.
            </p>
          </div>

          {/* Checkbox guard. Ring-offset color was zinc-900 — the dialog
              actually sits on bg-surface-sunken, so the offset showed
              the wrong color through the ring gap. Switched to the
              real bg + a danger-tinted ring. */}
          <label className="flex items-start gap-2.5 cursor-pointer group select-none">
            <input
              type="checkbox"
              checked={checked}
              onChange={(e) => onCheckedChange(e.target.checked)}
              className="mt-0.5 w-4 h-4 rounded border-line bg-surface-card text-bad cursor-pointer focus:outline-none focus-visible:ring-2 focus-visible:ring-red-500/60 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-sunken"
            />
            <span className="text-[12px] text-ink-mid group-hover:text-ink-mid leading-relaxed">
              I understand this will permanently delete all listed workspaces and their data
            </span>
          </label>
        </div>

        <div className="flex items-center justify-end gap-2 px-5 py-3 border-t border-line bg-surface/50">
          <button
            type="button"
            onClick={onCancel}
            // Was hover:bg-surface-card (same as base — silent no-op).
            // Lift to surface-elevated to match the Cancel pattern in
            // ConfirmDialog. Added focus-visible ring so keyboard users
            // see where focus lands.
            className="px-3.5 py-1.5 text-[13px] text-ink-mid hover:text-ink bg-surface-card hover:bg-surface-elevated border border-line hover:border-line-soft rounded-lg transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-sunken"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={!checked}
            // Hover goes DARKER, not lighter — bg-red-500 on white text
            // drops contrast below AA vs bg-red-700. Same trap fixed in
            // ConfirmDialog and ApprovalBanner. focus-visible ring matches.
            className={`px-3.5 py-1.5 text-[13px] rounded-lg transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-red-500/60 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-sunken
              ${checked
                ? "bg-red-600 hover:bg-red-700 text-white cursor-pointer"
                : "bg-red-900/30 text-bad/40 cursor-not-allowed"
              }`}
          >
            Delete All
          </button>
        </div>
      </div>
    </div>,
    document.body
  );
}