"use client";

// AttachmentVideo — inline native HTML5 <video controls> player for
// chat attachments (RFC #2991, PR-2).
//
// Why HTML5-native (vs custom JS player):
//
//   - Browser vendors ship hardware-accelerated decoders, captions,
//     and fullscreen UI. We get all of it for free.
//   - Native fullscreen via the <video> element's built-in button
//     (no AttachmentLightbox needed for video — the browser does it).
//   - Mobile-friendly: iOS / Android Safari + Chrome handle the
//     pinch + scrub UX the user already knows.
//
// Auth model — identical to AttachmentImage:
// platform-auth URIs need our cookie/token, so we fetch the bytes,
// wrap in a Blob, hand the browser an ObjectURL via <video src=>.
// External (http/https) URIs skip the fetch and use the raw URL.
//
// Memory caveat: a Blob holds the entire video in JS memory until
// the bubble unmounts. For multi-hundred-MB videos this is bad. The
// server caps single-file uploads at 25MB (chat_files.go), so we're
// bounded; if larger files become a real shape, switch to streaming
// via MediaSource or just `<video src=…>` with a credentials-aware
// fetch via service worker. v2 if measured-needed.

import { useState, useEffect, useRef } from "react";
import { platformAuthHeaders } from "@/lib/api";
import type { ChatAttachment } from "./types";
import { isPlatformAttachment, resolveAttachmentHref } from "./uploads";
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
  | { kind: "ready"; src: string }
  | { kind: "error" };

export function AttachmentVideo({ workspaceId, attachment, onDownload, tone }: Props) {
  const [state, setState] = useState<FetchState>({ kind: "idle" });
  const blobUrlRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });

    if (!isPlatformAttachment(attachment.uri)) {
      // External video (http/https) — let the browser stream it
      // natively without the JS-blob detour.
      const href = resolveAttachmentHref(workspaceId, attachment.uri);
      if (!cancelled) setState({ kind: "ready", src: href });
      return;
    }

    void (async () => {
      try {
        const href = resolveAttachmentHref(workspaceId, attachment.uri);
        const res = await fetch(href, {
          headers: platformAuthHeaders(),
          credentials: "include",
          // Videos are larger than images on average; give the request
          // more headroom. The server's per-request body cap (50MB) is
          // still the actual ceiling.
          signal: AbortSignal.timeout(120_000),
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
        setState({ kind: "ready", src: url });
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
        className="rounded-md border border-line/50 bg-surface-card/40 animate-pulse"
        style={{ width: 320, height: 180 }}
        aria-label={`Loading ${attachment.name}`}
      />
    );
  }

  return (
    <div
      className={`inline-block rounded-lg overflow-hidden border ${
        tone === "user" ? "border-blue-400/30" : "border-line/50"
      }`}
    >
      <video
        controls
        // preload="metadata" so the browser fetches just enough to
        // show duration + first frame thumbnail without streaming
        // the whole file before the user clicks play.
        preload="metadata"
        // playsInline keeps mobile Safari from auto-fullscreening
        // on play; the user can still hit the native fullscreen
        // button (or PiP on Chrome) if they want.
        playsInline
        // Native fullscreen via the <video> control bar; no
        // AttachmentLightbox needed for video.
        src={state.src}
        // Cap thumbnail / inline display so the bubble doesn't blow
        // up vertical layout for tall portrait clips. The native
        // fullscreen button uses the original aspect ratio.
        style={{ maxWidth: 320, maxHeight: 240, display: "block" }}
        // Bytes that aren't actually a valid video (corrupt blob,
        // wrong Content-Type) fail load → swap to chip.
        onError={() => setState({ kind: "error" })}
      >
        <track kind="captions" />
        {attachment.name}
      </video>
    </div>
  );
}

// Local getTenantSlug() removed — auth-header construction now goes
// through platformAuthHeaders() from @/lib/api (#178).
