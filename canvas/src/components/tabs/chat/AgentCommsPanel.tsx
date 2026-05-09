"use client";

import { useState, useEffect, useLayoutEffect, useMemo, useRef, useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "@/lib/api";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { showToast } from "../../Toaster";
import { extractResponseText, extractRequestText } from "./message-parser";
import { inferA2AErrorHint } from "./a2aErrorHint";

export interface ActivityEntry {
  id: string;
  activity_type: string;
  source_id: string | null;
  target_id: string | null;
  method: string | null;
  summary: string | null;
  request_body: Record<string, unknown> | null;
  response_body: Record<string, unknown> | null;
  status: string;
  created_at: string;
}

interface CommMessage {
  id: string;
  /** UI-facing flow from THIS workspace's point of view:
   *
   *    "out" — this workspace either initiated the call (a2a_send)
   *            OR self-logged the reply from a peer it had called
   *            (a2a_receive with source_id == workspaceId).
   *    "in"  — a peer initiated the call to us (a2a_receive with
   *            source_id != workspaceId).
   *
   *  Distinct from activity_type because the agent runtime self-
   *  logs its outbound calls' replies as `a2a_receive` rows; without
   *  this normalisation the UI labels would render those as
   *  incoming ("← From X") and right-justify them on the wrong
   *  side, even though from the user's perspective the call WAS
   *  outgoing. See toCommMessage for the resolution rules. */
  flow: "in" | "out";
  peerName: string;
  peerId: string;
  text: string;
  responseText: string | null;
  /** "ok" | "error" — surfaces failed deliveries with their own
   *  visual treatment + recovery actions instead of an opaque
   *  "[A2A_ERROR]" body the user can't act on. */
  status: string;
  timestamp: string;
}

function resolveName(id: string): string {
  const nodes = useCanvasStore.getState().nodes;
  const node = nodes.find((n) => n.id === id);
  return (node?.data as WorkspaceNodeData)?.name || id.slice(0, 8);
}

export function toCommMessage(entry: ActivityEntry, workspaceId: string): CommMessage | null {
  // delegation activity rows are written by the platform's /delegate
  // handler. Two methods:
  //   - "delegate"        — the initial outbound; status pending/dispatched
  //   - "delegate_result" — the eventual reply; status completed/queued/failed
  //
  // Flow direction: even though both rows have source_id=us (the
  // platform writes them on our row), the CONVERSATIONAL direction
  // differs. 'delegate' is us asking the peer; 'delegate_result' is
  // the peer's reply coming back. Render them as alternating bubbles
  // (out + in) so the user sees a chat-like back-and-forth instead
  // of a one-sided wall of "→ To X" rows.
  //
  // Text content: the platform's `summary` is boilerplate
  // ("Delegating to <UUID>" / "Delegation queued — target at
  // capacity") — useful for an audit log, useless in a chat UI.
  // Prefer the real payload:
  //   - outbound: request_body.task (the task text the agent sent)
  //   - inbound:  response_body.response_preview (the peer's reply text)
  // Falls back to a name-resolved summary when the payload is empty.
  if (entry.activity_type === "delegation") {
    const peerId = entry.target_id || "";
    if (!peerId) return null;
    const isResult = entry.method === "delegate_result";
    const peerName = resolveName(peerId);

    let text: string;
    if (isResult) {
      const rb = entry.response_body as Record<string, unknown> | null;
      const replyText =
        (typeof rb?.response_preview === "string" && rb.response_preview) ||
        (typeof rb?.text === "string" && rb.text) ||
        "";
      if (replyText) {
        text = replyText;
      } else if (entry.status === "queued") {
        // No actual reply yet — peer's a2a-proxy queued the call;
        // show what the user needs to know without the boilerplate.
        text = `Queued — ${peerName} is busy on a prior task, reply will arrive when they're free`;
      } else if (entry.status === "failed") {
        text = entry.summary || `Delegation to ${peerName} failed`;
      } else {
        text = entry.summary || "(no reply)";
      }
    } else {
      const reqTask = (entry.request_body as Record<string, unknown> | null)?.task;
      text = (typeof reqTask === "string" && reqTask) || `Delegating to ${peerName}`;
    }

    return {
      id: entry.id,
      flow: isResult ? "in" : "out",
      peerName,
      peerId,
      text,
      // Result text is now the primary `text` (above), so don't
      // duplicate it as responseText — that would render a divider
      // line under the reply with the same content below.
      responseText: null,
      status: entry.status || "ok",
      timestamp: entry.created_at,
    };
  }

  // a2a_receive activity rows come in two shapes:
  //
  //   1. Real incoming call (a peer called us): source_id = the peer,
  //      target_id = us. peerId is source_id, flow is "in".
  //
  //   2. Self-logged response to an outbound call (the workspace's own
  //      runtime calls report_activity("a2a_receive", ...) after
  //      delegating; see workspace/a2a_tools.py:181). source_id =
  //      our own workspace_id, target_id = the peer that replied.
  //      peerId must come from target_id (otherwise the peer-name
  //      resolves to "us" and Restart would target THIS workspace),
  //      and flow is "out" — from the user's perspective this row
  //      belongs to the outbound thread, not an incoming one.
  //
  // a2a_send rows are always outbound from us: source_id = us,
  // target_id = the peer.
  const isSendActivity = entry.activity_type === "a2a_send";
  const isSelfLoggedReceive =
    entry.activity_type === "a2a_receive" && entry.source_id === workspaceId;
  const flow: "in" | "out" = isSendActivity || isSelfLoggedReceive ? "out" : "in";
  const peerId =
    isSendActivity || isSelfLoggedReceive
      ? entry.target_id || ""
      : entry.source_id || "";
  if (!peerId) return null;

  const text = extractRequestText(entry.request_body) || entry.summary || "";
  const responseText = entry.response_body ? extractResponseText(entry.response_body) : null;

  return {
    id: entry.id,
    flow,
    peerName: resolveName(peerId),
    peerId,
    text,
    responseText,
    status: entry.status || "ok",
    timestamp: entry.created_at,
  };
}

/** Strip the [A2A_ERROR] sentinel prefix the workspace runtime adds
 *  to failed delegation responses, so the UI can render the underlying
 *  message (or fall back to a generic explanation when the inner text
 *  is empty — currently common because httpx exceptions often
 *  stringify as ""). */
const A2A_ERROR_PREFIX = "[A2A_ERROR]";

function unwrapErrorText(raw: string | null): string {
  if (!raw) return "";
  const trimmed = raw.trim();
  if (trimmed.startsWith(A2A_ERROR_PREFIX)) {
    return trimmed.slice(A2A_ERROR_PREFIX.length).trim();
  }
  return trimmed;
}

// inferA2AErrorHint moved to ./a2aErrorHint so the Activity tab and
// this panel render identical hints for the same symptom.

export function AgentCommsPanel({ workspaceId }: { workspaceId: string }) {
  const [messages, setMessages] = useState<CommMessage[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Dedup by timestamp+type+peer to handle API load + WebSocket race
  const seenKeys = useRef(new Set<string>());
  const bottomRef = useRef<HTMLDivElement>(null);
  // Mirrors the my-chat scroll behaviour from ChatTab (PR #2903) —
  // smooth-scroll on a long history gets interrupted by concurrent
  // renders and lands the panel mid-conversation. Switch the first
  // arrival to instant; subsequent appends animate.
  const hasInitialScrollRef = useRef(false);

  // Load history. Extracted so the error-state retry button can
  // re-invoke without remount. ChatTab uses the same shape
  // (loadInitial → loadError state → retry button).
  const loadInitial = useCallback(() => {
    setLoading(true);
    setLoadError(null);
    seenKeys.current.clear();
    api.get<ActivityEntry[]>(`/workspaces/${workspaceId}/activity?source=agent&limit=50`)
      .then((entries) => {
        const filtered = (entries ?? [])
          .filter((e) =>
            e.activity_type === "a2a_send" ||
            e.activity_type === "a2a_receive" ||
            e.activity_type === "delegation",
          )
          .reverse();
        const msgs: CommMessage[] = [];
        for (const e of filtered) {
          // Per-row try/catch so a single malformed activity row
          // (e.g. unexpected request_body shape) doesn't kill the
          // batch — the previous code threw out of the for-loop and
          // setMessages([3 items]) never ran, leaving the panel
          // stuck on the empty state with no diagnostic in the
          // console because the outer .catch silently swallowed
          // everything.
          try {
            const m = toCommMessage(e, workspaceId);
            if (m) {
              const key = `${m.timestamp}:${m.flow}:${m.peerId}`;
              msgs.push(m);
              seenKeys.current.add(key);
            }
          } catch (rowErr) {
            console.warn(
              "AgentCommsPanel: failed to map activity row",
              { id: e.id, type: e.activity_type, err: rowErr },
            );
          }
        }
        setMessages(msgs);
        setLoading(false);
      })
      .catch((err) => {
        // Surface the failure in the console so a stuck panel is
        // diagnosable without a debugger. Previous bare
        // `.catch(() => setLoading(false))` swallowed every load
        // failure (network errors, JSON parse errors, throws inside
        // the .then body) — the panel just sat on the empty state
        // with zero signal.
        console.warn("AgentCommsPanel: load activity failed", err);
        setLoadError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      });
  }, [workspaceId]);

  useEffect(() => {
    loadInitial();
  }, [loadInitial]);

  // Live updates routed through the global ReconnectingSocket. The
  // previous pattern of `new WebSocket(WS_URL)` per panel had no
  // onclose / no reconnect, so any drop (idle timeout, browser
  // background-tab throttle) silently stopped delivering events until
  // the panel re-mounted. Routing through useSocketEvent inherits the
  // store socket's reconnect, backoff, and HTTP fallback for free.
  useSocketEvent((msg) => {
    try {
      if (msg.workspace_id !== workspaceId) return;

        // Two live-update paths:
        //   1. ACTIVITY_LOGGED — fired by the LogActivity helper for
        //      a2a_send / a2a_receive (and delegation rows IF the
        //      delegation handler is ever refactored to use it). Today
        //      the platform's delegation handlers do direct INSERT
        //      INTO activity_logs WITHOUT firing ACTIVITY_LOGGED, so
        //      the delegation branch here is defensive — it'd light
        //      up automatically the day delegation handlers are
        //      refactored to call LogActivity.
        //   2. DELEGATION_SENT / DELEGATION_STATUS / DELEGATION_COMPLETE
        //      / DELEGATION_FAILED — fired by the platform's delegation
        //      handlers directly. These are the ONLY live signals the
        //      panel currently has for delegation rows; the GET on
        //      mount (which reads from activity_logs) shows them, but
        //      without this branch, nothing surfaced live until the
        //      next remount. Synthesise an ActivityEntry from the
        //      payload so toCommMessage's existing delegation branch
        //      handles them identically to the GET path.
        let entry: ActivityEntry | null = null;
        if (msg.event === "ACTIVITY_LOGGED") {
          const p = msg.payload || {};
          const type = p.activity_type as string;
          const sourceId = p.source_id as string | null;
          if (!sourceId) return; // canvas-initiated, not agent comms
          if (type !== "a2a_send" && type !== "a2a_receive" && type !== "delegation") return;
          entry = {
            id: p.id as string || crypto.randomUUID(),
            activity_type: type,
            source_id: sourceId,
            target_id: p.target_id as string | null,
            method: p.method as string | null,
            summary: p.summary as string | null,
            request_body: p.request_body as Record<string, unknown> | null,
            response_body: p.response_body as Record<string, unknown> | null,
            status: p.status as string || "ok",
            created_at: msg.timestamp || new Date().toISOString(),
          };
        } else if (
          msg.event === "DELEGATION_SENT" ||
          msg.event === "DELEGATION_STATUS" ||
          msg.event === "DELEGATION_COMPLETE" ||
          msg.event === "DELEGATION_FAILED"
        ) {
          const p = msg.payload || {};
          const targetId = (p.target_id as string) || "";
          if (!targetId) return;
          // Map event → status. DELEGATION_STATUS payload includes its
          // own `status` field (queued / dispatched). Other events have
          // implicit status: SENT → pending, COMPLETE → completed,
          // FAILED → failed.
          //
          // Populate request_body / response_body from the payload so
          // toCommMessage's delegation branch can read the actual
          // task / reply text via the same code path the GET-on-mount
          // uses. Without this, live-pushed bubbles would fall back
          // to the boilerplate summary ("Delegating to <id>") instead
          // of the real text.
          let status: string;
          let summary: string;
          let requestBody: Record<string, unknown> | null = null;
          let responseBody: Record<string, unknown> | null = null;
          if (msg.event === "DELEGATION_STATUS") {
            status = (p.status as string) || "queued";
            summary = `Delegation ${status}`;
          } else if (msg.event === "DELEGATION_COMPLETE") {
            status = "completed";
            const preview = (p.response_preview as string) || "";
            summary = `Delegation completed (${preview.slice(0, 60)})`;
            responseBody = { response_preview: preview };
          } else if (msg.event === "DELEGATION_FAILED") {
            status = "failed";
            summary = `Delegation failed: ${(p.error as string) || "unknown"}`;
          } else {
            status = "pending";
            // DELEGATION_SENT carries `task_preview` (truncated to 100
            // chars at broadcast time in delegation.go). Surface as
            // request_body.task so the inbound bubble shows what was
            // actually delegated, not the UUID stub summary.
            const taskPreview = (p.task_preview as string) || "";
            summary = `Delegating to ${(p.target_id as string)?.slice(0, 8) || "peer"}`;
            if (taskPreview) {
              requestBody = { task: taskPreview };
            }
          }
          entry = {
            id: (p.delegation_id as string) || crypto.randomUUID(),
            activity_type: "delegation",
            source_id: workspaceId,
            target_id: targetId,
            method: msg.event === "DELEGATION_SENT" ? "delegate" : "delegate_result",
            summary,
            request_body: requestBody,
            response_body: responseBody,
            status,
            created_at: msg.timestamp || new Date().toISOString(),
          };
        } else {
          return;
        }

        const m = toCommMessage(entry, workspaceId);
        if (m) {
          const key = `${m.timestamp}:${m.flow}:${m.peerId}`;
          if (seenKeys.current.has(key)) return;
          seenKeys.current.add(key);
          setMessages((prev) => [...prev, m]);
        }
    } catch { /* ignore */ }
  });

  // useLayoutEffect (not useEffect) so the scroll runs BEFORE paint —
  // otherwise the user sees the panel jump for one frame on every
  // append. Mirrors ChatTab's MyChatPanel scroll block.
  useLayoutEffect(() => {
    if (!hasInitialScrollRef.current && messages.length > 0) {
      // Instant on first arrival — smooth-scroll on a long history
      // gets interrupted by concurrent renders and lands the panel
      // mid-conversation (the chat-opens-in-middle bug class).
      hasInitialScrollRef.current = true;
      bottomRef.current?.scrollIntoView({ behavior: "instant" as ScrollBehavior });
      return;
    }
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  if (loading) {
    return <div className="text-xs text-ink-soft text-center py-8">Loading agent communications...</div>;
  }

  if (loadError !== null && messages.length === 0) {
    // Mirrors ChatTab my-chat error UI — surfaces the load failure
    // with a retry button instead of silently rendering empty state.
    return (
      <div
        role="alert"
        className="mx-2 mt-2 rounded-lg border border-red-800/50 bg-red-950/30 px-3 py-2.5"
      >
        <p className="text-[11px] text-bad mb-1.5">
          Failed to load agent communications: {loadError}
        </p>
        <button
          onClick={loadInitial}
          className="text-[10px] px-2 py-0.5 rounded bg-red-800/40 text-bad hover:bg-red-700/50 transition-colors"
        >
          Retry
        </button>
      </div>
    );
  }

  if (messages.length === 0) {
    return (
      <div className="text-xs text-ink-soft text-center py-8">
        No agent-to-agent communications yet.
        <br />
        <span className="text-ink-soft">Delegations and peer messages will appear here.</span>
      </div>
    );
  }

  return <GroupedCommsView messages={messages} bottomRef={bottomRef} />;
}

// ALL_PEERS is the sentinel selectedPeerId value for "show every peer
// in one chronological feed" — the panel's pre-grouping default.
// Picked to be a value no real peerId can collide with (workspace IDs
// are UUIDs).
export const ALL_PEERS = "__all__";

/** PeerSummary is one entry in the sub-tab bar — the per-peer
 *  message count + most-recent timestamp used for ordering. Exported
 *  so the sort/count behaviour can be unit-tested without React. */
export interface PeerSummary {
  peerId: string;
  peerName: string;
  count: number;
  lastTs: string;
}

/** buildPeerSummary collapses the flat message list into per-peer
 *  rows, sorted by most-recent activity descending. Order matches
 *  Slack/Linear's DM list — active conversations rise to the top.
 *  Pure function so the sort + count behaviour is testable without
 *  rendering the panel. */
export function buildPeerSummary(messages: CommMessage[]): PeerSummary[] {
  const acc = new Map<string, PeerSummary>();
  for (const m of messages) {
    const existing = acc.get(m.peerId);
    if (existing) {
      existing.count += 1;
      if (m.timestamp > existing.lastTs) existing.lastTs = m.timestamp;
    } else {
      acc.set(m.peerId, {
        peerId: m.peerId,
        peerName: m.peerName,
        count: 1,
        lastTs: m.timestamp,
      });
    }
  }
  return Array.from(acc.values()).sort((a, b) => (a.lastTs < b.lastTs ? 1 : -1));
}

/** GroupedCommsView renders the messages list with a peer-keyed
 *  sub-tab bar at the top so the user can drill into one DD↔X thread
 *  at a time instead of reading a single chronological mix.
 *
 *  Tab list derivation: walk the messages once, count per-peer, sort
 *  by most-recent timestamp DESC so the active conversations rise to
 *  the top. "All" stays pinned as the leftmost tab. */
function GroupedCommsView({
  messages,
  bottomRef,
}: {
  messages: CommMessage[];
  bottomRef: React.RefObject<HTMLDivElement | null>;
}) {
  const [selectedPeerId, setSelectedPeerId] = useState<string>(ALL_PEERS);

  // Build per-peer summary: count + most-recent timestamp + display
  // name. One pass over messages — O(n). Logic lives in a pure
  // helper so it's unit-testable without rendering the panel.
  const peerSummary = useMemo(() => buildPeerSummary(messages), [messages]);

  // Auto-prune: if the user had selected a peer and that peer no
  // longer has messages (rare — only happens if dedupe removes the
  // last bubble for them), fall back to "All" rather than rendering
  // an empty thread.
  useEffect(() => {
    if (selectedPeerId === ALL_PEERS) return;
    if (!peerSummary.some((p) => p.peerId === selectedPeerId)) {
      setSelectedPeerId(ALL_PEERS);
    }
  }, [peerSummary, selectedPeerId]);

  const visible = useMemo(() => {
    if (selectedPeerId === ALL_PEERS) return messages;
    return messages.filter((m) => m.peerId === selectedPeerId);
  }, [messages, selectedPeerId]);

  return (
    <div className="flex flex-col h-full min-h-0">
      <PeerTabs
        peers={peerSummary}
        totalCount={messages.length}
        selectedPeerId={selectedPeerId}
        onSelect={setSelectedPeerId}
      />
      <div className="flex-1 overflow-y-auto p-3 space-y-2">
        {visible.map((msg) =>
          // Only render the error UI when there is NO usable response
          // content. A "error" status from the platform means the HTTP
          // transport layer had a problem — but the agent response text
          // may have arrived and been stored in response_body.text.
          // Delegation results set responseText via extractResponseText
          // once that function learned to parse body.text, so checking
          // !msg.responseText here correctly identifies "no actual reply
          // was received" vs. "reply arrived but status=error".
          //
          // Without this guard, successful delegation results were
          // rendered as error banners, PMs saw "restart" prompts and
          // restarted working agents, and retry storms formed as the
          // platform re-delivered the same completed work (issue #159).
          msg.status === "error" && !msg.responseText ? (
            <ErrorMessage key={msg.id} msg={msg} />
          ) : (
            <NormalMessage key={msg.id} msg={msg} />
          ),
        )}
        <WaitingBubbles visible={visible} />
        <div ref={bottomRef} />
      </div>
    </div>
  );
}

/** PeerTabs renders the horizontally-scrolling sub-tab bar.
 *  Keyboard: ArrowLeft / ArrowRight cycle peers (matches the existing
 *  My Chat / Agent Comms tab pattern in ChatTab). */
function PeerTabs({
  peers,
  totalCount,
  selectedPeerId,
  onSelect,
}: {
  peers: Array<{ peerId: string; peerName: string; count: number; lastTs: string }>;
  totalCount: number;
  selectedPeerId: string;
  onSelect: (peerId: string) => void;
}) {
  // "All" + each peer, in tab-bar order. Built once per render and
  // used both for click handling and for ArrowLeft/ArrowRight cycling.
  const ids = [ALL_PEERS, ...peers.map((p) => p.peerId)];

  return (
    <div
      role="tablist"
      aria-label="Peer threads"
      className="flex border-b border-line/40 bg-surface-sunken/30 px-2 shrink-0 overflow-x-auto"
      onKeyDown={(e) => {
        const idx = ids.indexOf(selectedPeerId);
        if (idx < 0) return;
        if (e.key === "ArrowRight") {
          e.preventDefault();
          onSelect(ids[(idx + 1) % ids.length]);
        } else if (e.key === "ArrowLeft") {
          e.preventDefault();
          onSelect(ids[(idx - 1 + ids.length) % ids.length]);
        }
      }}
    >
      <PeerTabButton
        active={selectedPeerId === ALL_PEERS}
        onClick={() => onSelect(ALL_PEERS)}
        label="All"
        count={totalCount}
      />
      {peers.map((p) => (
        <PeerTabButton
          key={p.peerId}
          active={selectedPeerId === p.peerId}
          onClick={() => onSelect(p.peerId)}
          label={p.peerName}
          count={p.count}
        />
      ))}
    </div>
  );
}

function PeerTabButton({
  active,
  onClick,
  label,
  count,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
  count: number;
}) {
  return (
    <button
      role="tab"
      aria-selected={active}
      tabIndex={active ? 0 : -1}
      onClick={onClick}
      className={`shrink-0 px-3 py-1.5 text-[10px] font-medium transition-colors whitespace-nowrap ${
        active
          ? "border-b-2 border-cyan-500 text-cyan-200"
          : "border-b-2 border-transparent text-ink-soft hover:text-ink-mid"
      }`}
    >
      {label} <span className="text-[9px] text-ink-soft">({count})</span>
    </button>
  );
}

/** WaitingBubbles renders one "typing" bubble per peer that has an
 *  in-flight outbound delegation — i.e., the most recent outbound
 *  message to that peer is still pending or queued and no later inbound
 *  reply has arrived. Mirrors the bouncing-dots indicator in ChatTab so
 *  the operator sees the same visual cue regardless of whether they're
 *  watching their own chat or a peer thread.
 *
 *  Why "per peer" not "one global": when multiple delegations are in
 *  flight to different peers (common during fan-out), one shared
 *  spinner under-reports — the user can't tell whether ALL peers are
 *  still working or only the visible ones. Per-peer matches Slack-style
 *  typing indicators and keeps the signal honest.
 *
 *  Why we look at the LAST per-peer message: once a peer replies (an
 *  "in" bubble lands), the corresponding "out" bubble is no longer the
 *  tail — even if status hasn't been mutated to "completed", the inbound
 *  reply means the wait is over. Looking at the tail collapses both
 *  cases into one rule.
 */
function WaitingBubbles({ visible }: { visible: CommMessage[] }) {
  // Group by peer, keep only the chronologically-last message per peer,
  // emit a bubble when that tail is an outbound pending/queued.
  const tailByPeer = new Map<string, CommMessage>();
  for (const m of visible) {
    const prev = tailByPeer.get(m.peerId);
    if (!prev || m.timestamp > prev.timestamp) tailByPeer.set(m.peerId, m);
  }
  const waitingPeers = Array.from(tailByPeer.values()).filter(
    (m) => m.flow === "out" && (m.status === "pending" || m.status === "queued"),
  );
  if (waitingPeers.length === 0) return null;
  return (
    <>
      {waitingPeers.map((m) => (
        <div
          key={`waiting-${m.peerId}`}
          className="flex justify-end"
          // Outbound thread → right-justified to match the "out" bubble
          // alignment, so the dots feel like they belong to the message
          // they're replying to.
        >
          <div
            className="max-w-[85%] rounded-lg px-3 py-2 text-xs bg-cyan-900/30 border border-cyan-700/20"
            // role+aria-label so screen readers announce the wait;
            // matches the announcing pattern used by Toaster.
            role="status"
            aria-label={`Waiting for reply from ${m.peerName}`}
          >
            <div className="text-[9px] text-ink-soft mb-1">→ To {m.peerName}</div>
            <span className="flex items-center gap-2 text-ink-mid">
              <span className="flex gap-0.5" aria-hidden="true">
                <span
                  className="w-1.5 h-1.5 bg-cyan-300/70 rounded-full motion-safe:animate-bounce"
                  style={{ animationDelay: "0ms" }}
                />
                <span
                  className="w-1.5 h-1.5 bg-cyan-300/70 rounded-full motion-safe:animate-bounce"
                  style={{ animationDelay: "150ms" }}
                />
                <span
                  className="w-1.5 h-1.5 bg-cyan-300/70 rounded-full motion-safe:animate-bounce"
                  style={{ animationDelay: "300ms" }}
                />
              </span>
              <span className="text-[10px]">
                {m.status === "queued"
                  ? `${m.peerName} is busy — reply will arrive when they're free`
                  : `Waiting for ${m.peerName}…`}
              </span>
            </span>
          </div>
        </div>
      ))}
    </>
  );
}

function NormalMessage({ msg }: { msg: CommMessage }) {
  return (
    <div className={`flex ${msg.flow === "out" ? "justify-end" : "justify-start"}`}>
      <div
        className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
          msg.flow === "out"
            ? "bg-cyan-900/30 text-cyan-100 border border-cyan-700/20"
            : "bg-surface-card/80 text-ink border border-line/30"
        }`}
      >
        <div className="text-[9px] text-ink-soft mb-1">
          {msg.flow === "out" ? `→ To ${msg.peerName}` : `← From ${msg.peerName}`}
        </div>
        {msg.text ? (
          // Outgoing bubble (cyan-900) is dark in both themes → prose-invert default.
          // Incoming bubble (surface-card) themes light → only invert in dark.
          <MarkdownBody
            className="text-ink-mid"
            invert={msg.flow === "out" ? "always" : "dark-only"}
          >
            {msg.text}
          </MarkdownBody>
        ) : (
          <div className="text-ink-mid">(no message text)</div>
        )}
        {msg.responseText && (
          <MarkdownBody
            className="mt-1.5 pt-1.5 border-t border-line/30 text-ink-mid"
            invert={msg.flow === "out" ? "always" : "dark-only"}
          >
            {msg.responseText}
          </MarkdownBody>
        )}
        <div className="text-[9px] text-ink-soft mt-1">
          {new Date(msg.timestamp).toLocaleTimeString()}
        </div>
      </div>
    </div>
  );
}

/** Failure-state row. Replaces the unactionable "X failed [A2A_ERROR]"
 *  bubble with: a clear banner naming the peer, the underlying
 *  error text (if any), an inferred cause hint, and recovery
 *  actions — Restart workspace, Open workspace.
 *
 *  Recovery actions show on BOTH directions because both target the
 *  same peer (toCommMessage now resolves peerId to the peer in
 *  either case): an outbound delivery failure ("we called X and it
 *  errored"), an inbound runtime failure ("X called us and our
 *  reply errored" — rare), or the agent-self-logged "I called X and
 *  got an error back" pattern that is the most common shape. The
 *  user always wants to restart or inspect the failing peer. */
function ErrorMessage({ msg }: { msg: CommMessage }) {
  const selectNode = useCanvasStore((s) => s.selectNode);
  const [restarting, setRestarting] = useState(false);
  const errorText = unwrapErrorText(msg.responseText);
  const hint = inferA2AErrorHint(errorText);

  // Guard against acting on a peer whose workspace has been deleted
  // since this row was logged. Without the guard, restart 404s
  // surface as a generic toast and Open silently sets a dangling
  // selection that renders nothing in the side panel.
  const peerExists = (): boolean => {
    return useCanvasStore.getState().nodes.some((n) => n.id === msg.peerId);
  };

  const handleRestart = async () => {
    if (restarting) return;
    if (!peerExists()) {
      showToast(`${msg.peerName} no longer exists`, "error");
      return;
    }
    setRestarting(true);
    try {
      await api.post(`/workspaces/${msg.peerId}/restart`, {});
      showToast(`Restarting ${msg.peerName}…`, "success");
    } catch (e) {
      showToast(
        `Restart failed: ${e instanceof Error ? e.message : "unknown error"}`,
        "error",
      );
    } finally {
      setRestarting(false);
    }
  };

  const handleOpen = () => {
    if (!peerExists()) {
      showToast(`${msg.peerName} no longer exists`, "error");
      return;
    }
    selectNode(msg.peerId);
  };

  return (
    <div className={`flex ${msg.flow === "out" ? "justify-end" : "justify-start"}`}>
      <div className="max-w-[85%] rounded-lg border border-red-800/50 bg-red-950/30 px-3 py-2 text-xs">
        <div className="flex items-center gap-1.5 text-[10px] text-bad font-semibold uppercase tracking-wide mb-1.5">
          <span aria-hidden="true">⚠</span>
          {msg.flow === "out"
            ? `Failed to deliver to ${msg.peerName}`
            : `${msg.peerName} returned an error`}
        </div>

        {msg.text && (
          <div className="text-[10px] text-ink-soft mb-1.5">
            <span className="uppercase tracking-wide">Task</span>
            <MarkdownBody className="text-ink-mid">{msg.text}</MarkdownBody>
          </div>
        )}

        <div className="rounded bg-surface/60 border border-red-900/40 px-2 py-1.5 mb-1.5">
          <div className="text-[9px] uppercase tracking-wide text-bad mb-0.5">
            Underlying error
          </div>
          <code className="text-[11px] font-mono text-red-200 whitespace-pre-wrap break-words">
            {errorText || "(no detail returned)"}
          </code>
        </div>

        <p className="text-[10px] text-ink-mid leading-snug mb-2">{hint}</p>

        {msg.peerId && (
          <div className="flex flex-wrap items-center gap-1.5">
            <button
              type="button"
              onClick={handleRestart}
              disabled={restarting}
              className="px-2 py-0.5 rounded bg-red-900/50 hover:bg-red-800/60 border border-red-700/40 text-[10px] text-red-200 disabled:opacity-50 transition-colors"
            >
              {restarting ? "Restarting…" : `Restart ${msg.peerName}`}
            </button>
            <button
              type="button"
              onClick={handleOpen}
              className="px-2 py-0.5 rounded bg-surface-card hover:bg-surface-card border border-line/50 text-[10px] text-ink-mid transition-colors"
            >
              Open {msg.peerName}
            </button>
          </div>
        )}

        <div className="text-[9px] text-ink-soft mt-1.5">
          {new Date(msg.timestamp).toLocaleTimeString()}
        </div>
      </div>
    </div>
  );
}

/** Tiny markdown wrapper matching ChatTab's My Chat styling. Same
 *  remark-gfm pipeline (tables, strikethrough, task lists) plus the
 *  prose tweaks that keep paragraphs tight inside a small bubble.
 *  Code blocks get an `overflow-x-auto` so a long line of code doesn't
 *  blow out the bubble's max-width — agent-to-agent replies routinely
 *  ship code samples and JSON.
 *
 *  `invert` controls the prose color flip:
 *  - "always": container bg is dark in BOTH themes (cyan-900, red-950),
 *    so prose always wants light body text.
 *  - "dark-only": container bg uses a theming token that goes light in
 *    light mode (e.g. bg-surface-card). Prose only inverts in dark
 *    mode; light mode keeps default dark prose colors against the
 *    light bg. Without this, light mode rendered light text on light
 *    bg = invisible markdown. */
function MarkdownBody({
  children,
  className,
  invert = "always",
}: {
  children: string;
  className?: string;
  invert?: "always" | "dark-only";
}) {
  const proseInvert = invert === "always" ? "prose-invert" : "dark:prose-invert";
  return (
    <div
      className={`prose prose-sm ${proseInvert} max-w-none [&>p]:mb-1 [&>p:last-child]:mb-0 [&_pre]:overflow-x-auto [&_table]:block [&_table]:overflow-x-auto ${className ?? ""}`}
    >
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{children}</ReactMarkdown>
    </div>
  );
}
