"use client";

// AttachmentAudio — inline native HTML5 <audio controls> player for
// chat attachments (RFC #2991, PR-2).
//
// Same auth + Blob-URL pattern as AttachmentImage / AttachmentVideo.
// Native audio control bar handles play/pause/scrub/volume/download,
// and there's no fullscreen UI to worry about (audio doesn't need
// AttachmentLightbox).

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

export function AttachmentAudio({ workspaceId, attachment, onDownload, tone }: Props) {
  const [state, setState] = useState<FetchState>({ kind: "idle" });
  const blobUrlRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });

    if (!isPlatformAttachment(attachment.uri)) {
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
        style={{ width: 280, height: 40 }}
        aria-label={`Loading ${attachment.name}`}
      />
    );
  }

  return (
    <div
      className={`inline-flex flex-col gap-1 rounded-md border px-2 py-1 ${
        tone === "user" ? "border-blue-400/30 bg-accent-strong/10" : "border-line/50 bg-surface-card/40"
      }`}
    >
      {/* Filename label so the user knows what they're hearing
          before pressing play. Short, single-line, truncated. */}
      <span className="text-[10px] text-ink-mid truncate max-w-[280px]" title={attachment.name}>
        {attachment.name}
      </span>
      <audio
        controls
        preload="metadata"
        src={state.src}
        style={{ width: 280, height: 32 }}
        onError={() => setState({ kind: "error" })}
      >
        {attachment.name}
      </audio>
    </div>
  );
}

// Local getTenantSlug() removed — auth-header construction now goes
// through platformAuthHeaders() from @/lib/api (#178).
