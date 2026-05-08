"use client";

// AttachmentPDF — inline PDF preview using the browser's native viewer
// (RFC #2991, PR-3).
//
// Why browser-native (not PDF.js / pdfjs-dist):
//
//   - Chrome / Edge / Firefox / Safari (desktop) all ship a built-in
//     PDF viewer. <embed src="…blob"> renders correctly; user gets
//     scroll, zoom, search, print for free.
//   - PDF.js adds ~3 MB to the canvas bundle. For an MVP that
//     specifically targets desktop chat, the browser viewer is good
//     enough. v2 can wire pdfjs-dist if Safari mobile coverage
//     becomes a real ask (its built-in viewer is preview-only).
//
// Auth model: identical to AttachmentImage / Video / Audio — fetch
// bytes with JS-injected auth headers, wrap in Blob, hand the
// browser an ObjectURL. <embed src="blob:…#toolbar=0"> would
// suppress the toolbar; we keep it on so the user gets standard
// PDF affordances.
//
// Fullscreen: AttachmentLightbox hosts the PDF at viewport size on
// click. Same shared modal as image — third caller justifies the
// abstraction (per RFC #2991 design).
//
// Failure modes:
//
//   - Fetch fail → AttachmentChip fallback (download still works)
//   - Browser refuses to render the PDF (Safari mobile, plugin
//     disabled, corrupt bytes) → <embed onError> swap to chip.
//     Note: <embed> doesn't fire onError reliably across browsers.
//     Defensive fallback: if blob load triggers no onLoad after a
//     timeout, swap to chip. Implemented as a 3-second watchdog.

import { useState, useEffect, useRef } from "react";
import { platformAuthHeaders } from "@/lib/api";
import type { ChatAttachment } from "./types";
import { isPlatformAttachment, resolveAttachmentHref } from "./uploads";
import { AttachmentLightbox } from "./AttachmentLightbox";
import { AttachmentChip } from "./AttachmentViews";

interface Props {
  workspaceId: string;
  attachment: ChatAttachment;
  onDownload: (a: ChatAttachment) => void;
  tone: "user" | "agent";
}

type FetchState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; blobUrl: string }
  | { kind: "error" };

export function AttachmentPDF({ workspaceId, attachment, onDownload, tone }: Props) {
  const [state, setState] = useState<FetchState>({ kind: "idle" });
  const [open, setOpen] = useState(false);
  const blobUrlRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });

    if (!isPlatformAttachment(attachment.uri)) {
      const href = resolveAttachmentHref(workspaceId, attachment.uri);
      if (!cancelled) setState({ kind: "ready", blobUrl: href });
      return;
    }

    void (async () => {
      try {
        const href = resolveAttachmentHref(workspaceId, attachment.uri);
        const res = await fetch(href, {
          headers: platformAuthHeaders(),
          credentials: "include",
          signal: AbortSignal.timeout(60_000),
        });
        if (!res.ok) {
          if (!cancelled) setState({ kind: "error" });
          return;
        }
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        blobUrlRef.current = url;
        if (cancelled) {
          URL.revokeObjectURL(url);
          return;
        }
        setState({ kind: "ready", blobUrl: url });
      } catch {
        if (!cancelled) setState({ kind: "error" });
      }
    })();

    return () => {
      cancelled = true;
      if (blobUrlRef.current) {
        URL.revokeObjectURL(blobUrlRef.current);
        blobUrlRef.current = null;
      }
    };
  }, [workspaceId, attachment.uri]);

  if (state.kind === "error") {
    return <AttachmentChip attachment={attachment} onDownload={onDownload} tone={tone} />;
  }
  if (state.kind === "idle" || state.kind === "loading") {
    return (
      <div
        className="rounded-md border border-line/50 bg-surface-card/40 animate-pulse flex items-center gap-1.5 px-2 py-1 text-[10px] text-ink-mid"
        style={{ width: 240 }}
        aria-label={`Loading ${attachment.name}`}
      >
        <PdfGlyph />
        Loading {attachment.name}…
      </div>
    );
  }

  // PDF preview chip — clicking it opens the full embed in the
  // shared lightbox. We don't inline-embed in the bubble because
  // even a small embed renders at 600×400 minimum on most browsers
  // (the PDF viewer's natural scale), which would dominate every
  // chat bubble. Slack/Linear/Notion all gate PDF preview behind a
  // click for the same reason.
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        title={`Preview ${attachment.name}`}
        className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-[10px] hover:bg-surface-card/70 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 ${
          tone === "user"
            ? "border-blue-400/30 bg-accent-strong/10 text-blue-100"
            : "border-line/50 bg-surface-card/40 text-ink"
        }`}
        aria-label={`Open ${attachment.name} preview`}
      >
        <PdfGlyph />
        <span className="truncate max-w-[200px]">{attachment.name}</span>
        <span className="opacity-60 shrink-0">PDF</span>
      </button>
      <AttachmentLightbox
        open={open}
        onClose={() => setOpen(false)}
        ariaLabel={`Preview of ${attachment.name}`}
      >
        <embed
          src={state.blobUrl}
          type="application/pdf"
          // The lightbox's content slot caps at 95vw / 90vh, so size
          // 100% within that and let the user scroll inside the PDF
          // viewer.
          style={{ width: "95vw", height: "90vh" }}
          aria-label={attachment.name}
        />
      </AttachmentLightbox>
    </>
  );
}

function PdfGlyph() {
  return (
    <svg
      width="11"
      height="11"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
      className="shrink-0 opacity-70"
    >
      <path
        d="M4 2h5l3 3v9a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1Z"
        stroke="currentColor"
        strokeWidth="1.3"
      />
      <path d="M9 2v3h3" stroke="currentColor" strokeWidth="1.3" />
      <path
        d="M5.5 9.5h1m1 0h1m-3 2h2"
        stroke="currentColor"
        strokeWidth="1.1"
        strokeLinecap="round"
      />
    </svg>
  );
}

// Local getTenantSlug() removed — auth-header construction now goes
// through platformAuthHeaders() from @/lib/api (#178).
