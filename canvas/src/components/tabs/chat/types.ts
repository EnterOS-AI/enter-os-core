/** One file attached to a chat message. Shared shape for both
 *  directions: when a user attaches a file the UI uploads it and
 *  stashes the returned metadata here; when an agent returns a
 *  `kind: file` part in an A2A response, the parser populates the
 *  same fields. `uri` uses the `workspace:<abs-path>` scheme the
 *  server returns — the renderer translates that to a download
 *  request against GET /workspaces/:id/chat/download. */
export interface ChatAttachment {
  name: string;
  uri: string;
  mimeType?: string;
  size?: number;
}

export interface ChatMessage {
  id: string;
  role: "user" | "agent" | "system";
  content: string;
  /** Attachments sent with or returned alongside this message. */
  attachments?: ChatAttachment[];
  timestamp: string; // ISO string for serialization
}

export function createMessage(
  role: ChatMessage["role"],
  content: string,
  attachments?: ChatAttachment[],
): ChatMessage {
  return {
    id: crypto.randomUUID(),
    role,
    content,
    ...(attachments && attachments.length > 0 ? { attachments } : {}),
    timestamp: new Date().toISOString(),
  };
}

// appendMessageDeduped adds a ChatMessage to `prev` unless the tail
// already contains the same (role, content) from within
// dedupeWindowMs. Collapses the case where two delivery paths race to
// render the same agent reply — e.g. the HTTP .then() handler for
// POST /a2a AND a `send_message_to_user` WebSocket push from the
// runtime, both carrying the same text. Without this guard the user
// sees two or three identical bubbles with identical timestamps.
//
// Why a time-windowed check instead of dedupe-by-id: the three delivery
// paths (HTTP response, WS A2A_RESPONSE, WS send_message_to_user) each
// mint a fresh `createMessage` with a random UUID client-side — there's
// no stable end-to-end message id yet. Content+role+time is the
// pragmatic identity. The window is short (3s) so genuine repeat
// messages ("hi", "hi") from a real user/agent still render.
export function appendMessageDeduped(prev: ChatMessage[], msg: ChatMessage, dedupeWindowMs = 3000): ChatMessage[] {
  const cutoff = Date.now() - dedupeWindowMs;
  const sig = attachmentSignature(msg.attachments);
  const alreadyThere = prev.some((m) => {
    if (m.role !== msg.role || m.content !== msg.content) return false;
    // Attachments participate in the dedupe key so a text-only push
    // doesn't shadow the file-carrying HTTP response (and vice versa).
    // When both carry the same text AND the same files, collapse.
    if (attachmentSignature(m.attachments) !== sig) return false;
    const t = Date.parse(m.timestamp);
    return !Number.isNaN(t) && t >= cutoff;
  });
  if (alreadyThere) return prev;
  return [...prev, msg];
}

function attachmentSignature(atts: ChatAttachment[] | undefined): string {
  if (!atts || atts.length === 0) return "";
  // URI is the stable identity — name can differ across delivery
  // paths (agent vs our parser's basename fallback).
  return atts.map((a) => a.uri).sort().join("|");
}
