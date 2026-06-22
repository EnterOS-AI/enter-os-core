"use client";

// 04 · Chat — message thread + composer + sub-tabs.
// Wired to the same /workspaces/:id/a2a (method message/send) endpoint
// that the desktop ChatTab uses. Render parity with desktop ChatTab is
// achieved by reusing its renderers rather than forking a reduced
// mobile path: the Agent Comms sub-tab mounts the same AgentCommsPanel,
// and message attachments route through the same AttachmentPreview
// dispatch the desktop My-Chat bubble uses (#231/#232).

import { useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

import { useCanvasStore } from "@/store/canvas";
import { type ChatAttachment, type ChatMessage, createMessage } from "@/components/tabs/chat/types";
import {
  useChatHistory,
  useChatSend,
  useChatSocket,
} from "@/components/tabs/chat/hooks";
import { AgentCommsPanel } from "@/components/tabs/chat/AgentCommsPanel";
import { AttachmentPreview } from "@/components/tabs/chat/AttachmentPreview";
import { ToolTraceChips } from "@/components/tabs/chat/ToolTraceChips";
import { downloadChatFile } from "@/components/tabs/chat/uploads";

import { toMobileAgent } from "./components";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, usePalette } from "./palette";
import { Icons, StatusDot, TierChip } from "./primitives";

const formatStoredTimestamp = (iso: string): string => {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
};

type SubTab = "my" | "a2a";

function MarkdownBubble({
  children,
  dark,
  accent,
}: {
  children: string;
  dark: boolean;
  accent: string;
}) {
  const codeBg = dark ? "rgba(255,255,255,0.08)" : "rgba(0,0,0,0.06)";
  const codeBlockBg = dark ? "#1a1a1a" : "#f5f5f0";
  const linkColor = accent;
  const quoteBorder = dark ? "rgba(255,250,240,0.15)" : "rgba(40,30,20,0.15)";

  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        p: ({ children }) => (
          <div style={{ margin: "2px 0", lineHeight: "inherit" }}>{children}</div>
        ),
        a: ({ href, children }) => (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            style={{ color: linkColor, textDecoration: "underline" }}
          >
            {children}
          </a>
        ),
        pre: ({ children }) => (
          <pre
            style={{
              background: codeBlockBg,
              padding: "8px 10px",
              borderRadius: 8,
              overflow: "auto",
              fontSize: 12,
              lineHeight: 1.5,
              fontFamily: MOBILE_FONT_MONO,
              margin: "4px 0",
            }}
          >
            {children}
          </pre>
        ),
        code: ({ children, className }) => {
          const isBlock = className != null && String(className).length > 0;
          if (isBlock) {
            return (
              <code style={{ fontFamily: MOBILE_FONT_MONO, fontSize: 12 }}>
                {children}
              </code>
            );
          }
          return (
            <code
              style={{
                background: codeBg,
                padding: "1px 4px",
                borderRadius: 4,
                fontSize: 13,
                fontFamily: MOBILE_FONT_MONO,
              }}
            >
              {children}
            </code>
          );
        },
        ul: ({ children }) => (
          <ul style={{ margin: "4px 0", paddingLeft: 18, listStyle: "disc" }}>
            {children}
          </ul>
        ),
        ol: ({ children }) => (
          <ol style={{ margin: "4px 0", paddingLeft: 18, listStyle: "decimal" }}>
            {children}
          </ol>
        ),
        li: ({ children }) => <li style={{ margin: "2px 0" }}>{children}</li>,
        strong: ({ children }) => (
          <strong style={{ fontWeight: 600 }}>{children}</strong>
        ),
        em: ({ children }) => <em style={{ fontStyle: "italic" }}>{children}</em>,
        h1: ({ children }) => (
          <div style={{ fontSize: 16, fontWeight: 700, margin: "4px 0" }}>{children}</div>
        ),
        h2: ({ children }) => (
          <div style={{ fontSize: 15, fontWeight: 700, margin: "4px 0" }}>{children}</div>
        ),
        h3: ({ children }) => (
          <div style={{ fontSize: 14, fontWeight: 700, margin: "4px 0" }}>{children}</div>
        ),
        h4: ({ children }) => (
          <div style={{ fontSize: 14, fontWeight: 600, margin: "4px 0" }}>{children}</div>
        ),
        h5: ({ children }) => (
          <div style={{ fontSize: 13, fontWeight: 600, margin: "4px 0" }}>{children}</div>
        ),
        h6: ({ children }) => (
          <div style={{ fontSize: 13, fontWeight: 600, margin: "4px 0" }}>{children}</div>
        ),
        blockquote: ({ children }) => (
          <blockquote
            style={{
              borderLeft: `2px solid ${quoteBorder}`,
              margin: "4px 0",
              paddingLeft: 8,
              opacity: 0.85,
            }}
          >
            {children}
          </blockquote>
        ),
        hr: () => (
          <hr
            style={{
              border: "none",
              borderTop: `0.5px solid ${quoteBorder}`,
              margin: "6px 0",
            }}
          />
        ),
        table: ({ children }) => (
          <table
            style={{
              borderCollapse: "collapse",
              fontSize: 13,
              margin: "4px 0",
              width: "100%",
            }}
          >
            {children}
          </table>
        ),
        thead: ({ children }) => <thead style={{ fontWeight: 600 }}>{children}</thead>,
        th: ({ children }) => (
          <th
            style={{
              border: `0.5px solid ${quoteBorder}`,
              padding: "4px 6px",
              textAlign: "left",
            }}
          >
            {children}
          </th>
        ),
        td: ({ children }) => (
          <td
            style={{
              border: `0.5px solid ${quoteBorder}`,
              padding: "4px 6px",
            }}
          >
            {children}
          </td>
        ),
      }}
    >
      {children}
    </ReactMarkdown>
  );
}

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
  const nodes = useCanvasStore((s) => s.nodes);
  const node = useMemo(() => nodes.find((n) => n.id === agentId), [nodes, agentId]);
  const [draft, setDraft] = useState("");
  const [tab, setTab] = useState<SubTab>("my");
  const scrollRef = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [pendingFiles, setPendingFiles] = useState<File[]>([]);
  // Preserve composer state so a genuine send error can restore the draft
  // and attachments instead of silently dropping them (mc#2908 F7).
  const lastComposerRef = useRef<{ text: string; files: File[] } | null>(null);

  const {
    messages,
    loading: historyLoading,
    loadError: historyError,
    loadInitial,
    appendMessageDeduped,
  } = useChatHistory(agentId);

  const {
    sending,
    uploading,
    sendMessage,
    error: sendError,
    clearError,
    releaseSendGuards,
  } = useChatSend(agentId, {
    getHistoryMessages: () => messages,
    onUserMessage: appendMessageDeduped,
    onAgentMessage: appendMessageDeduped,
  });

  useChatSocket(agentId, {
    // A reply landing / send completing PROVES the agent is reachable, so
    // clear any stale "Failed to send" banner (core#2736) — mirrors ChatTab.
    onAgentMessage: (m) => {
      appendMessageDeduped(m);
      clearError();
    },
    onSendComplete: () => {
      releaseSendGuards();
      clearError();
    },
  });

  // Restore draft + attachments when a genuine send error surfaces, so the
  // user can retry without retyping (mc#2908 F7). Clear the snapshot after
  // restoring to avoid replaying on unrelated error changes.
  useEffect(() => {
    if (sendError && lastComposerRef.current) {
      setDraft(lastComposerRef.current.text);
      setPendingFiles(lastComposerRef.current.files);
      lastComposerRef.current = null;
    }
  }, [sendError, setDraft, setPendingFiles]);

  // The agent is "thinking" when the user's send is in flight OR the workspace
  // reports an in-flight task — either way it's reachable, so clear any stale
  // "unreachable" banner the moment it's working (core#2745). Mirrors ChatTab.
  const thinking = sending || !!node?.data?.currentTask;
  useEffect(() => {
    if (thinking) clearError();
  }, [thinking, clearError]);

  // Elapsed-seconds counter for the thinking indicator (core#2720/#2745) —
  // ticks while the agent is working, resets when it stops. Gives the same
  // "●●● Ns" feedback the desktop shows so a long turn doesn't look stalled.
  const [thinkingElapsed, setThinkingElapsed] = useState(0);
  useEffect(() => {
    if (!thinking) {
      setThinkingElapsed(0);
      return;
    }
    setThinkingElapsed(0);
    const started = performance.now();
    const id = window.setInterval(() => {
      setThinkingElapsed(Math.floor((performance.now() - started) / 1000));
    }, 1000);
    return () => window.clearInterval(id);
  }, [thinking]);

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

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages]);

  // Consume any agent messages that arrived while history was loading.
  const initialConsumeDoneRef = useRef(false);
  useEffect(() => {
    if (historyLoading || initialConsumeDoneRef.current) return;
    initialConsumeDoneRef.current = true;
    const consume = useCanvasStore.getState().consumeAgentMessages;
    const msgs = consume(agentId);
    for (const m of msgs) {
      appendMessageDeduped(
        createMessage("agent", m.content, m.attachments),
      );
    }
  }, [historyLoading, agentId, appendMessageDeduped]);

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

  const onFilesPicked = (fileList: FileList | null) => {
    if (!fileList) return;
    const picked = Array.from(fileList);
    setPendingFiles((prev) => {
      const keyed = new Set(prev.map((f) => `${f.name}:${f.size}`));
      return [...prev, ...picked.filter((f) => !keyed.has(`${f.name}:${f.size}`))];
    });
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const removePendingFile = (index: number) =>
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));

  // Route attachment downloads through the same authenticated helper
  // the desktop ChatTab uses (downloadChatFile) so platform-scheme
  // URIs get a real Blob with auth headers instead of about:blank.
  const downloadAttachment = (att: ChatAttachment) => {
    downloadChatFile(agentId, att).catch(() => {
      // AttachmentPreview's own error affordance covers the in-bubble
      // failure state; matches ChatTab's behaviour of not double-
      // reporting a download failure.
    });
  };

  const send = async () => {
    const text = draft.trim();
    // Multi-send (core#2726): do NOT gate on `sending`. The A2A proxy holds
    // each turn open, but the user can fire follow-up messages while the agent
    // is still working — useChatSend releases its in-flight guard right after
    // the POST fires, so successive sends queue independently. (Desktop ChatTab
    // shipped this; mobile reused the hook but still blocked here.)
    if ((!text && pendingFiles.length === 0) || !reachable) return;
    clearError();
    // Snapshot the composer so a subsequent genuine send error can restore
    // the draft and attachments instead of leaving the input empty (mc#2908 F7).
    lastComposerRef.current = { text, files: [...pendingFiles] };
    setDraft("");
    const files = pendingFiles;
    setPendingFiles([]);
    await sendMessage(text, files);
  };

  return (
    <div
      data-testid="chat-panel"
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
            className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
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
            className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
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
                className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
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

      {/* Agent Comms — reuse the desktop AgentCommsPanel verbatim so
          mobile renders the identical peer/A2A + delegation feed
          (history GET + live socket events) instead of a placeholder
          (#231). The panel owns its own scroll/load/error/empty
          states, matching ChatTab's agent-comms tabpanel. */}
      {tab === "a2a" && (
        <div style={{ flex: 1, minHeight: 0, overflow: "hidden" }}>
          <AgentCommsPanel workspaceId={agentId} />
        </div>
      )}

      {/* Messages */}
      {tab === "my" && (
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
        {tab === "my" && historyLoading && (
          <div role="status" aria-live="polite" style={{ padding: "20px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
            Loading chat history…
          </div>
        )}
        {tab === "my" && !historyLoading && historyError && (
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
                loadInitial();
              }}
              aria-label="Retry loading chat history"
              className="focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400"
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
        {tab === "my" && !historyLoading && !historyError && messages.length === 0 && (
          <div role="status" aria-live="polite" style={{ padding: "20px 4px", textAlign: "center", color: p.text3, fontSize: 13 }}>
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
                  {m.content && (
                    <MarkdownBubble dark={dark} accent={p.accent}>
                      {m.content}
                    </MarkdownBubble>
                  )}
                  {m.attachments && m.attachments.length > 0 && (
                    <div
                      style={{
                        display: "flex",
                        flexWrap: "wrap",
                        gap: 4,
                        marginTop: m.content ? 6 : 0,
                      }}
                    >
                      {m.attachments.map((att, i) => (
                        <AttachmentPreview
                          key={`${m.id}-${i}`}
                          workspaceId={agentId}
                          attachment={att}
                          onDownload={downloadAttachment}
                          tone={mine ? "user" : "agent"}
                        />
                      ))}
                    </div>
                  )}
                  {/* Tool-call chain — reuse the desktop ChatTab renderer
                      verbatim (#231 parity). Collapsed-by-default chip list
                      under the agent bubble; previously mobile dropped the
                      whole chain so a long tool-using turn rendered as a bare
                      reply with no visible work. */}
                  {!mine && m.toolTrace && m.toolTrace.length > 0 && (
                    <ToolTraceChips trace={m.toolTrace} />
                  )}
                  <div
                    style={{
                      fontSize: 10,
                      marginTop: 4,
                      opacity: mine ? 0.75 : 0.5,
                      fontFamily: MOBILE_FONT_MONO,
                    }}
                  >
                    {formatStoredTimestamp(m.timestamp)}
                  </div>
                </div>
              </div>
            );
          })}
        {thinking && (
          <div
            data-testid="mobile-thinking-indicator"
            aria-label={`Agent working, ${thinkingElapsed} seconds`}
            style={{
              alignSelf: "flex-start",
              display: "flex",
              alignItems: "center",
              gap: 8,
              padding: "8px 12px",
              borderRadius: 14,
              background: p.surface2,
              color: p.text2,
              fontSize: 12,
              fontFamily: MOBILE_FONT_SANS,
            }}
          >
            <span style={{ letterSpacing: 2, color: p.accent }}>●●●</span>
            <span>{thinkingElapsed}s</span>
          </div>
        )}
        {/* Suppress the "Failed to send — agent may be unreachable" banner
            while the agent is demonstrably WORKING (sending in flight OR the
            workspace reports an in-flight task). The effect above only clears
            on the thinking TRANSITION, so a non-524 send error that lands
            mid-turn (e.g. a long poll-mode turn whose POST times out at the CF
            edge while currentTask is still set) would otherwise show the
            banner UNDER the live "●●● 148s" timer — the exact contradiction in
            the report. Gating render on !thinking closes that for good; once
            the turn ends, a still-unresolved error surfaces normally. */}
        {sendError && !thinking && (
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
            {sendError}
          </div>
        )}
      </div>
      )}

      {/* Footer ID + composer belong to My Chat only. The Agent Comms
          tab is a read-only peer/A2A feed (parity with desktop
          ChatTab, where the agent-comms tabpanel has no composer). */}
      {tab === "my" && (
      <>
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
        {pendingFiles.length > 0 && (
          <div
            style={{
              display: "flex",
              flexWrap: "wrap",
              gap: 6,
              marginBottom: 8,
              paddingLeft: 2,
            }}
          >
            {pendingFiles.map((f, i) => (
              <div
                key={`${f.name}:${f.size}`}
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 4,
                  padding: "3px 8px",
                  borderRadius: 10,
                  background: dark ? "#2a2823" : "#ece9e0",
                  fontSize: 12,
                  color: p.text2,
                  maxWidth: "100%",
                }}
              >
                <span
                  style={{
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                    whiteSpace: "nowrap",
                  }}
                >
                  {f.name}
                </span>
                <button
                  type="button"
                  onClick={() => removePendingFile(i)}
                  aria-label={`Remove ${f.name}`}
                  className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
                  style={{
                    border: "none",
                    background: "transparent",
                    color: p.text3,
                    cursor: "pointer",
                    fontSize: 12,
                    padding: 0,
                    lineHeight: 1,
                  }}
                >
                  ✕
                </button>
              </div>
            ))}
          </div>
        )}
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
          <input
            ref={fileInputRef}
            type="file"
            multiple
            style={{ display: "none" }}
            onChange={(e) => onFilesPicked(e.target.files)}
            aria-hidden="true"
          />
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            // Multi-send parity (core#2726 / CR2 #2762): the attach button
            // must NOT disable while a send is in flight, so the user can add
            // attachments to a follow-up message while the agent is still
            // working. Keep the uploading gate (single concurrent upload) and
            // the reachable gate (offline agent).
            disabled={!reachable || uploading}
            aria-label="Attach"
            className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
            style={{
              width: 32,
              height: 32,
              borderRadius: 999,
              border: "none",
              cursor: reachable && !uploading ? "pointer" : "not-allowed",
              background: "transparent",
              color: p.text3,
              flexShrink: 0,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              opacity: !reachable || uploading ? 0.4 : 1,
            }}
          >
            {Icons.attach({ size: 16 })}
          </button>
          <textarea
            ref={composerRef}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            aria-label="Message"
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
              // iOS Safari/PWA zooms the viewport when a focused textarea
              // has a computed font-size below 16px. 14.5 triggers that
              // focus-zoom; the page looks broken until the user pinches
              // back (#224, same class as desktop #1434 / sibling #225).
              // 16px is the minimum that keeps focus from zooming.
              fontSize: 16,
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
            // Multi-send (core#2726): the tap button is mobile's PRIMARY send
            // affordance, so it must NOT disable while `sending` — otherwise a
            // follow-up message can't be tapped during an in-flight turn (CR2
            // #2762). Gate only on empty input / unreachable / uploading; the
            // hook handles concurrent sends.
            disabled={(!draft.trim() && pendingFiles.length === 0) || !reachable || uploading}
            aria-label="Send"
            className="focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-100 dark:focus-visible:ring-offset-zinc-900"
            style={{
              width: 36,
              height: 36,
              borderRadius: 999,
              border: "none",
              cursor:
                (draft.trim() || pendingFiles.length > 0) && reachable && !uploading
                  ? "pointer"
                  : "not-allowed",
              flexShrink: 0,
              background:
                (draft.trim() || pendingFiles.length > 0) && reachable && !uploading
                  ? p.accent
                  : dark
                    ? "#2a2823"
                    : "#ece9e0",
              color: (draft.trim() || pendingFiles.length > 0) && reachable && !uploading ? "#fff" : p.text3,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {uploading ? (
              <span style={{ fontSize: 10, fontWeight: 600 }}>↑</span>
            ) : (
              Icons.send({ size: 16 })
            )}
          </button>
        </div>
      </div>
      </>
      )}
    </div>
  );
}
