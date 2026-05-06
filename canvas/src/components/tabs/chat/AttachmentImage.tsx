"use client";

// AttachmentImage — inline image thumbnail + click-to-fullscreen.
// First "specialized renderer" landing under RFC #2991 PR-1.
//
// Auth model
// ----------
//
// The Critical UX/Security trade-off (per RFC's hostile-self-review
// item #2): the bytes live behind workspace auth. A bare
// <img src="https://reno-stars.../chat/download?path=…"> WILL NOT
// include our cookie + Origin headers when the browser loads it —
// even for same-origin canvas-server, the auth chain (cookie + token
// + X-Molecule-Org-Slug header) is JS-injected, not browser-default.
//
// Solution: same auth path the chip download uses. Fetch the bytes
// with the JS auth headers, wrap in a Blob, hand the browser an
// ObjectURL. The image renders from local memory; no second request,
// no auth leakage, no CORS pain.
//
// That same blob URL is what the lightbox shows on click — single
// fetch, cached for the lifetime of the message bubble.
//
// Failure modes
// -------------
//
// - Fetch fails (404, 403, network) → fall back to AttachmentChip
//   (the existing file-pill download flow). The user still gets a
//   working download; we just lose the inline preview.
// - Decoded as non-image (server returned wrong Content-Type, or
//   bytes are corrupt) → onError handler swaps to AttachmentChip.
// - Bytes too large — no enforcement here; the server caps at 25MB
//   per file (chat_files.go), which is too big for a thumbnail but
//   acceptable for a chat-attached image. If we hit pain we can
//   downscale via canvas, but defer that to v2.

import { useState, useEffect, useRef } from "react";
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

export function AttachmentImage({ workspaceId, attachment, onDownload, tone }: Props) {
  const [state, setState] = useState<FetchState>({ kind: "idle" });
  const [open, setOpen] = useState(false);
  // Track whether we created the ObjectURL so cleanup runs on the
  // exact value we minted (state could change between effect setup
  // and effect cleanup if a new fetch fires).
  const blobUrlRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });

    // For non-platform URIs (http/https external image hosts) we can
    // skip the auth fetch — browser loads them directly. We bail out
    // of the auth-fetch flow and use the raw URL via resolveAttachmentHref.
    if (!isPlatformAttachment(attachment.uri)) {
      const href = resolveAttachmentHref(workspaceId, attachment.uri);
      if (!cancelled) setState({ kind: "ready", blobUrl: href });
      return;
    }

    // Platform-auth path: identical to downloadChatFile but we keep
    // the blob (don't trigger a Save-As). Use the same headers it does
    // by going through it indirectly — no, downloadChatFile triggers a
    // Save-As. Need a separate fetch.
    void (async () => {
      try {
        const href = resolveAttachmentHref(workspaceId, attachment.uri);
        const headers: Record<string, string> = {};
        // Read the same env var downloadChatFile reads — single source
        // of truth would be cleaner; refactor opportunity for PR-2 if
        // we add the same path to AttachmentVideo.
        const adminToken = process.env.NEXT_PUBLIC_ADMIN_TOKEN;
        if (adminToken) headers["Authorization"] = `Bearer ${adminToken}`;
        const slug = getTenantSlug();
        if (slug) headers["X-Molecule-Org-Slug"] = slug;
        const res = await fetch(href, {
          headers,
          credentials: "include",
          signal: AbortSignal.timeout(30_000),
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
      // Free the ObjectURL when the bubble unmounts — keeps memory
      // bounded across long chat histories.
      if (blobUrlRef.current) {
        URL.revokeObjectURL(blobUrlRef.current);
        blobUrlRef.current = null;
      }
    };
  }, [workspaceId, attachment.uri]);

  // Failure → render the existing file chip. Maintains the download
  // affordance even if preview fails; the user never gets stuck.
  if (state.kind === "error") {
    return <AttachmentChip attachment={attachment} onDownload={onDownload} tone={tone} />;
  }

  // Loading → small placeholder pill so the bubble doesn't reflow
  // when the image lands. Sized to roughly the thumbnail's aspect
  // ratio guess (a 240x180 box) so the layout is stable.
  if (state.kind === "loading" || state.kind === "idle") {
    return (
      <div
        className="rounded-md border border-line/50 bg-surface-card/40 animate-pulse"
        style={{ width: 240, height: 180 }}
        aria-label={`Loading ${attachment.name}`}
      />
    );
  }

  // Ready → inline thumbnail with click handler. The img has its
  // own onError so a corrupt blob (server returned the right size
  // but invalid bytes) falls through to the chip too.
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        title={`Preview ${attachment.name}`}
        className={`group relative inline-block max-w-full rounded-lg overflow-hidden border focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 ${
          tone === "user" ? "border-blue-400/30" : "border-line/50"
        }`}
        aria-label={`Open ${attachment.name} preview`}
      >
        <img
          src={state.blobUrl}
          alt={attachment.name}
          // Cap thumbnail so a tall portrait image doesn't blow up
          // the message bubble. The lightbox shows the full size.
          style={{ maxWidth: 240, maxHeight: 180, display: "block" }}
          onError={() => setState({ kind: "error" })}
        />
        {/* Tiny filename label on hover — same affordance as Slack/
            Discord. Helps when several images land in one bubble. */}
        <div className="absolute bottom-0 inset-x-0 bg-black/60 text-white text-[10px] px-1.5 py-0.5 truncate opacity-0 group-hover:opacity-100 transition-opacity">
          {attachment.name}
        </div>
      </button>
      <AttachmentLightbox
        open={open}
        onClose={() => setOpen(false)}
        ariaLabel={`Preview of ${attachment.name}`}
      >
        <img
          src={state.blobUrl}
          alt={attachment.name}
          className="max-w-[95vw] max-h-[90vh] object-contain"
        />
      </AttachmentLightbox>
    </>
  );
}

// Internal helper — duplicated from uploads.ts (it's not exported
// there). Kept local so this component doesn't reach into private
// surface; if AttachmentVideo / AttachmentPDF in PR-2/PR-3 also need
// it, lift to an exported helper at that point (the third-caller
// rule).
function getTenantSlug(): string | null {
  if (typeof window === "undefined") return null;
  const host = window.location.hostname;
  // Tenant subdomain shape: <slug>.moleculesai.app
  const m = host.match(/^([^.]+)\.moleculesai\.app$/);
  return m ? m[1] : null;
}
