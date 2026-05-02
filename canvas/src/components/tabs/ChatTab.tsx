"use client";

import { useState, useRef, useEffect, useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "@/lib/api";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { type ChatMessage, type ChatAttachment, createMessage, appendMessageDeduped } from "./chat/types";
import { uploadChatFiles, downloadChatFile } from "./chat/uploads";
import { AttachmentChip, PendingAttachmentPill } from "./chat/AttachmentViews";
import { extractFilesFromTask } from "./chat/message-parser";
import { AgentCommsPanel } from "./chat/AgentCommsPanel";
import { appendActivityLine } from "./chat/activityLog";
import { activityRowToMessages, type ActivityRowForHydration } from "./chat/historyHydration";
import { runtimeDisplayName } from "@/lib/runtime-names";
import { ConfirmDialog } from "@/components/ConfirmDialog";

interface Props {
  workspaceId: string;
  data: WorkspaceNodeData;
}

type ChatSubTab = "my-chat" | "agent-comms";

// A2A response shape (subset). The full schema is in @a2a-js/sdk but we only
// need parts/artifacts text + file extraction for the synchronous fallback.
interface A2AFileRef {
  name?: string;
  mimeType?: string;
  uri?: string;
  bytes?: string;
  size?: number;
}
// Outbound shape matches a2a-sdk's JSON-RPC `SendMessageRequest`
// Pydantic union (TextPart | FilePart | DataPart). The flat
// protobuf shape `{url, filename, mediaType}` is rejected at the
// request boundary with `Field required` errors — keep this
// outbound shape unless a2a-sdk migrates the JSON-RPC schema.
interface A2APart {
  kind: string;
  text?: string;
  file?: A2AFileRef;
}
interface A2AResponse {
  result?: {
    parts?: A2APart[];
    artifacts?: Array<{ parts: A2APart[] }>;
  };
}

/** Detect activity-log rows that the workspace's own runtime fired
 *  against itself but were misclassified as canvas-source. The proper
 *  fix is the X-Workspace-ID header from `self_source_headers()` in
 *  workspace/platform_auth.py, which makes the platform record
 *  source_id = workspace_id. But three failure modes still leak a
 *  self-message into "My Chat":
 *
 *    1. Historical rows already in the DB with source_id=NULL.
 *    2. Workspace containers running pre-fix heartbeat.py / main.py
 *       (the fix only takes effect after an image rebuild + redeploy).
 *    3. Future internal triggers added without the helper.
 *
 *  This client-side filter recognises the heartbeat trigger by its
 *  exact prefix — the heartbeat assembles
 *
 *    "Delegation results are ready. Review them and take appropriate
 *     action:\n" + summary_lines + report_instruction
 *
 *  in workspace/heartbeat.py. The prefix is template-fixed so a
 *  string match is reliable. If the heartbeat copy ever changes,
 *  update this constant in the same commit.
 *
 *  This is a backstop, not the primary defence — the X-Workspace-ID
 *  header is. Filtering content is fragile to copy edits, so keep
 *  the list narrow. */
const INTERNAL_SELF_MESSAGE_PREFIXES = [
  "Delegation results are ready. Review them and take appropriate action",
];

function isInternalSelfMessage(text: string): boolean {
  return INTERNAL_SELF_MESSAGE_PREFIXES.some((p) => text.startsWith(p));
}

// extractReplyText pulls the agent's text reply out of an A2A response.
// Concatenates ALL text parts (joined with "\n") rather than returning
// just the first. Claude Code and other runtimes commonly emit multi-
// part text replies for long content (markdown tables, code blocks),
// and the prior "first part wins" implementation silently truncated
// the rest — observed on a 15k-char Wave 1 brief that rendered only
// the table header. Mirrors extractTextsFromParts in message-parser.ts.
//
// Server-side counterpart in workspace-server/internal/channels/
// manager.go has the same single-part bug; fix that too if/when a
// channel-delivered reply (Slack, Lark, etc.) gets truncated.
function extractReplyText(resp: A2AResponse): string {
  const collect = (parts: A2APart[] | undefined): string => {
    if (!parts) return "";
    return parts
      .filter((p) => p.kind === "text")
      .map((p) => p.text ?? "")
      .filter(Boolean)
      .join("\n");
  };
  const result = resp?.result;
  const collected: string[] = [];
  const fromParts = collect(result?.parts);
  if (fromParts) collected.push(fromParts);
  // Walk artifacts even if parts had text — some producers (Hermes
  // tool calls) emit a summary in parts AND details in artifacts.
  // Returning early on parts dropped the artifact body silently.
  if (result?.artifacts) {
    for (const a of result.artifacts) {
      const t = collect(a.parts);
      if (t) collected.push(t);
    }
  }
  return collected.join("\n");
}

// Agent-returned files live on the same response shape as text —
// delegated to extractFilesFromTask in message-parser.ts, which also
// walks status.message.parts (that ChatTab's legacy text extractor
// doesn't). Single source of truth for file-part parsing across
// live chat, activity log replay, and any future consumers.

/**
 * Load chat history from the activity_logs database via the platform API.
 * Uses source=canvas to only get user-initiated messages (not agent-to-agent).
 */
async function loadMessagesFromDB(workspaceId: string): Promise<{ messages: ChatMessage[]; error: string | null }> {
  try {
    const activities = await api.get<ActivityRowForHydration[]>(
      `/workspaces/${workspaceId}/activity?type=a2a_receive&source=canvas&limit=50`,
    );

    const messages: ChatMessage[] = [];
    // Activities are newest-first, reverse for chronological order.
    // Per-row mapping lives in chat/historyHydration.ts so it can be
    // unit-tested without spinning up the full ChatTab component
    // (regression cover for the timestamp-collapse bug).
    for (const a of [...activities].reverse()) {
      messages.push(...activityRowToMessages(a, isInternalSelfMessage));
    }
    return { messages, error: null };
  } catch (err) {
    return {
      messages: [],
      error: err instanceof Error ? err.message : "Failed to load chat history",
    };
  }
}

/**
 * ChatTab container — renders sub-tab bar + My Chat or Agent Comms panel.
 */
export function ChatTab({ workspaceId, data }: Props) {
  const [subTab, setSubTab] = useState<ChatSubTab>("my-chat");

  return (
    <div className="flex flex-col h-full">
      {/* Sub-tab bar — role="tablist" so screen readers expose tab context */}
      <div
        role="tablist"
        className="flex border-b border-zinc-800/40 bg-zinc-900/30 px-2 shrink-0"
        onKeyDown={(e) => {
          const tabs: ChatSubTab[] = ["my-chat", "agent-comms"];
          const idx = tabs.indexOf(subTab);
          if (e.key === "ArrowRight") { e.preventDefault(); setSubTab(tabs[(idx + 1) % tabs.length]); }
          else if (e.key === "ArrowLeft") { e.preventDefault(); setSubTab(tabs[(idx - 1 + tabs.length) % tabs.length]); }
        }}
      >
        <button
          id="chat-tab-my-chat"
          role="tab"
          aria-selected={subTab === "my-chat"}
          aria-controls="chat-panel-my-chat"
          tabIndex={subTab === "my-chat" ? 0 : -1}
          onClick={() => setSubTab("my-chat")}
          className={`px-3 py-1.5 text-[10px] font-medium transition-colors ${
            subTab === "my-chat"
              ? "text-zinc-200 border-b-2 border-blue-500"
              : "text-zinc-500 hover:text-zinc-300"
          }`}
        >
          My Chat
        </button>
        <button
          id="chat-tab-agent-comms"
          role="tab"
          aria-selected={subTab === "agent-comms"}
          aria-controls="chat-panel-agent-comms"
          tabIndex={subTab === "agent-comms" ? 0 : -1}
          onClick={() => setSubTab("agent-comms")}
          className={`px-3 py-1.5 text-[10px] font-medium transition-colors ${
            subTab === "agent-comms"
              ? "text-zinc-200 border-b-2 border-blue-500"
              : "text-zinc-500 hover:text-zinc-300"
          }`}
        >
          Agent Comms
        </button>
      </div>
      {/* Content — both panels are always in the DOM so aria-controls targets exist.
           Inactive panel is hidden via a conditional `hidden` Tailwind class
           (display: none) because the native HTML `hidden` attribute is
           overridden by the panel's own `flex` utility — that's why both
           sections used to render stacked. */}
      <div
        id="chat-panel-my-chat"
        role="tabpanel"
        aria-labelledby="chat-tab-my-chat"
        className={`flex-1 overflow-hidden flex-col ${
          subTab === "my-chat" ? "flex" : "hidden"
        }`}
      >
        <MyChatPanel workspaceId={workspaceId} data={data} />
      </div>
      <div
        id="chat-panel-agent-comms"
        role="tabpanel"
        aria-labelledby="chat-tab-agent-comms"
        className={`flex-1 overflow-hidden flex-col ${
          subTab === "agent-comms" ? "flex" : "hidden"
        }`}
      >
        <AgentCommsPanel workspaceId={workspaceId} />
      </div>
    </div>
  );
}

/**
 * MyChatPanel — user↔agent conversation (extracted from original ChatTab).
 */
function MyChatPanel({ workspaceId, data }: Props) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  // `sending` is strictly the "this tab kicked off a send and hasn't
  // seen the reply yet" signal. Previously this was initialized from
  // data.currentTask to pick up in-flight agent work on mount, but
  // that conflated agent-busy (workspace heartbeat) with user-
  // in-flight (local send): when the WS dropped a TASK_COMPLETE event,
  // currentTask lingered, the component re-mounted with sending=true,
  // and the Send button stayed disabled forever even though nothing
  // local was in flight. For the "agent is busy, show spinner" UX,
  // use data.currentTask directly in the render path.
  const [sending, setSending] = useState(false);
  const [thinkingElapsed, setThinkingElapsed] = useState(0);
  const [activityLog, setActivityLog] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const currentTaskRef = useRef(data.currentTask);
  const sendingFromAPIRef = useRef(false);
  const [agentReachable, setAgentReachable] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmRestart, setConfirmRestart] = useState(false);
  const bottomRef = useRef<HTMLDivElement>(null);
  // Files the user has picked but not yet sent. Cleared on send
  // (upload success) or by the × on each pill.
  const [pendingFiles, setPendingFiles] = useState<File[]>([]);
  const [uploading, setUploading] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  // Guard against a double-click during the upload phase: React
  // state updates from the click that started the upload haven't
  // flushed yet, so the disabled-button logic sees `uploading=false`
  // from the closure and lets a second `sendMessage` enter. A ref
  // observes the latest value synchronously.
  const sendInFlightRef = useRef(false);
  // Monotonic token bumped on every sendMessage entry. Each .then()/
  // .catch() captures its own token in closure and bails if a newer
  // send has superseded it — prevents a late HTTP response for an
  // earlier message from clobbering the flags / appending text that
  // belong to a newer in-flight send. Race scenario the token closes:
  // (1) send msg #1 (2) WS push for msg #1 arrives, releases guards
  // (3) user sends msg #2 (4) HTTP for msg #1 finally lands — without
  // the token check, .then() sees sendingFromAPIRef=true (set by
  // msg #2's send), enters the main body, and processes msg #1's body
  // as if it were msg #2's reply.
  const sendTokenRef = useRef(0);

  // Release every in-flight send guard at once. Used by every site
  // that ends a send: pendingAgentMsgs WS push, ACTIVITY_LOGGED
  // a2a_receive ok/error WS event, HTTP .then() success, and HTTP
  // .catch() success. Keep these in lockstep — a future contributor
  // adding a new "I saw the reply" path that only clears `sending` +
  // `sendingFromAPIRef` (the natural pair) silently re-introduces
  // the post-WS Send-button freeze, because the disabled-button
  // logic can't see `sendInFlightRef` and so the visible state diverges
  // from the synchronous re-entry guard at line 464.
  const releaseSendGuards = useCallback(() => {
    setSending(false);
    sendingFromAPIRef.current = false;
    sendInFlightRef.current = false;
  }, []);

  // Load chat history from database on mount
  useEffect(() => {
    setLoading(true);
    setLoadError(null);
    loadMessagesFromDB(workspaceId).then(({ messages: msgs, error: fetchErr }) => {
      setMessages(msgs);
      setLoadError(fetchErr);
      setLoading(false);
    });
  }, [workspaceId]);

  // Agent reachability
  useEffect(() => {
    const reachable = data.status === "online" || data.status === "degraded";
    setAgentReachable(reachable);
    setError(reachable ? null : `Agent is ${data.status}`);
  }, [data.status]);

  useEffect(() => {
    currentTaskRef.current = data.currentTask;
  }, [data.currentTask]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  // Consume agent push messages (send_message_to_user) from global store.
  // Runtimes like Claude Code SDK deliver their reply via a WS push rather
  // than the /a2a HTTP response — when that happens, the push is the
  // authoritative "reply arrived" signal for the UI, so clear `sending`
  // here too. The HTTP .then() coordinates through sendingFromAPIRef so
  // whichever path clears first wins.
  const pendingAgentMsgs = useCanvasStore((s) => s.agentMessages[workspaceId]);
  useEffect(() => {
    if (!pendingAgentMsgs || pendingAgentMsgs.length === 0) return;
    const consume = useCanvasStore.getState().consumeAgentMessages;
    const msgs = consume(workspaceId);
    for (const m of msgs) {
      // Dedupe in case the agent proactively pushed the same text the
      // HTTP /a2a response already delivered (observed with the Hermes
      // runtime, which emits both a reply body and a send_message_to_user
      // push for the same content). Attachments ride along with the
      // message so files returned by the A2A_RESPONSE WS path render
      // their download chips.
      setMessages((prev) => appendMessageDeduped(prev, createMessage("agent", m.content, m.attachments)));
    }
    if (sendingFromAPIRef.current && msgs.length > 0) {
      // Reply arrived via WS push (e.g. claude-code SDK). Release all
      // three guards together — without sendInFlightRef the next
      // sendMessage() silently no-ops at the synchronous re-entry
      // check.
      releaseSendGuards();
    }
  }, [pendingAgentMsgs, workspaceId]);

  // Resolve workspace ID → name for activity display
  const resolveWorkspaceName = useCallback((id: string) => {
    const nodes = useCanvasStore.getState().nodes;
    const node = nodes.find((n) => n.id === id);
    return (node?.data as WorkspaceNodeData)?.name || id.slice(0, 8);
  }, []);

  // Elapsed timer while sending
  useEffect(() => {
    if (!sending) {
      setThinkingElapsed(0);
      return;
    }
    const startTime = Date.now();
    const timer = setInterval(() => {
      setThinkingElapsed(Math.floor((Date.now() - startTime) / 1000));
    }, 1000);
    return () => clearInterval(timer);
  }, [sending]);

  // Live activity feed seed — clears when not sending. The actual
  // event subscription is unconditional below (useSocketEvent at the
  // top level — hooks can't be conditional). The handler gates on
  // `sending` itself so it's a no-op when idle.
  useEffect(() => {
    if (!sending) {
      setActivityLog([]);
      return;
    }
    setActivityLog([`Processing with ${runtimeDisplayName(data.runtime)}...`]);
  }, [sending, data.runtime]);

  // Subscribe to global WS via the singleton ReconnectingSocket (no
  // per-component WebSocket — the previous pattern dropped events
  // silently on any reconnect because each panel's raw socket had no
  // onclose handler).
  useSocketEvent((msg) => {
    if (!sending) return;
    try {
        if (msg.event === "ACTIVITY_LOGGED") {
          // Filter to events for THIS workspace. The platform's
          // BroadcastOnly fires to every connected client, and
          // without this guard a sibling workspace's a2a_send would
          // surface as "→ Delegating to X..." inside the wrong
          // chat panel. (workspace_id on the WS envelope is the
          // workspace whose activity_log row we just wrote.)
          if (msg.workspace_id !== workspaceId) return;

          const p = msg.payload || {};
          const type = p.activity_type as string;
          const method = (p.method as string) || "";
          const status = (p.status as string) || "";
          const targetId = (p.target_id as string) || "";
          const durationMs = p.duration_ms as number | undefined;
          const summary = (p.summary as string) || "";

          let line = "";
          if (type === "a2a_receive" && method === "message/send") {
            const targetName = resolveWorkspaceName(targetId || msg.workspace_id);
            if (status === "ok" && durationMs) {
              const sec = Math.round(durationMs / 1000);
              line = `← ${targetName} responded (${sec}s)`;
              // The platform logs a successful a2a_receive once the workspace
              // has fully produced its reply. That's the authoritative "done"
              // signal for the spinner — clear it even if the reply hasn't
              // surfaced through the store yet (it may be delivered shortly
              // via pendingAgentMsgs or the HTTP .then()).
              const own = (targetId || msg.workspace_id) === workspaceId;
              if (own && sendingFromAPIRef.current) {
                releaseSendGuards();
              }
            } else if (status === "error") {
              line = `⚠ ${targetName} error`;
              const own = (targetId || msg.workspace_id) === workspaceId;
              if (own && sendingFromAPIRef.current) {
                releaseSendGuards();
                setError("Agent error (Exception) — see workspace logs for details.");
              }
            }
          } else if (type === "a2a_send") {
            const targetName = resolveWorkspaceName(targetId);
            line = `→ Delegating to ${targetName}...`;
          } else if (type === "task_update") {
            if (summary) line = `⟳ ${summary}`;
          } else if (type === "agent_log") {
            // Per-tool-use telemetry from claude_sdk_executor's
            // _report_tool_use. The summary already carries an icon
            // + human-readable args (📄 Read /path, ⚡ Bash: …)
            // so we render it verbatim. No icon prefix here — the
            // emoji at the start of summary is the visual marker.
            if (summary) line = summary;
          }

          if (line) {
            setActivityLog((prev) => appendActivityLine(prev, line));
          }
        } else if (msg.event === "TASK_UPDATED" && msg.workspace_id === workspaceId) {
          const task = (msg.payload?.current_task as string) || "";
          if (task) {
            setActivityLog((prev) => appendActivityLine(prev, `⟳ ${task}`));
          }
        }
        // A2A_RESPONSE is already consumed by the store and its text is
        // appended to messages via the pendingAgentMsgs effect above; we
        // don't need to duplicate it here.
    } catch { /* ignore */ }
  });

  const sendMessage = async () => {
    const text = input.trim();
    const filesToSend = pendingFiles;
    // Allow sending if EITHER text OR attachments are present — a user
    // can drop a file with no text and the agent still receives it.
    if ((!text && filesToSend.length === 0) || !agentReachable || sending || uploading) return;
    // Synchronous re-entry guard — see sendInFlightRef comment.
    if (sendInFlightRef.current) return;
    sendInFlightRef.current = true;

    // Upload attachments first so we can include URIs in the A2A
    // message parts. Sequential-before-send: a message with references
    // to files not yet staged would fail agent-side; staging happens
    // synchronously via /chat/uploads before message/send dispatch.
    let uploaded: ChatAttachment[] = [];
    if (filesToSend.length > 0) {
      setUploading(true);
      try {
        uploaded = await uploadChatFiles(workspaceId, filesToSend);
      } catch (e) {
        setUploading(false);
        sendInFlightRef.current = false;
        setError(e instanceof Error ? `Upload failed: ${e.message}` : "Upload failed");
        return;
      }
      setUploading(false);
    }

    setInput("");
    setPendingFiles([]);
    setMessages((prev) => [...prev, createMessage("user", text, uploaded)]);
    setSending(true);
    sendingFromAPIRef.current = true;
    setError(null);
    // Capture this send's token so the .then()/.catch() callbacks can
    // detect a newer send that may have superseded them. See the
    // sendTokenRef declaration for the race scenario this closes.
    const myToken = ++sendTokenRef.current;

    // Build conversation history from prior messages (last 20)
    const history = messages
      .filter((m) => m.role === "user" || m.role === "agent")
      .slice(-20)
      .map((m) => ({
        role: m.role === "user" ? "user" : "agent",
        parts: [{ kind: "text", text: m.content }],
      }));

    // A2A parts: text part (if any) + file parts (per attachment). The
    // agent sees both in a single turn, matching the A2A spec shape.
    // Wire shape is v0 — see A2APart definition above.
    const parts: A2APart[] = [];
    if (text) parts.push({ kind: "text", text });
    for (const att of uploaded) {
      parts.push({
        kind: "file",
        file: {
          name: att.name,
          mimeType: att.mimeType,
          uri: att.uri,
          size: att.size,
        },
      });
    }

    // A2A calls can legitimately take minutes — LLM latency +
    // multi-turn tool use is common on slower providers (Hermes+minimax,
    // Claude Code invoking bash/file tools, etc.). The 15s default
    // would silently abort the fetch here, leaving the server to
    // complete the reply and the user staring at
    // "agent may be unreachable". Match the upload timeout (60s × 2)
    // for the happy-path ceiling; anything longer is genuinely stuck.
    api.post<A2AResponse>(`/workspaces/${workspaceId}/a2a`, {
      method: "message/send",
      params: {
        message: {
          role: "user",
          messageId: crypto.randomUUID(),
          parts,
        },
        metadata: { history },
      },
    }, { timeoutMs: 120_000 })
      .then((resp) => {
        // Bail without touching any flags if a newer sendMessage has
        // already run — its myToken bumped sendTokenRef, so this is
        // a stale callback for an earlier message. The newer send
        // owns the in-flight guards now.
        if (sendTokenRef.current !== myToken) return;
        // Skip if the WS A2A_RESPONSE event already handled this response.
        // Both paths (WS + HTTP) check sendingFromAPIRef — whichever clears
        // it first wins, the other becomes a no-op (no duplicate messages).
        if (!sendingFromAPIRef.current) {
          sendInFlightRef.current = false;
          return;
        }
        const replyText = extractReplyText(resp);
        const replyFiles = extractFilesFromTask((resp?.result ?? {}) as Record<string, unknown>);
        if (replyText || replyFiles.length > 0) {
          setMessages((prev) =>
            appendMessageDeduped(prev, createMessage("agent", replyText, replyFiles)),
          );
        }
        releaseSendGuards();
      })
      .catch(() => {
        // Stale-callback guard — same rationale as .then().
        if (sendTokenRef.current !== myToken) return;
        // Same dedup guard as .then(): if a WS path (pendingAgentMsgs
        // or ACTIVITY_LOGGED a2a_receive ok) already delivered the
        // reply, sendingFromAPIRef is already false and there's
        // nothing to roll back. Surfacing "Failed to send" here would
        // contradict the agent reply the user is currently reading —
        // exactly the false-positive observed when the HTTP request
        // hung up (proxy idle / 502) after WS already won.
        if (!sendingFromAPIRef.current) {
          sendInFlightRef.current = false;
          return;
        }
        releaseSendGuards();
        setError("Failed to send message — agent may be unreachable");
      });
  };

  const onFilesPicked = (fileList: FileList | null) => {
    if (!fileList) return;
    const picked = Array.from(fileList);
    // Deduplicate against current pending set by name+size — user
    // picking the same file twice shouldn't append it.
    setPendingFiles((prev) => {
      const keyed = new Set(prev.map((f) => `${f.name}:${f.size}`));
      return [...prev, ...picked.filter((f) => !keyed.has(`${f.name}:${f.size}`))];
    });
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const removePendingFile = (index: number) =>
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));

  // Monotonic counter so two paste events within the same wall-clock
  // second still produce distinct filenames. Without this, on
  // Firefox (where pasted images have an empty `file.name`), two
  // pastes ~100ms apart could yield identical synthetic names AND
  // identical sizes, collapsing into one attachment via the
  // `name:size` dedup in onFilesPicked.
  const pasteCounterRef = useRef(0);

  /** Paste-from-clipboard image attachment.
   *
   *  Browser clipboard image items arrive as `File`s whose `name` is
   *  often a generic "image.png" (Chrome) or empty (Firefox/Safari),
   *  so two consecutive screenshot pastes collide on the name+size
   *  dedup the file-picker uses. Re-tag each pasted image with a
   *  per-paste unique name so dedup keeps them apart and the upload
   *  pipeline (which expects a non-empty filename) is happy.
   *
   *  Falls through to onFilesPicked via direct File[] (NOT through
   *  the DataTransfer constructor — that throws on Safari < 14.1
   *  and old Edge, silently aborting the paste).
   *
   *  Only intercepts the paste when the clipboard has at least one
   *  image; text-only pastes fall through to the textarea's default
   *  behaviour. */
  const mimeToExt = (mime: string): string => {
    // Avoid raw `mime.split("/")[1]` — that yields `"svg+xml"`,
    // `"jpeg"`, `"webp"` etc. which produce ugly filenames and may
    // trip server-side extension allowlists. Map known types
    // explicitly; unknown falls back to a safe default.
    if (mime === "image/svg+xml") return "svg";
    if (mime === "image/jpeg") return "jpg";
    if (mime === "image/png") return "png";
    if (mime === "image/gif") return "gif";
    if (mime === "image/webp") return "webp";
    if (mime === "image/heic") return "heic";
    return "png";
  };

  const onPasteIntoComposer = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    if (!dropEnabled) return;
    const items = e.clipboardData?.items;
    if (!items || items.length === 0) return;
    const imageFiles: File[] = [];
    for (let i = 0; i < items.length; i++) {
      const item = items[i];
      if (!item.type.startsWith("image/")) continue;
      const file = item.getAsFile();
      if (!file) continue;
      const ext = mimeToExt(file.type);
      const stamp = new Date()
        .toISOString()
        .replace(/[:.]/g, "-")
        .slice(0, 19);
      const seq = pasteCounterRef.current++;
      const fname = `pasted-${stamp}-${seq}-${i}.${ext}`;
      imageFiles.push(new File([file], fname, { type: file.type }));
    }
    if (imageFiles.length === 0) return;
    e.preventDefault();
    // Reuse the picker path so file-size guards, dedup, and pending-
    // list state all run through the same code. Build a synthetic
    // FileList-like object to avoid the DataTransfer constructor —
    // that's missing on Safari < 14.1 / old Edge and would silently
    // throw, leaving the paste a no-op.
    addPastedFiles(imageFiles);
  };

  // Variant of onFilesPicked that accepts a File[] directly, sidestepping
  // the DataTransfer-FileList round-trip. Same dedup + state shape.
  const addPastedFiles = (files: File[]) => {
    setPendingFiles((prev) => {
      const keyed = new Set(prev.map((f) => `${f.name}:${f.size}`));
      return [...prev, ...files.filter((f) => !keyed.has(`${f.name}:${f.size}`))];
    });
  };

  // Drag-and-drop staging. dragDepthRef counts enter vs leave events so
  // the overlay doesn't flicker when the cursor crosses nested children
  // (textarea, buttons) — dragenter/dragleave fire for every boundary.
  const [dragOver, setDragOver] = useState(false);
  const dragDepthRef = useRef(0);
  const dropEnabled = agentReachable && !sending && !uploading;
  const isFileDrag = (e: React.DragEvent) =>
    Array.from(e.dataTransfer.types || []).includes("Files");

  const onDragEnter = (e: React.DragEvent) => {
    if (!dropEnabled || !isFileDrag(e)) return;
    e.preventDefault();
    dragDepthRef.current += 1;
    setDragOver(true);
  };
  const onDragOver = (e: React.DragEvent) => {
    if (!dropEnabled || !isFileDrag(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  };
  const onDragLeave = (e: React.DragEvent) => {
    if (!dropEnabled || !isFileDrag(e)) return;
    dragDepthRef.current = Math.max(0, dragDepthRef.current - 1);
    if (dragDepthRef.current === 0) setDragOver(false);
  };
  const onDrop = (e: React.DragEvent) => {
    if (!dropEnabled || !isFileDrag(e)) return;
    e.preventDefault();
    dragDepthRef.current = 0;
    setDragOver(false);
    onFilesPicked(e.dataTransfer.files);
  };

  const downloadAttachment = (att: ChatAttachment) => {
    // Errors here are rare but user-visible (401 on a revoked token,
    // 404 if the agent deleted the file). Surface via the inline
    // error banner — the message list itself stays untouched.
    downloadChatFile(workspaceId, att).catch((e) => {
      setError(e instanceof Error ? `Download failed: ${e.message}` : "Download failed");
    });
  };

  const isOnline = data.status === "online" || data.status === "degraded";

  return (
    <div
      className="flex flex-col h-full relative"
      onDragEnter={onDragEnter}
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
    >
      {dragOver && (
        <div
          className="absolute inset-0 z-20 flex items-center justify-center bg-blue-500/10 border-2 border-dashed border-blue-400 rounded pointer-events-none"
          aria-live="polite"
        >
          <div className="bg-zinc-900/90 border border-blue-400/50 rounded-lg px-4 py-2 text-xs text-blue-200">
            Drop to attach
          </div>
        </div>
      )}
      {/* Messages */}
      <div className="flex-1 overflow-y-auto p-3 space-y-3">
        {loading && (
          <div className="text-xs text-zinc-500 text-center py-4">Loading chat history...</div>
        )}
        {!loading && loadError !== null && messages.length === 0 && (
          <div
            role="alert"
            className="mx-2 mt-2 rounded-lg border border-red-800/50 bg-red-950/30 px-3 py-2.5"
          >
            <p className="text-[11px] text-red-400 mb-1.5">
              Failed to load chat history: {loadError}
            </p>
            <button
              onClick={() => {
                setLoading(true);
                setLoadError(null);
                loadMessagesFromDB(workspaceId).then(({ messages: msgs, error: fetchErr }) => {
                  setMessages(msgs);
                  setLoadError(fetchErr);
                  setLoading(false);
                });
              }}
              className="text-[10px] px-2 py-0.5 rounded bg-red-800/40 text-red-300 hover:bg-red-700/50 transition-colors"
            >
              Retry
            </button>
          </div>
        )}
        {!loading && loadError === null && messages.length === 0 && (
          <div className="text-xs text-zinc-500 text-center py-8">
            No messages yet. Send a message to start chatting with this agent.
          </div>
        )}
        {messages.map((msg) => (
          <div key={msg.id} className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}>
            <div
              className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
                msg.role === "user"
                  ? "bg-blue-600/30 text-blue-100 border border-blue-500/20"
                  : msg.role === "system"
                    ? "bg-red-900/30 text-red-200 border border-red-800/30"
                    : "bg-zinc-800/80 text-zinc-200 border border-zinc-700/30"
              }`}
            >
              {msg.content && (
                <div className="prose prose-sm prose-invert max-w-none [&>p]:mb-1 [&>p:last-child]:mb-0">
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>{msg.content}</ReactMarkdown>
                </div>
              )}
              {msg.attachments && msg.attachments.length > 0 && (
                <div className={`flex flex-wrap gap-1 ${msg.content ? "mt-1.5" : ""}`}>
                  {msg.attachments.map((att, i) => (
                    <AttachmentChip
                      key={`${msg.id}-${i}`}
                      attachment={att}
                      onDownload={downloadAttachment}
                      tone={msg.role === "user" ? "user" : "agent"}
                    />
                  ))}
                </div>
              )}
              <div className="text-[9px] text-zinc-500 mt-1">
                {new Date(msg.timestamp).toLocaleTimeString()}
              </div>
            </div>
          </div>
        ))}

        {/* Thinking indicator — shows when this tab is awaiting a reply
           OR when the workspace heartbeat reports an in-flight task
           (covers the "agent is already busy when I open the tab" case
           without locking the Send button on a stale currentTask). */}
        {(sending || !!data.currentTask) && (
          <div className="flex justify-start">
            <div className="bg-zinc-800/50 border border-zinc-700/30 rounded-lg px-3 py-2 max-w-[85%]">
              <div className="flex items-center gap-2 text-xs text-zinc-400">
                <span className="flex gap-0.5">
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "0ms" }} />
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "150ms" }} />
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "300ms" }} />
                </span>
                {thinkingElapsed}s
              </div>
              {activityLog.length > 0 && (
                <div className="mt-1.5 text-[9px] text-zinc-500 space-y-0.5">
                  <div className="text-zinc-400">Processing with {runtimeDisplayName(data.runtime)}...</div>
                  {activityLog.map((line, i) => (
                    <div key={line + i} className="pl-2 border-l border-zinc-700">◇ {line}</div>
                  ))}
                </div>
              )}
            </div>
          </div>
        )}
        <div ref={bottomRef} />
      </div>

      {/* Error banner */}
      {error && (
        <div className="px-3 py-2 bg-red-900/20 border-t border-red-800/30">
          <div className="flex items-center justify-between">
            <span className="text-[10px] text-red-400">{error}</span>
            {!isOnline && (
              <button
                onClick={() => setConfirmRestart(true)}
                className="text-[11px] px-2 py-0.5 bg-red-800/40 text-red-300 rounded hover:bg-red-700/50"
              >
                Restart
              </button>
            )}
          </div>
        </div>
      )}

      {/* Input */}
      <div className="p-3 border-t border-zinc-800">
        {pendingFiles.length > 0 && (
          <div className="flex flex-wrap gap-1.5 mb-2">
            {pendingFiles.map((f, i) => (
              <PendingAttachmentPill
                key={`${f.name}-${f.size}-${i}`}
                file={f}
                onRemove={() => removePendingFile(i)}
              />
            ))}
          </div>
        )}
        <div className="flex gap-2 items-end">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => onFilesPicked(e.target.files)}
            aria-hidden="true"
          />
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={!agentReachable || sending || uploading}
            aria-label="Attach file"
            title="Attach file"
            className="p-2 bg-zinc-800 hover:bg-zinc-700 border border-zinc-700 rounded-lg text-zinc-400 hover:text-zinc-200 transition-colors shrink-0 disabled:opacity-40"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
              <path d="M11 6.5 7 10.5a2 2 0 1 0 2.8 2.8l4-4a3.5 3.5 0 0 0-5-5l-4.5 4.5a5 5 0 0 0 7 7l4-4" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </button>
          <textarea
            aria-label="Message to agent"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                sendMessage();
              }
            }}
            onPaste={onPasteIntoComposer}
            placeholder={agentReachable ? "Send a message... (Shift+Enter for new line, paste images to attach)" : `Agent is ${data.status}`}
            disabled={!agentReachable || sending}
            rows={1}
            className="flex-1 bg-zinc-800 border border-zinc-700 rounded-lg px-3 py-2 text-xs text-zinc-200 placeholder-zinc-500 focus:outline-none focus:border-blue-500 resize-none disabled:opacity-50"
          />
          <button
            onClick={sendMessage}
            disabled={(!input.trim() && pendingFiles.length === 0) || !agentReachable || sending || uploading}
            className="px-4 py-2 bg-blue-600 hover:bg-blue-500 text-xs font-medium rounded-lg text-white disabled:opacity-30 transition-colors shrink-0"
          >
            {uploading ? "Uploading…" : "Send"}
          </button>
        </div>
      </div>

      <ConfirmDialog
        open={confirmRestart}
        title="Restart workspace"
        message="Restart this workspace? The agent container will be stopped and re-provisioned."
        confirmLabel="Restart"
        confirmVariant="warning"
        onConfirm={() => {
          useCanvasStore.getState().restartWorkspace(workspaceId);
          setConfirmRestart(false);
        }}
        onCancel={() => setConfirmRestart(false)}
      />
    </div>
  );
}
