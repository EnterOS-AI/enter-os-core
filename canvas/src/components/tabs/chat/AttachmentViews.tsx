"use client";

// Small presentational components for chat attachments. Kept in a
// separate file so ChatTab.tsx stays focused on state + send/receive
// orchestration. Both variants share the file-icon + name + size
// layout; the only difference is the trailing action (remove for
// pending, download for completed).

import type { ChatAttachment } from "./types";

function formatSize(bytes: number | undefined): string {
  if (bytes == null) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Inline pill for a file that the user has picked but not yet sent.
 *  Renders above the textarea; clicking × pops it from the pending
 *  list without uploading. */
export function PendingAttachmentPill({
  file,
  onRemove,
}: {
  file: File;
  onRemove: () => void;
}) {
  return (
    <div className="flex items-center gap-1.5 rounded-md border border-line/60 bg-surface-card/80 px-2 py-1 text-[10px] text-ink-mid max-w-[200px]">
      <FileGlyph className="text-ink-mid shrink-0" />
      <span className="truncate" title={file.name}>{file.name}</span>
      <span className="text-ink-soft shrink-0 tabular-nums">{formatSize(file.size)}</span>
      <button
        onClick={onRemove}
        aria-label={`Remove ${file.name}`}
        className="ml-0.5 text-ink-soft hover:text-ink transition-colors shrink-0"
      >
        <svg width="10" height="10" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        </svg>
      </button>
    </div>
  );
}

/** Chip rendered inside a message bubble for a sent/received file.
 *  Clicking triggers the download via the passed onDownload callback
 *  so the parent controls workspace-scoped URL resolution. */
export function AttachmentChip({
  attachment,
  onDownload,
  tone,
}: {
  attachment: ChatAttachment;
  onDownload: (a: ChatAttachment) => void;
  tone: "user" | "agent";
}) {
  const toneClasses =
    tone === "user"
      ? "border-blue-400/30 bg-accent-strong/20 hover:bg-accent-strong/30 text-blue-100"
      : "border-line/50 bg-surface-card/40 hover:bg-surface-card/50 text-ink";
  return (
    <button
      onClick={() => onDownload(attachment)}
      title={`Download ${attachment.name}`}
      className={`flex items-center gap-1.5 rounded-md border px-2 py-1 text-[10px] transition-colors max-w-full ${toneClasses}`}
    >
      <FileGlyph className="shrink-0 opacity-70" />
      <span className="truncate">{attachment.name}</span>
      {attachment.size != null && (
        <span className="opacity-60 shrink-0 tabular-nums">{formatSize(attachment.size)}</span>
      )}
      <DownloadGlyph className="opacity-70 shrink-0" />
    </button>
  );
}

function FileGlyph({ className }: { className?: string }) {
  return (
    <svg width="10" height="10" viewBox="0 0 16 16" fill="none" className={className} aria-hidden="true">
      <path d="M4 2h5l3 3v9a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1Z" stroke="currentColor" strokeWidth="1.3" strokeLinejoin="round" />
      <path d="M9 2v3h3" stroke="currentColor" strokeWidth="1.3" strokeLinejoin="round" />
    </svg>
  );
}

function DownloadGlyph({ className }: { className?: string }) {
  return (
    <svg width="10" height="10" viewBox="0 0 16 16" fill="none" className={className} aria-hidden="true">
      <path d="M8 2v9M4 7l4 4 4-4" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M3 13h10" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
    </svg>
  );
}
