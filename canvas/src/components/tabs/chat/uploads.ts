import { PLATFORM_URL, platformAuthHeaders } from "@/lib/api";
import type { ChatAttachment } from "./types";

/** Chat attachments are intentionally uploaded via a direct fetch()
 *  instead of the `api.post` helper — `api.post` JSON-stringifies the
 *  body, which would 500 on a Blob. Auth headers (tenant slug, admin
 *  token, credentials) come from `platformAuthHeaders()` — the same
 *  helper `request()` uses, so a missing bearer surfaces as a single
 *  fix site instead of N copies. We deliberately do NOT set
 *  Content-Type so the browser writes the multipart boundary into the
 *  header; setting it manually would yield a multipart body the server
 *  can't parse. See lib/api.ts platformAuthHeaders() for the full
 *  rationale on why this pair must stay matched. */
export async function uploadChatFiles(
  workspaceId: string,
  files: File[],
): Promise<ChatAttachment[]> {
  if (files.length === 0) return [];

  const form = new FormData();
  for (const f of files) form.append("files", f, f.name);

  // Uploads legitimately take a while on cold cache (tar write +
  // docker cp into the container). 60s is comfortable for the 25MB/
  // 50MB caps the server enforces.
  const res = await fetch(`${PLATFORM_URL}/workspaces/${workspaceId}/chat/uploads`, {
    method: "POST",
    headers: platformAuthHeaders(),
    body: form,
    credentials: "include",
    signal: AbortSignal.timeout(60_000),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`upload failed: ${res.status} ${text}`);
  }
  const json = (await res.json()) as { files: ChatAttachment[] };
  return json.files ?? [];
}

/** Resolve a file URI into a browser-downloadable URL. Accepts:
 *    - `workspace:<abs-path>` (our canonical form)
 *    - `file:///workspace/...` (some agents emit this)
 *    - `/workspace/...` (bare absolute path inside the container)
 *    - `platform-pending:<wsid>/<file_id>` (poll-mode upload, staged
 *      on platform side; resolves to /pending-uploads/<file_id>/content)
 *  Everything that looks like an allowed-root container path is
 *  rewritten to the authenticated /chat/download endpoint. HTTP(S)
 *  URIs pass through unchanged so we can also render links to
 *  artefacts hosted off-platform. Unknown schemes fall back to the
 *  raw URI — the caller gets to decide how to render it. */
export function resolveAttachmentHref(
  workspaceId: string,
  uri: string,
): string {
  // platform-pending: agents-emitted URI that lives in the platform-side
  // staging layer (poll-mode chat uploads, see workspace-server's
  // chat_files.go ~line 690 + pendinguploads.Storage). The wire shape
  // is `platform-pending:<workspace_id>/<file_id>`. Resolving it
  // requires hitting GET /workspaces/<wsid>/pending-uploads/<file_id>/content
  // which streams the bytes with full workspace auth. Without this
  // case the browser sees an unhandled-protocol click → about:blank,
  // which was the user-visible bug from 2026-05-05 (reno-stars).
  if (uri.startsWith("platform-pending:")) {
    const rest = uri.slice("platform-pending:".length);
    const slash = rest.indexOf("/");
    // Defensive: if the URI doesn't have the expected wsid/fileid
    // shape, fall through to raw-URI handling so the consumer can
    // still try to render it (rather than producing a broken /pending-
    // uploads/// path).
    if (slash > 0) {
      const wsid = rest.slice(0, slash);
      const fileID = rest.slice(slash + 1);
      if (wsid && fileID) {
        // Use the URI's own workspace_id (the bytes live in THAT
        // workspace's pending-uploads store), not the chat's
        // workspace_id — these CAN differ when a user drags a file
        // into one workspace's chat that gets forwarded to another
        // (cross-workspace delegation, agent forwarding).
        return `${PLATFORM_URL}/workspaces/${wsid}/pending-uploads/${fileID}/content`;
      }
    }
    return uri;
  }
  const containerPath = normalizeWorkspaceUri(uri);
  if (containerPath) {
    return `${PLATFORM_URL}/workspaces/${workspaceId}/chat/download?path=${encodeURIComponent(containerPath)}`;
  }
  return uri;
}

/** Returns true when the URI points at a platform-side resource that
 *  requires our auth headers — caller should route through
 *  downloadChatFile rather than letting the browser navigate. */
export function isPlatformAttachment(uri: string): boolean {
  if (uri.startsWith("platform-pending:")) return true;
  return normalizeWorkspaceUri(uri) !== null;
}

/** Extracts the absolute container path from a workspace-scoped URI,
 *  or null if the URI isn't a container path. The matching roots
 *  mirror the server's `allowedRoots` allowlist. */
const ALLOWED_CONTAINER_ROOTS = ["/configs", "/workspace", "/home", "/plugins"];

function normalizeWorkspaceUri(uri: string): string | null {
  let path: string | null = null;
  if (uri.startsWith("workspace:")) {
    path = uri.slice("workspace:".length);
  } else if (uri.startsWith("file:///")) {
    path = uri.slice("file://".length); // keep the leading slash
  } else if (uri.startsWith("/")) {
    path = uri;
  }
  if (!path) return null;
  // Only rewrite when the path lands in an allowed root; otherwise
  // return null so the caller falls through to raw-URI handling
  // (which will open a new tab for HTTP-ish schemes).
  for (const root of ALLOWED_CONTAINER_ROOTS) {
    if (path === root || path.startsWith(root + "/")) return path;
  }
  return null;
}

/** Trigger a browser download for an attachment. Uses fetch+blob
 *  rather than an anchor navigation because the download endpoint
 *  requires workspace auth — and the browser won't attach
 *  `Authorization: Bearer` or `X-Molecule-Org-Slug` to a bare anchor
 *  click. A 25MB per-file cap server-side keeps the blob buffer
 *  bounded. HTTP(S) URIs skip the fetch path and open directly
 *  since they're off-platform artefacts that we don't own auth for. */
export async function downloadChatFile(
  workspaceId: string,
  attachment: ChatAttachment,
): Promise<void> {
  const href = resolveAttachmentHref(workspaceId, attachment.uri);
  if (!isPlatformAttachment(attachment.uri)) {
    // External URL — let the browser navigate. Opens in new tab so
    // the canvas context survives a navigation. `href` here is the
    // raw URI (http(s), or anything else the agent sent back).
    window.open(href, "_blank", "noopener,noreferrer");
    return;
  }

  const res = await fetch(href, {
    headers: platformAuthHeaders(),
    credentials: "include",
    signal: AbortSignal.timeout(60_000),
  });
  if (!res.ok) {
    throw new Error(`download failed: ${res.status}`);
  }
  const blob = await res.blob();
  // Revoke the object URL after the click — browsers hold the blob
  // until the URL is either revoked or the document unloads. 30s is
  // plenty of headroom for the click → save dialog round-trip.
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = attachment.name;
  a.rel = "noopener";
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}
