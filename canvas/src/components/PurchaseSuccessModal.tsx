"use client";

/**
 * PurchaseSuccessModal — demo-only post-purchase confirmation.
 *
 * Mounted on the canvas root (`app/page.tsx`). On first paint it inspects
 * `?purchase_success=1[&item=<name>]` on the current URL. If present, it
 * renders a centred modal styled after `ConfirmDialog`, schedules a 5s
 * auto-dismiss, and rewrites the URL via `history.replaceState` to drop
 * the params so a refresh after dismiss does NOT re-show the modal.
 *
 * Mock for the funding demo — there is no real billing surface behind
 * this. The marketplace "Purchase" button on the landing page redirects
 * here with the params; this modal is the only thing the user sees of
 * the "transaction".
 *
 * Styling matches the warm-paper @theme tokens (surface-sunken / line /
 * ink / good) so it tracks light + dark without per-mode overrides.
 */

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

const AUTO_DISMISS_MS = 5000;

function readPurchaseParams(): { open: boolean; item: string | null } {
  if (typeof window === "undefined") return { open: false, item: null };
  const sp = new URLSearchParams(window.location.search);
  const flag = sp.get("purchase_success");
  if (flag !== "1" && flag !== "true") return { open: false, item: null };
  return { open: true, item: sp.get("item") };
}

function stripPurchaseParams() {
  if (typeof window === "undefined") return;
  const url = new URL(window.location.href);
  url.searchParams.delete("purchase_success");
  url.searchParams.delete("item");
  // replaceState (not pushState) so back-button doesn't return to the
  // pre-strip URL and re-trigger the modal.
  window.history.replaceState({}, "", url.toString());
}

export function PurchaseSuccessModal() {
  const [open, setOpen] = useState(false);
  const [item, setItem] = useState<string | null>(null);
  const [mounted, setMounted] = useState(false);
  const dialogRef = useRef<HTMLDivElement>(null);

  // Read the URL params once on mount. We don't subscribe to navigation —
  // this modal is a one-shot for the demo redirect, not a persistent
  // listener.
  useEffect(() => {
    setMounted(true);
    const { open: shouldOpen, item: itemName } = readPurchaseParams();
    if (shouldOpen) {
      setOpen(true);
      setItem(itemName);
      // Clean the URL immediately so a refresh after the modal is closed
      // (or even while it's still open) does NOT re-trigger it.
      stripPurchaseParams();
    }
  }, []);

  // Auto-dismiss timer + Escape handler.
  useEffect(() => {
    if (!open) return;
    const t = window.setTimeout(() => setOpen(false), AUTO_DISMISS_MS);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    // Focus the close button so keyboard users land on it after redirect.
    const raf = requestAnimationFrame(() => {
      dialogRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
    });
    return () => {
      window.clearTimeout(t);
      window.removeEventListener("keydown", onKey);
      cancelAnimationFrame(raf);
    };
  }, [open]);

  if (!open || !mounted) return null;

  const itemLabel = item ? decodeURIComponent(item) : "Your new agent";

  return createPortal(
    <div
      className="fixed inset-0 z-[9999] flex items-center justify-center"
      data-testid="purchase-success-modal"
    >
      {/* Backdrop — click closes, matches ConfirmDialog backdrop. */}
      <div
        className="absolute inset-0 bg-black/60 backdrop-blur-sm"
        onClick={() => setOpen(false)}
        aria-hidden="true"
      />

      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="purchase-success-title"
        className="relative bg-surface-sunken border border-line rounded-xl shadow-2xl shadow-black/50 max-w-[420px] w-full mx-4 overflow-hidden"
      >
        <div className="px-6 pt-6 pb-4">
          <div className="flex items-start gap-4">
            {/* Success glyph — uses --color-good so it tracks the theme.
                Inline SVG over an emoji so it stays readable + on-brand
                in both light and dark. */}
            <div
              className="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-full"
              style={{
                background:
                  "color-mix(in srgb, var(--color-good) 15%, transparent)",
                color: "var(--color-good)",
              }}
            >
              <svg
                width="22"
                height="22"
                viewBox="0 0 24 24"
                fill="none"
                aria-hidden="true"
              >
                <circle
                  cx="12"
                  cy="12"
                  r="10"
                  stroke="currentColor"
                  strokeWidth="1.5"
                />
                <path
                  d="M7.5 12.5L10.5 15.5L16.5 9.5"
                  stroke="currentColor"
                  strokeWidth="1.8"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            </div>
            <div className="flex-1">
              <h3
                id="purchase-success-title"
                className="text-base font-semibold text-ink"
              >
                Purchase successful
              </h3>
              <p className="mt-1.5 text-[13px] leading-relaxed text-ink-mid">
                <span className="font-medium text-ink">{itemLabel}</span> has
                been added to your workspace. Provisioning starts in the
                background — you can keep working while it spins up.
              </p>
            </div>
          </div>
        </div>

        <div className="flex items-center justify-between gap-3 px-6 py-3 border-t border-line bg-surface/50">
          <span className="font-mono text-[10.5px] uppercase tracking-[0.12em] text-ink-soft">
            auto-dismiss · {AUTO_DISMISS_MS / 1000}s
          </span>
          <button
            type="button"
            onClick={() => setOpen(false)}
            className="px-3.5 py-1.5 text-[13px] rounded-lg bg-accent hover:bg-accent-strong text-white transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-sunken focus-visible:ring-accent/60"
          >
            Close
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
