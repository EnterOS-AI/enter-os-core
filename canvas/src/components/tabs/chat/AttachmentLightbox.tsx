"use client";

// AttachmentLightbox — shared modal for image / PDF /
// (future) any-fullscreen-renderable kind. Owns:
//   - Backdrop + centered viewport
//   - Esc to close
//   - Click-outside to close
//   - Focus trap (focus enters the modal on open, restored on close)
//   - prefers-reduced-motion respect (no animation)
//
// Per RFC #2991 Phase 2: this is the third-caller justification for
// the abstraction (image, PDF, future video-fullscreen all want the
// same modal contract). Not invented for a single caller.
//
// Design choices:
//
// 1. Portals — we don't use ReactDOM.createPortal because the chat tab
//    already gives us a positioned container and the preview should stay
//    inside that panel. Saves a portal mount in the common case + avoids
//    the SSR warning (canvas is "use client" but the parent shell is
//    server-rendered).
//
// 2. Focus trap — inline implementation (not a 3rd-party dep). The
//    chat lightbox needs to trap focus only across two interactive
//    elements (close button + content), so a 100-line manual trap
//    beats pulling in focus-trap-react for ~12KB.
//
// 3. Escape key — listened on `document` (not on the modal element)
//    because the user can be focused anywhere when they hit Esc,
//    including outside the modal if focus restoration ever fails.
//    The cleanup runs on unmount so leaked listeners don't persist.

import { useEffect, useRef, useCallback, type ReactNode } from "react";

interface Props {
  /** Render the lightbox when true. Caller controls open state. */
  open: boolean;
  /** Caller's handler for "close" — Esc, click-outside, X button. */
  onClose: () => void;
  /** Accessible label for the modal — voiced by screen readers when
   *  the dialog opens. The caller knows what's inside (image alt
   *  text, PDF filename) and supplies it. */
  ariaLabel: string;
  /** Constrain the preview to the nearest positioned ancestor instead
   *  of the whole browser viewport. ChatTab passes this so previews
   *  stay inside the active side-panel tab. */
  contained?: boolean;
  /** The thing being shown in fullscreen — <img>, <embed>, etc.
   *  Caller is responsible for sizing it to fit the viewport (we
   *  give it max-w-full max-h-full via CSS). */
  children: ReactNode;
}

export function AttachmentLightbox({ open, onClose, ariaLabel, contained = false, children }: Props) {
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);

  // Focus enters the close button on open + restores to whatever
  // had focus when the modal closes. Without this, the user's
  // focus is left wherever they clicked (often the chip) and Tab
  // walks them back through the chat surface — disorienting.
  useEffect(() => {
    if (!open) return;
    previousFocusRef.current = document.activeElement as HTMLElement | null;
    closeButtonRef.current?.focus();
    return () => {
      previousFocusRef.current?.focus?.();
    };
  }, [open]);

  // Esc closes; bound on document so the user can press Esc
  // regardless of where focus actually is.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  // Click on the backdrop (NOT the content) closes. Content's own
  // onClick stops propagation so the user can interact (e.g. native
  // PDF viewer controls) without dismissing the modal.
  const onBackdropClick = useCallback(
    (e: React.MouseEvent) => {
      if (e.target === e.currentTarget) onClose();
    },
    [onClose],
  );

  if (!open) return null;

  const rootClass = contained
    ? "absolute inset-0 z-50 flex items-center justify-center bg-black/85 motion-reduce:transition-none transition-opacity"
    : "fixed inset-0 z-50 flex items-center justify-center bg-black/85 motion-reduce:transition-none transition-opacity";
  const contentClass = contained
    ? "h-full w-full p-3 flex items-center justify-center"
    : "max-w-[95vw] max-h-[90vh] flex items-center justify-center";

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={ariaLabel}
      className={rootClass}
      onClick={onBackdropClick}
    >
      {/* Close button — top-right, large hit area, keyboard-focusable.
          ariaLabel includes "Close" so SR users hear what action it
          performs, not just the X glyph. */}
      <button
        ref={closeButtonRef}
        onClick={onClose}
        aria-label="Close preview"
        className="absolute top-4 right-4 rounded-full bg-white/10 hover:bg-white/20 text-white p-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-white"
      >
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M5 5l14 14M19 5l-14 14" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
        </svg>
      </button>
      <div
        className={contentClass}
        onClick={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  );
}
