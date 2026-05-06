"use client";

// AttachmentPreview — the SSOT dispatch point for chat-attachment
// rendering (RFC #2991, PR-1).
//
// Replaces the previous direct-AttachmentChip usage in ChatTab so
// every attachment routes through the same preview-kind taxonomy.
// Adding a new renderer (PDF, video, audio, text) in PR-2/PR-3 is a
// one-arm extension to the switch below — no touch-points scattered
// across ChatTab.tsx, AgentCommsPanel.tsx, or other chat consumers.
//
// Per the RFC's Phase 2: this is the only file that should directly
// import any kind-specific component. ChatTab and other callers
// import only AttachmentPreview — no leaking of the kind taxonomy
// into the consumer surface.

import type { ChatAttachment } from "./types";
import { getAttachmentPreviewKind } from "./preview-kind";
import { AttachmentImage } from "./AttachmentImage";
import { AttachmentVideo } from "./AttachmentVideo";
import { AttachmentAudio } from "./AttachmentAudio";
import { AttachmentPDF } from "./AttachmentPDF";
import { AttachmentTextPreview } from "./AttachmentTextPreview";
import { AttachmentChip } from "./AttachmentViews";

interface Props {
  workspaceId: string;
  attachment: ChatAttachment;
  /** Caller's download handler — used for the kind=file fallback
   *  and as the kind-specific renderers' fallback when their own
   *  preview fails (e.g. image fetch errored). */
  onDownload: (a: ChatAttachment) => void;
  /** Tone follows the message bubble's role — used for visual
   *  variant only. */
  tone: "user" | "agent";
}

export function AttachmentPreview({ workspaceId, attachment, onDownload, tone }: Props) {
  const kind = getAttachmentPreviewKind(attachment.mimeType, attachment.uri, attachment.name);
  switch (kind) {
    case "image":
      return (
        <AttachmentImage
          workspaceId={workspaceId}
          attachment={attachment}
          onDownload={onDownload}
          tone={tone}
        />
      );
    case "video":
      return (
        <AttachmentVideo
          workspaceId={workspaceId}
          attachment={attachment}
          onDownload={onDownload}
          tone={tone}
        />
      );
    case "audio":
      return (
        <AttachmentAudio
          workspaceId={workspaceId}
          attachment={attachment}
          onDownload={onDownload}
          tone={tone}
        />
      );
    case "pdf":
      return (
        <AttachmentPDF
          workspaceId={workspaceId}
          attachment={attachment}
          onDownload={onDownload}
          tone={tone}
        />
      );
    case "text":
      return (
        <AttachmentTextPreview
          workspaceId={workspaceId}
          attachment={attachment}
          onDownload={onDownload}
          tone={tone}
        />
      );
    case "file":
    default:
      return <AttachmentChip attachment={attachment} onDownload={onDownload} tone={tone} />;
  }
}
