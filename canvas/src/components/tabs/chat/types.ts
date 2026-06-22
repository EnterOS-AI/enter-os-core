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

/** One tool-use step the agent ran during a turn — the persisted twin
 *  of the live progress lines. Server stores these in the activity
 *  row's tool_trace; the chat-history endpoint returns them on the
 *  agent message so the chain survives a reload (core#2636). */
export interface ToolTraceEntry {
  tool: string;
  input?: string;
}

export interface ChatMessage {
  id: string;
  role: "user" | "agent" | "system";
  content: string;
  /** Attachments sent with or returned alongside this message. */
  attachments?: ChatAttachment[];
  /** Tool-use chain for an agent turn (rehydrated from tool_trace). */
  toolTrace?: ToolTraceEntry[];
  /** Set when this message is a user DECISION on a request (approve /
   *  reject / mark-done) rather than a chat turn — rendered as a centered
   *  chip in My Chat so the action is visible the moment it happens
   *  (core#2636). */
  decision?: "approved" | "rejected" | "done";
  timestamp: string; // ISO string for serialization
}

export function createMessage(
  role: ChatMessage["role"],
  content: string,
  attachments?: ChatAttachment[],
  toolTrace?: ToolTraceEntry[],
  id?: string,
): ChatMessage {
  return Object.freeze({
    // When the caller supplies an id (the sender threads the SAME id it
    // puts in the A2A payload's messageId), the USER_MESSAGE broadcast
    // echo — which carries that messageId — dedups against this
    // optimistic bubble (core#2697). Without it the optimistic id and
    // the payload messageId were two independent randomUUIDs, so the
    // origin device rendered its own message twice.
    id: id ?? crypto.randomUUID(),
    role,
    content,
    // Conditional spread avoids `attachments: undefined` appearing in
    // Object.keys() when no attachments are provided.
    ...(attachments?.length ? { attachments } : {}),
    ...(toolTrace?.length ? { toolTrace } : {}),
    timestamp: new Date().toISOString(),
  });
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

// appendMessageDedupedById is the cross-device sync deduper
// (core#2697). When the server carries a stable `messageId`
// (USER_MESSAGE broadcasts echo it back), collapse any tail entry
// with the same id regardless of timing. Origin device already
// optimistically added the message via onUserMessage; on the WS
// echo the same id, the second append is a no-op. Other devices
// receive the broadcast with no prior copy and append.
//
// Why a separate helper rather than widening appendMessageDeduped:
// the id-aware contract is "duplicate if id matches AND ids are
// stable," which is strictly stronger than the time-window
// fallback. Mixing them in one function would force every caller
// to thread an id-aware flag; the two paths are independent
// (agent-message triple-delivery has no id, USER_MESSAGE
// cross-device does).
export function appendMessageDedupedById(
  prev: ChatMessage[],
  msg: ChatMessage,
): ChatMessage[] {
  if (msg.id) {
    if (prev.some((m) => m.id === msg.id)) return prev;
  }
  return [...prev, msg];
}

function attachmentSignature(atts: ChatAttachment[] | undefined): string {
  if (!atts || atts.length === 0) return "";
  // URI is the stable identity — name can differ across delivery
  // paths (agent vs our parser's basename fallback).
  return atts.map((a) => a.uri).sort().join("|");
}
