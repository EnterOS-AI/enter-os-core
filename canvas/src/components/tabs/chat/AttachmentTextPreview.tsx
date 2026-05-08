"use client";

// AttachmentTextPreview — inline preview for text/code/JSON/YAML/etc
// (RFC #2991, PR-3).
//
// Shape: render first N lines (~10) in monospace inside the bubble.
// Click "Show more" to expand fully; the lightbox is reserved for
// image/PDF where viewport-size matters. For text, the bubble itself
// can host the full content.
//
// Why no syntax highlighting (yet):
//
//   - Pulling in shiki / highlight.js / prism adds 200-500KB to the
//     bundle for a feature that's nice-to-have. MVP uses plain
//     <pre><code>.
//   - Future: lazy-load shiki on first text-attachment render. v2
//     if the user reports the gap.
//
// Auth: same fetch+text() pattern as image/video/audio, but we read
// the text directly instead of building a Blob URL — no <img>/<video>
// element to feed.
//
// Memory: text files are usually small. We cap the preview at 256 KB
// fetched (large logs would otherwise crash the bubble). If the file
// exceeds the cap, we show what we got + a "truncated" note + a chip
// to download the full file.

import { useState, useEffect } from "react";
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
  | { kind: "ready"; text: string; truncated: boolean }
  | { kind: "error" };

const PREVIEW_LINE_COUNT = 10;
const MAX_FETCH_BYTES = 256 * 1024; // 256 KB

export function AttachmentTextPreview({ workspaceId, attachment, onDownload, tone }: Props) {
  const [state, setState] = useState<FetchState>({ kind: "idle" });
  const [expanded, setExpanded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });

    void (async () => {
      try {
        const href = resolveAttachmentHref(workspaceId, attachment.uri);
        // Only attach platform auth headers for in-platform URIs —
        // off-platform URLs (HTTP/HTTPS attachments) MUST NOT receive
        // our bearer token (it would leak the admin token to a third
        // party). The branch is preserved with the new shared helper.
        const headers: Record<string, string> = isPlatformAttachment(attachment.uri)
          ? platformAuthHeaders()
          : {};
        const res = await fetch(href, {
          headers,
          credentials: "include",
          signal: AbortSignal.timeout(30_000),
        });
        if (!res.ok) {
          if (!cancelled) setState({ kind: "error" });
          return;
        }
        // Read up to MAX_FETCH_BYTES. Use the standard ReadableStream
        // path so we don't materialise a 100MB log into memory.
        const reader = res.body?.getReader();
        if (!reader) {
          // Fallback: small text file, just .text() it.
          const text = await res.text();
          if (cancelled) return;
          setState({
            kind: "ready",
            text: text.slice(0, MAX_FETCH_BYTES),
            truncated: text.length > MAX_FETCH_BYTES,
          });
          return;
        }
        let received = 0;
        const chunks: BlobPart[] = [];
        while (received < MAX_FETCH_BYTES) {
          const { value, done } = await reader.read();
          if (done) break;
          // Copy into a fresh ArrayBuffer-backed view — TS in lib.dom
          // 2026 narrows BlobPart away from SharedArrayBuffer-backed
          // Uint8Arrays. Blob() accepts the copy fine at runtime.
          const copy = new Uint8Array(value.byteLength);
          copy.set(value);
          chunks.push(copy.buffer);
          received += value.byteLength;
        }
        // If we hit the cap but the stream isn't done, mark truncated.
        const truncated = received >= MAX_FETCH_BYTES;
        if (truncated) reader.cancel();
        const blob = new Blob(chunks);
        const text = await blob.text();
        if (cancelled) return;
        setState({ kind: "ready", text, truncated });
      } catch {
        if (!cancelled) setState({ kind: "error" });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceId, attachment.uri]);

  if (state.kind === "error") {
    return <AttachmentChip attachment={attachment} onDownload={onDownload} tone={tone} />;
  }
  if (state.kind === "idle" || state.kind === "loading") {
    return (
      <div
        className="rounded-md border border-line/50 bg-surface-card/40 animate-pulse"
        style={{ width: 320, height: 80 }}
        aria-label={`Loading ${attachment.name}`}
      />
    );
  }

  const lines = state.text.split("\n");
  const preview = expanded ? state.text : lines.slice(0, PREVIEW_LINE_COUNT).join("\n");
  const showExpandButton = !expanded && lines.length > PREVIEW_LINE_COUNT;

  return (
    <div
      className={`inline-block max-w-full rounded-md border ${
        tone === "user" ? "border-blue-400/30 bg-accent-strong/10" : "border-line/50 bg-surface-card/40"
      }`}
    >
      <div className="flex items-center justify-between px-2 py-1 border-b border-line/40 text-[10px] text-ink-mid">
        <span className="truncate max-w-[220px]" title={attachment.name}>
          {attachment.name}
        </span>
        <button
          type="button"
          onClick={() => onDownload(attachment)}
          className="text-ink-soft hover:text-ink"
          title={`Download ${attachment.name}`}
          aria-label={`Download ${attachment.name}`}
        >
          ⬇
        </button>
      </div>
      <pre className="overflow-x-auto px-2 py-1.5 text-[10px] leading-snug text-ink whitespace-pre font-mono max-w-[480px] max-h-[300px]">
        <code>{preview}</code>
      </pre>
      {showExpandButton && (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className="block w-full text-center text-[10px] text-ink-mid hover:text-ink py-1 border-t border-line/40"
        >
          Show all {lines.length} lines
        </button>
      )}
      {state.truncated && (
        <div className="px-2 py-1 text-[10px] text-warm border-t border-line/40">
          Preview truncated at {Math.round(MAX_FETCH_BYTES / 1024)} KB —{" "}
          <button
            type="button"
            onClick={() => onDownload(attachment)}
            className="underline"
          >
            download full file
          </button>
        </div>
      )}
    </div>
  );
}

// Local getTenantSlug() removed — auth-header construction now goes
// through platformAuthHeaders() from @/lib/api (#178).
