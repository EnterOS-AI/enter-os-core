"use client";

// 04 · Chat — message thread + composer + sub-tabs.
// Wired to the same /workspaces/:id/a2a (method message/send) endpoint
// that the desktop ChatTab uses, but with a slimmer surface: no
// attachments, no A2A topology overlay, no conversation tracing.

import { useEffect, useMemo, useRef, useState } from "react";

import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";

import { toMobileAgent } from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, usePalette } from "./palette";
import { Icons, StatusDot, TierChip } from "./primitives";

interface ChatMessage {
  id: string;
  role: "user" | "agent" | "system";
  text: string;
  ts: string;
}

const formatStoredTimestamp = (iso: string): string => {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
};

type SubTab = "my" | "a2a";

interface A2AResponseShape {
  result?: {
    parts?: Array<{ kind?: string; text?: string }>;
  };
  error?: { message?: string };
}

// Wire shape for GET /workspaces/:id/chat-history (chat_history.go → ChatHistoryResponse).
interface ApiChatMessage {
  id: string;
  role: string; // "user" | "agent" | "system"
  content: string;
  timestamp: string;
  attachments?: Array<{ name: string; uri: string; mimeType?: string; size?: number }>;
}

interface ChatHistoryResponse {
  messages: ApiChatMessage[];
  reached_end: boolean;
}

const formatTime = (date: Date) =>
  date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });

export function MobileChat({
  agentId,
  dark,
  onBack,
}: {
  agentId: string;
  dark: boolean;
  onBack: () => void;
}) {
  const p = usePalette(dark);
  // Selecting `nodes` stably avoids the `.find()` anti-pattern that
  // creates a new return value on every store update (React error #185).
  const nodes = useCanvasStore((s) => s.nodes);
  const node = useMemo(() => nodes.find((n) => n.id === agentId), [nodes, agentId]);
  // Bootstrap from the canvas store's per-workspace message buffer so the
  // user sees their prior thread on entry. The store is updated by the
  // socket → ChatTab flows the desktop runs; on mobile we read from the
  // same buffer to keep state coherent across viewports.
  // NOTE: selector returns undefined (stable) — do NOT use ?? [] here,
  // that creates a new [] reference on every store update when the key is
  // absent, causing infinite re-render (React error #185).
  const storedMessages = useCanvasStore((s) => s.agentMessages[agentId]);
  // Start empty — history is loaded via useEffect below.
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [draft, setDraft] = useState("");
  const [tab, setTab] = useState<SubTab>("my");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true); // history is loading on mount
  const [historyError, setHistoryError] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Synchronous re-entry guard. `setSending(true)` schedules a state
  // update but doesn't flush before a second tap can fire send() — a ref
  // mirrors the desktop ChatTab pattern (sendInFlightRef) and closes the
  // double-send race a stale `sending` lets through.
  const sendInFlightRef = useRef(false);
  const composerRef = useRef<HTMLTextAreaElement>(null);
  // Guard: don't treat the initial store population as a live push.
  // Set to false after the first render completes.
  const initDoneRef = useRef(false);

  // Auto-grow the textarea: reset height to 'auto' so the scrollHeight
  // shrinks when the user deletes text, then size to scrollHeight up to
  // a 5-line cap. Beyond the cap, internal scroll kicks in.
  useEffect(() => {
    const el = composerRef.current;
    if (!el) return;
    el.style.height = "auto";
    const next = Math.min(el.scrollHeight, 132); // ~5 lines at 14.5px/1.4
    el.style.height = `${next}px`;
  }, [draft]);

  // Fetch chat history on mount; keep merging live agentMessages while the
  // panel is open. InitDoneRef prevents the initial store snapshot from
  // triggering the live-merge path (the store buffer is populated by
  // ChatTab on desktop, not on mobile — this effect loads history as the
  // mobile-native path).
  useEffect(() => {
    let cancelled = false;

    const mapApiMessage = (m: ApiChatMessage): ChatMessage => ({
      id: m.id,
      role: m.role === "user" ? "user" : "agent",
      text: m.content,
      ts: formatStoredTimestamp(m.timestamp),
    });

    const syncLive = () => {
      const live = useCanvasStore.getState().agentMessages[agentId] ?? [];
      if (live.length > 0) {
        setMessages((prev) => {
          const existingIds = new Set(prev.map((m) => m.id));
          const newOnes = live
            .filter((m) => !existingIds.has(m.id))
            .map((m) => ({
              id: m.id,
              role: "agent" as const,
              text: m.content,
              ts: formatStoredTimestamp(m.timestamp),
            }));
          return newOnes.length > 0 ? [...prev, ...newOnes] : prev;
        });
      }
    };

    const bootstrap = async (): Promise<(() => void) | undefined> => {
      setLoading(true);
      setHistoryError(null);
      try {
        const res = await api.get<ChatHistoryResponse>(
          `/workspaces/${agentId}/chat-history?limit=50`,
        );
        if (cancelled) return;
        const initial = (res.messages ?? []).map(mapApiMessage);
        setMessages(initial);
        // Mark init done BEFORE marking loading=false so any store push
        // that arrives in the same tick is treated as live, not init.
        initDoneRef.current = true;
        setLoading(false);
        // Subscribe to live pushes after init is complete.
        syncLive();
        const unsubscribe = useCanvasStore.subscribe(syncLive);
        return unsubscribe; // returned for cleanup
      } catch (e) {
        if (cancelled) return;
        setHistoryError(e instanceof Error ? e.message : "Failed to load chat history");
        setLoading(false);
        initDoneRef.current = true;
        return undefined;
      }
    };

    let maybeUnsubscribe: (() => void) | undefined;
    bootstrap().then((fn) => { maybeUnsubscribe = fn; });

    return () => {
      cancelled = true;
      if (maybeUnsubscribe) maybeUnsubscribe();
    };
  }, [agentId]);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages]);

  if (!node) {
    return (
      <div
        style={{
          height: "100%",
          background: p.bg,
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          color: p.text3,
          fontSize: 13,
          fontFamily: MOBILE_FONT_SANS,
        }}
      >
        Agent not found.
      </div>
    );
  }
  const a = toMobileAgent(node);
  const reachable = a.status === "online" || a.status === "degraded";

  const send = async () => {
    const text = draft.trim();
    if (!text || sending || !reachable) return;
    if (sendInFlightRef.current) return;
    sendInFlightRef.current = true;
    setDraft("");
    setError(null);
    setSending(true);
    const myMsg: ChatMessage = {
      id: crypto.randomUUID(),
      role: "user",
      text,
      ts: formatTime(new Date()),
    };
    setMessages((m) => [...m, myMsg]);

    try {
      const res = await api.post<A2AResponseShape>(`/workspaces/${agentId}/a2a`, {
        method: "message/send",
        params: {
          message: {
            role: "user",
            messageId: crypto.randomUUID(),
            parts: [{ kind: "text", text }],
          },
        },
      });
      const reply =
        res.result?.parts?.find((part) => part.kind === "text")?.text ?? "";
      if (reply) {
        setMessages((m) => [
          ...m,
          {
            id: crypto.randomUUID(),
            role: "agent",
            text: reply,
            ts: formatTime(new Date()),
          },
        ]);
      } else if (res.error?.message) {
        setError(res.error.message);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to send");
    } finally {
      setSending(false);
      sendInFlightRef.current = false;
    }
  };

  return (
    <div
      style={{
        height: "100%",
        display: "flex",
        flexDirection: "column",
        background: p.bg,
        fontFamily: MOBILE_FONT_SANS,
      }}
    >
      {/* Header */}
      <div
        style={{
          padding: "max(env(safe-area-inset-top), 44px) 14px 10px",
          borderBottom: `0.5px solid ${p.divider}`,
          background: dark ? "rgba(21,20,15,0.85)" : "rgba(246,244,239,0.85)",
          backdropFilter: "blur(14px)",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
          <button
            type="button"
            onClick={onBack}
            aria-label="Back"
            style={{
              width: 36,
              height: 36,
              borderRadius: 999,
              border: "none",
              cursor: "pointer",
              background: "transparent",
              color: p.text2,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {Icons.back({ size: 18 })}
          </button>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
              <StatusDot status={a.status} size={7} dark={dark} halo={false} />
              <span
                style={{
                  fontSize: 15,
                  fontWeight: 600,
                  color: p.text,
                  whiteSpace: "nowrap",
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                }}
              >
                {a.name}
              </span>
              <TierChip tier={a.tier} dark={dark} />
            </div>
            <div
              style={{
                fontSize: 11,
                color: p.text3,
                marginTop: 2,
                fontFamily: MOBILE_FONT_MONO,
              }}
            >
              {a.runtime} · {a.skills} skills
            </div>
          </div>
          <button
            type="button"
            aria-label="More"
            style={{
              width: 36,
              height: 36,
              borderRadius: 999,
              border: "none",
              cursor: "pointer",
              background: "transparent",
              color: p.text2,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {Icons.more({ size: 18 })}
          </button>
        </div>
        {/* Sub-tabs */}
        <div style={{ display: "flex", gap: 18, marginTop: 12, paddingLeft: 4 }}>
          {(
            [
              { id: "my", label: "My Chat" },
              { id: "a2a", label: "Agent Comms" },
            ] as const
          ).map((t) => {
            const on = tab === t.id;
            return (
              <button
                key={t.id}
                type="button"
                onClick={() => setTab(t.id)}
                style={{
                  padding: "4px 0 8px",
                  border: "none",
                  background: "transparent",
                  fontSize: 13.5,
                  cursor: "pointer",
                  color: on ? p.text : p.text3,
                  fontWeight: on ? 600 : 500,
                  borderBottom: on ? `2px solid ${p.accent}` : "2px solid transparent",
                }}
              >
                {t.label}
              </button>
            );
          })}
        </div>
      </div>

      {/* Messages */}
      <div
        ref={scrollRef}
        style={{
          flex: 1,
          overflow: "auto",
          padding: "14px 14px 16px",
          display: "flex",
          flexDirection: "column",
          gap: 8,
        }}
      >
        {tab === "a2a" && (
          <div
            style={{
              padding: "20px 4px",
              textAlign: "center",
              color: p.text3,
              fontSize: 13,
            }}
          >
            Agent Comms — peer-to-peer A2A traffic surfaces in the Comms tab.
          </div>
        )}
        {tab === "my" && loading && (
          <div style={{ padding: "20px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
            <div style={{ marginBottom: 6, opacity: 0.6, animation: "spin 1s linear infinite", display: "inline-block", fontSize: 16 }}>⟳</div>
            <div>Loading chat history…</div>
          </div>
        )}
        {tab === "my" && !loading && historyError && (
          <div
            role="alert"
            style={{
              padding: "14px 4px",
              textAlign: "center",
              color: p.failed,
              fontSize: 13,
            }}
          >
            <div style={{ marginBottom: 8 }}>Could not load chat history.</div>
            <button
              type="button"
              onClick={() => {
                setLoading(true);
                setHistoryError(null);
                api.get(`/workspaces/${agentId}/chat-history?limit=50`).then(
                  (res: unknown) => {
                    const r = res as ChatHistoryResponse;
                    setMessages((r.messages ?? []).map((m) => ({
                      id: m.id,
                      role: m.role === "user" ? "user" : "agent",
                      text: m.content,
                      ts: formatStoredTimestamp(m.timestamp),
                    })));
                    setLoading(false);
                    initDoneRef.current = true;
                  },
                ).catch((e: unknown) => {
                  setHistoryError(e instanceof Error ? e.message : "Failed to load");
                  setLoading(false);
                  initDoneRef.current = true;
                });
              }}
              style={{
                padding: "6px 14px",
                borderRadius: 14,
                border: `0.5px solid ${p.failed}`,
                background: "transparent",
                color: p.failed,
                fontSize: 12,
                cursor: "pointer",
              }}
            >
              Retry
            </button>
          </div>
        )}
        {tab === "my" && !loading && !historyError && messages.length === 0 && (
          <div style={{ padding: "20px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
            Send a message to start chatting.
          </div>
        )}
        {tab === "my" &&
          messages.map((m) => {
            const mine = m.role === "user";
            return (
              <div
                key={m.id}
                style={{
                  display: "flex",
                  justifyContent: mine ? "flex-end" : "flex-start",
                }}
              >
                <div
                  style={{
                    maxWidth: "78%",
                    background: mine ? p.accent : dark ? "#22211c" : "#fff",
                    color: mine ? "#fff" : p.text,
                    border: mine ? "none" : `0.5px solid ${p.border}`,
                    borderRadius: mine ? "18px 18px 4px 18px" : "18px 18px 18px 4px",
                    padding: "9px 13px",
                    fontSize: 14.5,
                    lineHeight: 1.4,
                    overflowWrap: "anywhere",
                  }}
                >
                  {m.text}
                  <div
                    style={{
                      fontSize: 10,
                      marginTop: 4,
                      opacity: mine ? 0.75 : 0.5,
                      fontFamily: MOBILE_FONT_MONO,
                    }}
                  >
                    {m.ts}
                  </div>
                </div>
              </div>
            );
          })}
        {error && (
          <div
            role="alert"
            style={{
              alignSelf: "center",
              padding: "6px 12px",
              borderRadius: 12,
              background: `${p.failed}1a`,
              color: p.failed,
              fontSize: 12,
            }}
          >
            {error}
          </div>
        )}
      </div>

      {/* Footer ID */}
      <div
        style={{
          padding: "0 14px 6px",
          textAlign: "center",
          fontFamily: MOBILE_FONT_MONO,
          fontSize: 9.5,
          color: p.text3,
          letterSpacing: "0.04em",
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
        }}
      >
        {agentId}
      </div>

      {/* Composer */}
      <div
        style={{
          padding: "10px 12px max(env(safe-area-inset-bottom), 16px)",
          borderTop: `0.5px solid ${p.divider}`,
          background: dark ? "rgba(21,20,15,0.92)" : "rgba(246,244,239,0.92)",
          backdropFilter: "blur(14px)",
        }}
      >
        <div
          style={{
            display: "flex",
            alignItems: "flex-end",
            gap: 8,
            background: dark ? "#22211c" : "#fff",
            border: `0.5px solid ${p.border}`,
            borderRadius: 22,
            padding: "6px 6px 6px 12px",
          }}
        >
          <button
            type="button"
            aria-label="Attach"
            style={{
              width: 32,
              height: 32,
              borderRadius: 999,
              border: "none",
              cursor: "pointer",
              background: "transparent",
              color: p.text3,
              flexShrink: 0,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {Icons.attach({ size: 16 })}
          </button>
          <textarea
            ref={composerRef}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              // Enter sends; Shift+Enter inserts a newline. Skip when the
              // IME is composing — pressing Enter to commit a Chinese/
              // Japanese candidate would otherwise dispatch the half-typed
              // message (the same regression the desktop ChatTab guards).
              if (
                e.key === "Enter" &&
                !e.shiftKey &&
                !e.nativeEvent.isComposing &&
                e.keyCode !== 229
              ) {
                e.preventDefault();
                send();
              }
            }}
            placeholder={reachable ? "Send a message…" : `Agent is ${a.status}`}
            disabled={!reachable}
            rows={1}
            style={{
              flex: 1,
              border: "none",
              outline: "none",
              background: "transparent",
              fontSize: 14.5,
              lineHeight: 1.4,
              color: p.text,
              padding: "6px 0",
              fontFamily: "inherit",
              minWidth: 0,
              resize: "none",
              maxHeight: 132,
              overflowY: "auto",
            }}
          />
          <button
            type="button"
            onClick={send}
            disabled={!draft.trim() || !reachable || sending}
            aria-label="Send"
            style={{
              width: 36,
              height: 36,
              borderRadius: 999,
              border: "none",
              cursor: draft.trim() && !sending ? "pointer" : "not-allowed",
              flexShrink: 0,
              background:
                draft.trim() && reachable && !sending
                  ? p.accent
                  : dark
                    ? "#2a2823"
                    : "#ece9e0",
              color: draft.trim() && reachable && !sending ? "#fff" : p.text3,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {Icons.send({ size: 16 })}
          </button>
        </div>
      </div>
    </div>
  );
}
