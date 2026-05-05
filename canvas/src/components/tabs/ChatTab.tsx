"use client";

import { useState, useRef, useEffect, useCallback, useLayoutEffect } from "react";
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

/** Initial chat history page size. The newest N messages are rendered
 *  on first paint; older history is fetched on demand via loadOlder()
 *  when the user scrolls the top sentinel into view. */
const INITIAL_HISTORY_LIMIT = 10;
/** Subsequent older-history batch size. Larger than INITIAL so a long
 *  scroll-back doesn't fan out into many round-trips. */
const OLDER_HISTORY_BATCH = 20;

/**
 * Load chat history from the activity_logs database via the platform API.
 * Uses source=canvas to only get user-initiated messages (not agent-to-agent).
 *
 * Pagination:
 *  - Pass `limit` to bound the page size (newest-first from server).
 *  - Pass `beforeTs` (RFC3339) to fetch rows STRICTLY OLDER than that
 *    timestamp. Combined with limit, this yields the next-older page
 *    when scrolling backward through history.
 *
 * `reachedEnd` is true when the server returned fewer rows than asked
 * for — caller uses this to disable further older-batch fetches.
 * (Counts row-level returns, not chat-bubble count: each row may
 * produce 1-2 bubbles.)
 */
async function loadMessagesFromDB(
  workspaceId: string,
  limit: number,
  beforeTs?: string,
): Promise<{ messages: ChatMessage[]; error: string | null; reachedEnd: boolean }> {
  try {
    const params = new URLSearchParams({
      type: "a2a_receive",
      source: "canvas",
      limit: String(limit),
    });
    if (beforeTs) params.set("before_ts", beforeTs);
    const activities = await api.get<ActivityRowForHydration[]>(
      `/workspaces/${workspaceId}/activity?${params.toString()}`,
    );

    const messages: ChatMessage[] = [];
    // Activities are newest-first, reverse for chronological order.
    // Per-row mapping lives in chat/historyHydration.ts so it can be
    // unit-tested without spinning up the full ChatTab component
    // (regression cover for the timestamp-collapse bug).
    for (const a of [...activities].reverse()) {
      messages.push(...activityRowToMessages(a, isInternalSelfMessage));
    }
    return { messages, error: null, reachedEnd: activities.length < limit };
  } catch (err) {
    return {
      messages: [],
      error: err instanceof Error ? err.message : "Failed to load chat history",
      reachedEnd: true,
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
        className="flex border-b border-line/40 bg-surface-sunken/30 px-2 shrink-0"
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
          className={`px-3 py-1.5 text-[10px] font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ${
            subTab === "my-chat"
              ? "text-ink border-b-2 border-accent"
              : "text-ink-mid hover:text-ink"
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
          className={`px-3 py-1.5 text-[10px] font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ${
            subTab === "agent-comms"
              ? "text-ink border-b-2 border-accent"
              : "text-ink-mid hover:text-ink"
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
  // Lazy-load older history on scroll-up.
  // - containerRef = the scrollable messages viewport
  // - topRef       = sentinel above the messages list; IO observes it
  //                  and triggers loadOlder() when it enters view
  // - hasMore      = false once a fetch returns < limit rows; stops IO
  // - loadingOlder = drives the "Loading older messages…" UI label
  // - inflightRef  = synchronous guard against double-entry of loadOlder
  //                  when the IO callback fires twice in the same
  //                  microtask (state-based guard would be stale until
  //                  the next React commit)
  // - scrollAnchorRef = saves distance-from-bottom before a prepend
  //                  so the useLayoutEffect below can restore the
  //                  user's exact viewport position. Without this,
  //                  prepending older messages would jump the scroll
  //                  position by the height of the new content.
  // - oldestMessageRef / hasMoreRef = let the loadOlder closure read
  //                  the latest values without taking them as deps —
  //                  every live agent push mutates `messages`, and
  //                  having loadOlder depend on `messages` would tear
  //                  down + re-arm the IntersectionObserver on every
  //                  push. Refs decouple the observer lifecycle from
  //                  message-list updates.
  const containerRef = useRef<HTMLDivElement>(null);
  const topRef = useRef<HTMLDivElement>(null);
  const [hasMore, setHasMore] = useState(true);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const inflightRef = useRef(false);
  // The scroll anchor includes the first-message id as it was BEFORE
  // the prepend — see useLayoutEffect below for why. Without this tag,
  // a live agent push that appends WHILE loadOlder is in flight would
  // run useLayoutEffect against the append (anchor still set), the
  // "restore" math would scroll the user to a stale offset, AND the
  // append's normal scroll-to-bottom would be swallowed.
  const scrollAnchorRef = useRef<
    { savedDistanceFromBottom: number; expectFirstIdNotEqual: string | null } | null
  >(null);
  const oldestMessageRef = useRef<ChatMessage | null>(null);
  const hasMoreRef = useRef(true);
  // Monotonic token bumped on workspace switch + on every loadOlder
  // entry. Each fetch's .then() captures its own token; if the token
  // has moved, the resolved messages belong to a stale workspace or a
  // superseded fetch and we silently drop them. Without this guard, a
  // workspace switch mid-fetch would have the in-flight promise
  // resolve into the new workspace's setMessages — the user sees
  // someone else's history briefly.
  const fetchTokenRef = useRef(0);
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

  // Initial-load fetch — used by the mount effect and the "Retry"
  // button below. Single source of truth so the two paths can't drift
  // (e.g. INITIAL_HISTORY_LIMIT bumped in the effect but not the
  // retry, leading to inconsistent first-paint sizes).
  const loadInitial = useCallback(() => {
    setLoading(true);
    setLoadError(null);
    setHasMore(true);
    // Bump the token; any in-flight fetch from the previous workspace
    // (or a previous retry) will see token != myToken in its .then()
    // and silently bail — the late response can't clobber the new
    // workspace's state.
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    loadMessagesFromDB(workspaceId, INITIAL_HISTORY_LIMIT).then(
      ({ messages: msgs, error: fetchErr, reachedEnd }) => {
        if (fetchTokenRef.current !== myToken) return;
        setMessages(msgs);
        setLoadError(fetchErr);
        setHasMore(!reachedEnd);
        setLoading(false);
      },
    );
  }, [workspaceId]);

  // Load chat history on mount / workspace switch.
  // Initial load is bounded to INITIAL_HISTORY_LIMIT (newest 10) — the
  // rest streams in as the user scrolls up via loadOlder() below. Pre-
  // 2026-05-05 this fetched the newest 50 in one shot; on a long-running
  // workspace that meant 50× message-bubble paint + DOM cost on every
  // tab-open even when the user only wanted to read the last few.
  useEffect(() => {
    loadInitial();
  }, [loadInitial]);

  // Mirror the latest oldest-message + hasMore into refs so loadOlder
  // can read them without taking `messages` as a dep. Every live push
  // through agentMessages would otherwise recreate loadOlder and tear
  // down the IO observer.
  useEffect(() => {
    oldestMessageRef.current = messages[0] ?? null;
  }, [messages]);
  useEffect(() => {
    hasMoreRef.current = hasMore;
  }, [hasMore]);

  // Fetch the next-older batch and prepend. Stable identity (deps =
  // [workspaceId]) so the IntersectionObserver effect below doesn't
  // re-arm on every messages update.
  const loadOlder = useCallback(async () => {
    // inflightRef is the load-bearing guard — synchronous, set BEFORE
    // any await, so two IO callbacks dispatched in the same microtask
    // can't both pass. The state checks are defensive secondary
    // gates for the slow-scroll case.
    if (inflightRef.current || !hasMoreRef.current) return;
    const oldest = oldestMessageRef.current;
    if (!oldest) return;
    const container = containerRef.current;
    if (!container) return;
    inflightRef.current = true;
    // Capture the user's distance-from-bottom BEFORE we prepend so the
    // useLayoutEffect can restore it after the new DOM lands. The
    // expectFirstIdNotEqual tag is what the layout effect checks
    // against `messages[0].id` to disambiguate prepend (id changed) vs
    // append (id unchanged → live message landed mid-fetch). Without
    // it, an agent push during loadOlder runs the "restore" against a
    // stale anchor — user gets yanked + the append's bottom-pin is
    // swallowed.
    scrollAnchorRef.current = {
      savedDistanceFromBottom: container.scrollHeight - container.scrollTop,
      expectFirstIdNotEqual: oldest.id,
    };
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    setLoadingOlder(true);
    try {
      const { messages: older, reachedEnd } = await loadMessagesFromDB(
        workspaceId,
        OLDER_HISTORY_BATCH,
        oldest.timestamp,
      );
      // Workspace switched (or another loadOlder bumped the token)
      // mid-fetch — drop these results, they belong to a stale tab.
      if (fetchTokenRef.current !== myToken) {
        scrollAnchorRef.current = null;
        return;
      }
      if (older.length > 0) {
        setMessages((prev) => [...older, ...prev]);
      } else {
        // Nothing came back — clear the anchor so the next paint doesn't
        // try to "restore" against a no-op prepend.
        scrollAnchorRef.current = null;
      }
      setHasMore(!reachedEnd);
    } finally {
      setLoadingOlder(false);
      inflightRef.current = false;
    }
  }, [workspaceId]);

  // IntersectionObserver on the top sentinel. Fires loadOlder() the
  // moment the user scrolls within 200px of the top. AbortController
  // unwires cleanly on workspace switch / unmount; root is the
  // scrollable container so we observe only what's visible inside it.
  //
  // Dependencies:
  //  - loadOlder    — stable per workspaceId (refs decouple it from
  //                   message updates), so this dep is here for the
  //                   workspace-switch case only
  //  - hasMore      — re-run when older history runs out so we
  //                   disconnect cleanly
  //  - hasMessages  — load-bearing: the sentinel JSX is gated on
  //                   `messages.length > 0`, so topRef.current is null
  //                   on the empty-messages render. We re-arm exactly
  //                   once when messages first land. NOT depending on
  //                   `messages.length` (or `messages`) directly so
  //                   each subsequent message append doesn't tear down
  //                   + re-arm the observer.
  const hasMessages = messages.length > 0;
  useEffect(() => {
    const top = topRef.current;
    const container = containerRef.current;
    if (!top || !container) return;
    if (!hasMore) return; // stop observing when no older history exists
    const ac = new AbortController();
    const io = new IntersectionObserver(
      (entries) => {
        if (ac.signal.aborted) return;
        if (entries[0]?.isIntersecting) loadOlder();
      },
      { root: container, rootMargin: "200px 0px 0px 0px", threshold: 0 },
    );
    io.observe(top);
    ac.signal.addEventListener("abort", () => io.disconnect());
    return () => ac.abort();
  }, [loadOlder, hasMore, hasMessages]);

  // Agent reachability
  useEffect(() => {
    const reachable = data.status === "online" || data.status === "degraded";
    setAgentReachable(reachable);
    setError(reachable ? null : `Agent is ${data.status}`);
  }, [data.status]);

  useEffect(() => {
    currentTaskRef.current = data.currentTask;
  }, [data.currentTask]);

  // Scroll behavior across messages updates:
  //  - Prepend (loadOlder landed)  → restore the user's saved
  //    distance-from-bottom so their reading position is unchanged.
  //  - Append / initial            → pin to latest bubble.
  // useLayoutEffect (not useEffect) so scroll restoration runs BEFORE
  // paint — otherwise the user sees the page jump for one frame.
  useLayoutEffect(() => {
    const container = containerRef.current;
    const anchor = scrollAnchorRef.current;
    // Only honor the anchor when this messages-update is the prepend
    // we expected. messages[0].id is the test:
    //   - prepend  → messages[0] is one of the older rows → id !== expectFirstIdNotEqual
    //   - append   → messages[0] unchanged → id === expectFirstIdNotEqual → fall through
    // Without this check, an agent push that lands mid-loadOlder would
    // run the restore against the append's update, yank the user's
    // scroll, AND swallow the append's bottom-pin.
    if (
      anchor &&
      container &&
      messages.length > 0 &&
      messages[0].id !== anchor.expectFirstIdNotEqual
    ) {
      container.scrollTop = container.scrollHeight - anchor.savedDistanceFromBottom;
      scrollAnchorRef.current = null;
      return;
    }
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
          className="absolute inset-0 z-20 flex items-center justify-center bg-accent/10 border-2 border-dashed border-blue-400 rounded pointer-events-none"
          aria-live="polite"
        >
          <div className="bg-surface-sunken/90 border border-blue-400/50 rounded-lg px-4 py-2 text-xs text-blue-200">
            Drop to attach
          </div>
        </div>
      )}
      {/* Messages */}
      <div ref={containerRef} className="flex-1 overflow-y-auto p-3 space-y-3">
        {loading && (
          <div className="text-xs text-ink-soft text-center py-4">Loading chat history...</div>
        )}
        {!loading && loadError !== null && messages.length === 0 && (
          <div
            role="alert"
            className="mx-2 mt-2 rounded-lg border border-red-800/50 bg-red-950/30 px-3 py-2.5"
          >
            <p className="text-[11px] text-bad mb-1.5">
              Failed to load chat history: {loadError}
            </p>
            <button
              onClick={loadInitial}
              className="text-[10px] px-2 py-0.5 rounded bg-red-800/40 text-bad hover:bg-red-700/50 transition-colors"
            >
              Retry
            </button>
          </div>
        )}
        {!loading && loadError === null && messages.length === 0 && (
          <div className="text-xs text-ink-soft text-center py-8">
            No messages yet. Send a message to start chatting with this agent.
          </div>
        )}
        {/* Top sentinel for lazy-loading older history. The IO observer
            in the effect above watches this; entering view triggers the
            next-older batch fetch. Sits ABOVE messages.map so it's the
            first thing the user reaches when scrolling up.

            Only mounted when there might be more history (hasMore) so a
            short conversation doesn't pay an idle observer. The
            "Loading older messages…" line replaces the sentinel during
            the fetch so the user sees feedback for the scroll-up
            gesture. Once we hit the end, we drop the sentinel entirely
            instead of showing a "no more messages" footer — the user's
            scroll resting against the top of the conversation IS the
            signal. */}
        {hasMore && messages.length > 0 && (
          <div ref={topRef} className="text-xs text-ink-soft text-center py-1">
            {loadingOlder ? "Loading older messages…" : " "}
          </div>
        )}
        {messages.map((msg) => (
          <div key={msg.id} className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}>
            <div
              className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
                msg.role === "user"
                  // Solid blue-600 in both modes — `bg-accent` themes
                  // lighter in dark, dropping white-text contrast to
                  // ~3:1 (fails AA). blue-600 keeps ~5:1 against white
                  // on both warm-paper and dark-slate panels.
                  ? "bg-blue-600 text-white border border-blue-700 dark:bg-blue-500 dark:border-blue-400 shadow-sm"
                  : msg.role === "system"
                    // Bump the system bubble's opacity in dark — /10
                    // overlay was nearly invisible against the dark
                    // panel bg.
                    ? "bg-bad/10 text-bad border border-bad/40 dark:bg-bad/25 dark:text-bad dark:border-bad/60"
                    // Agent bubble in dark: surface-card (#1a1d23) is
                    // only ~7% lighter than the panel bg-surface
                    // (#0e1014). Bump to zinc-700 for a clearly
                    // elevated bubble; light mode keeps the warm
                    // surface-card tint.
                    : "bg-surface-card text-ink border border-line dark:bg-zinc-700 dark:text-zinc-100 dark:border-zinc-600 shadow-sm"
              }`}
            >
              {msg.content && (
                <div
                  className={`prose prose-sm max-w-none [&>p]:mb-1 [&>p:last-child]:mb-0 ${
                    msg.role === "user"
                      ? "prose-invert"
                      // Agent bubbles in dark mode: invert prose AND brighten
                      // the body/heading/bold/code tokens. prose-invert's
                      // default `--tw-prose-invert-body: zinc-300` lands at
                      // ~5.3:1 against bg-zinc-700 — passes AA but reads
                      // washed out next to the user bubble's crisp
                      // white-on-blue (~10:1). Push body to zinc-100 so the
                      // agent text matches that crispness.
                      : "dark:prose-invert dark:[--tw-prose-invert-body:theme(colors.zinc.100)] dark:[--tw-prose-invert-headings:theme(colors.white)] dark:[--tw-prose-invert-bold:theme(colors.white)] dark:[--tw-prose-invert-code:theme(colors.zinc.100)]"
                  }`}
                >
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
              <div className={`text-[9px] mt-1 ${msg.role === "user" ? "text-white/70" : "text-ink-mid"}`}>
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
            <div className="bg-surface-card/50 border border-line/30 rounded-lg px-3 py-2 max-w-[85%]">
              <div className="flex items-center gap-2 text-xs text-ink-mid">
                <span className="flex gap-0.5">
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "0ms" }} />
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "150ms" }} />
                  <span className="w-1.5 h-1.5 bg-zinc-500 rounded-full motion-safe:animate-bounce" style={{ animationDelay: "300ms" }} />
                </span>
                {thinkingElapsed}s
              </div>
              {activityLog.length > 0 && (
                <div className="mt-1.5 text-[9px] text-ink-soft space-y-0.5">
                  <div className="text-ink-mid">Processing with {runtimeDisplayName(data.runtime)}...</div>
                  {activityLog.map((line, i) => (
                    <div key={line + i} className="pl-2 border-l border-line">◇ {line}</div>
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
            <span className="text-[10px] text-bad">{error}</span>
            {!isOnline && (
              <button
                onClick={() => setConfirmRestart(true)}
                className="text-[11px] px-2 py-0.5 bg-red-800/40 text-bad rounded hover:bg-red-700/50"
              >
                Restart
              </button>
            )}
          </div>
        </div>
      )}

      {/* Input */}
      <div className="p-3 border-t border-line">
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
            className="p-2 bg-surface-card hover:bg-surface-card border border-line rounded-lg text-ink-mid hover:text-ink transition-colors shrink-0 disabled:opacity-40"
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
            className="flex-1 bg-surface-card border border-line rounded-lg px-3 py-2 text-xs text-ink placeholder-ink-soft dark:bg-zinc-800 dark:border-zinc-600 dark:placeholder-zinc-500 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 resize-none disabled:opacity-50"
          />
          <button
            onClick={sendMessage}
            disabled={(!input.trim() && pendingFiles.length === 0) || !agentReachable || sending || uploading}
            className="px-4 py-2 bg-accent-strong hover:bg-accent text-xs font-medium rounded-lg text-white disabled:opacity-30 transition-colors shrink-0"
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
