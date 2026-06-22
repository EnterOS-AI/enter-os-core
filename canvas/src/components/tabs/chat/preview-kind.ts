// preview-kind.ts — single source of truth for "what renderer should
// this attachment use" (RFC #2991, PR-1).
//
// Per the RFC's Phase 2 design, MIME type is the dispatch axis. The
// wire shape (ChatAttachment.mimeType) already carries it end-to-end
// from the server's chat_files.go through agent_message_writer.go to
// the canvas hydrater — we just need to map it to a render kind.
//
// Why a separate file from AttachmentPreview.tsx: the kind helper is
// a pure function that's easier to unit-test in isolation than a
// React component, and unit tests across MIME families are the
// regression line for new types added later.

/** The render-kind taxonomy. Each kind has a dedicated component:
 *
 *    image  → AttachmentImage (inline thumbnail + click → lightbox)
 *    video  → AttachmentVideo (HTML5 <video controls>, native fullscreen)
 *    audio  → AttachmentAudio (HTML5 <audio controls>)
 *    pdf    → AttachmentPDF (browser-native <embed>, fullscreen modal)
 *    text   → AttachmentTextPreview (monospace, first N lines, expand)
 *    file   → AttachmentChip (existing fallback — generic file pill)
 *
 * NB: `text` includes JSON, YAML, source code, plain text — anything
 * that renders sensibly as preformatted ASCII without a specialized
 * viewer. PR-1 ships only `image` + `file`; PR-2 adds video/audio;
 * PR-3 adds pdf + text. All routed through this same dispatch table
 * so adding a new kind is a one-line registration. */
export type AttachmentPreviewKind = "image" | "video" | "audio" | "pdf" | "text" | "file";

/** Maps a MIME type to the render kind. Falls back to "file" for
 *  any MIME we don't have a renderer for (current behavior — the
 *  attachment chip is the universal fallback).
 *
 *  Filename-based fallback: when mimeType is missing or generic
 *  (application/octet-stream), inspect the URI's extension. The
 *  workspace-server's chat_files.go derives Content-Type from the
 *  file extension, but agent-emitted attachments may not always
 *  set mimeType, and the canvas should still preview a file named
 *  `screenshot.png` even if the wire shape lacks the MIME.
 *
 *  Strict MIME match always wins; extension fallback only applies
 *  to empty / generic. Unknown extension → "file". */
export function getAttachmentPreviewKind(
  mimeType: string | undefined,
  uri?: string,
  name?: string,
): AttachmentPreviewKind {
  const mime = (mimeType ?? "").toLowerCase().trim();

  // Strict MIME match (preferred — set by server's Content-Type
  // detection or by the agent's explicit mimeType field).
  if (mime.startsWith("image/")) return "image";
  if (mime.startsWith("video/")) return "video";
  if (mime.startsWith("audio/")) return "audio";
  if (mime === "application/pdf") return "pdf";
  if (
    mime.startsWith("text/") ||
    mime === "application/json" ||
    mime === "application/yaml" ||
    mime === "application/x-yaml" ||
    mime === "application/javascript" ||
    mime === "application/typescript"
  ) {
    return "text";
  }

  // Extension-based fallback — only when MIME is missing or
  // application/octet-stream (the server's "I don't know" default).
  // Skip when MIME is set to something specific we just don't have
  // a renderer for (e.g. application/zip → file is correct).
  const looksGeneric = mime === "" || mime === "application/octet-stream";
  if (looksGeneric) {
    const ext = extractExtension(uri, name);
    if (ext) {
      const kind = EXTENSION_KIND.get(ext);
      if (kind) return kind;
    }
  }

  return "file";
}

// Extension → kind table for the fallback branch. Keep this list
// short and curated — every entry is a UX commitment to render
// inline, and a wrong inference (e.g. .doc rendered as text) is
// worse than the generic file chip.
const EXTENSION_KIND: ReadonlyMap<string, AttachmentPreviewKind> = new Map([
  // Images
  ["png", "image"],
  ["jpg", "image"],
  ["jpeg", "image"],
  ["gif", "image"],
  ["webp", "image"],
  ["svg", "image"],
  ["avif", "image"],
  ["bmp", "image"],
  // Video
  ["mp4", "video"],
  ["webm", "video"],
  ["mov", "video"],
  ["mkv", "video"],
  // Audio
  ["mp3", "audio"],
  ["wav", "audio"],
  ["ogg", "audio"],
  ["m4a", "audio"],
  ["flac", "audio"],
  // PDF
  ["pdf", "pdf"],
  // Text-ish (rendered as preformatted ASCII)
  ["txt", "text"],
  ["md", "text"],
  ["json", "text"],
  ["yaml", "text"],
  ["yml", "text"],
  ["js", "text"],
  ["ts", "text"],
  ["tsx", "text"],
  ["jsx", "text"],
  ["py", "text"],
  ["go", "text"],
  ["rs", "text"],
  ["java", "text"],
  ["c", "text"],
  ["cpp", "text"],
  ["h", "text"],
  ["hpp", "text"],
  ["sh", "text"],
  ["bash", "text"],
  ["html", "text"],
  ["css", "text"],
  ["sql", "text"],
  ["toml", "text"],
  ["ini", "text"],
  ["xml", "text"],
  ["csv", "text"],
  ["log", "text"],
]);

/** Extracts the lowercased extension from a uri or name, without
 *  the leading dot. Returns "" when no extension is present. */
function extractExtension(uri: string | undefined, name: string | undefined): string {
  // Prefer name (always a leaf path); fall back to uri's last
  // segment. Strip query string + fragment so a URI like
  // "https://example.com/foo.png?download=1" still parses as png.
  const candidate = name || uri || "";
  if (!candidate) return "";
  let leaf = candidate.split(/[\\/]/).pop() || "";
  // Drop ?query and #fragment.
  leaf = leaf.split(/[?#]/)[0];
  const dot = leaf.lastIndexOf(".");
  if (dot < 0 || dot === leaf.length - 1) return "";
  return leaf.slice(dot + 1).toLowerCase();
}
