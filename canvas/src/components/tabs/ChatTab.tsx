"use client";

import { useState, useRef, useEffect, useCallback, useLayoutEffect } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "@/lib/api";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { type ChatMessage, type ChatAttachment, createMessage, appendMessageDeduped } from "./chat/types";
import { downloadChatFile, isPlatformAttachment } from "./chat/uploads";
import { PendingAttachmentPill } from "./chat/AttachmentViews";
import { AttachmentPreview } from "./chat/AttachmentPreview";
import { AgentCommsPanel } from "./chat/AgentCommsPanel";
import { appendActivityLine } from "./chat/activityLog";
import { runtimeDisplayName } from "@/lib/runtime-names";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { useChatHistory } from "./chat/hooks/useChatHistory";
import { useChatSend } from "./chat/hooks/useChatSend";
import { useChatSocket } from "./chat/hooks/useChatSocket";

export { extractReplyText } from "./chat/hooks/useChatSend";

interface Props {
  workspaceId: string;
  data: WorkspaceNodeData;
}

type ChatSubTab = "my-chat" | "agent-comms";

/**
 * ChatTab container — renders sub-tab bar + My Chat or Agent Comms panel.
 */
export function ChatTab({ workspaceId, data }: Props) {
  const [subTab, setSubTab] = useState<ChatSubTab>("my-chat");

  return (
    <div data-testid="chat-panel" className="flex flex-col h-full">
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
  const [input, setInput] = useState("");
  const [pendingFiles, setPendingFiles] = useState<File[]>([]);
  const [activityLog, setActivityLog] = useState<string[]>([]);
  const [thinkingElapsed, setThinkingElapsed] = useState(0);
  const [agentReachable, setAgentReachable] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmRestart, setConfirmRestart] = useState(false);
  const [dragOver, setDragOver] = useState(false);

  const containerRef = useRef<HTMLDivElement>(null);
  const topRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const hasInitialScrollRef = useRef(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const dragDepthRef = useRef(0);
  const pasteCounterRef = useRef(0);

  const history = useChatHistory(workspaceId, containerRef);
  const chatSend = useChatSend(workspaceId, {
    getHistoryMessages: () => history.messages,
    onUserMessage: (msg) => history.setMessages((prev) => [...prev, msg]),
    onAgentMessage: (msg) => history.setMessages((prev) => appendMessageDeduped(prev, msg)),
  });
  const { sending, uploading, sendMessage, error: sendError, clearError: clearSendError, releaseSendGuards, sendingFromAPIRef } = chatSend;

  const displayError = error || sendError;

  useChatSocket(workspaceId, {
    onAgentMessage: (msg) => {
      history.setMessages((prev) => appendMessageDeduped(prev, msg));
      if (sendingFromAPIRef.current) {
        releaseSendGuards();
      }
    },
    onActivityLog: (entry) => {
      if (!sending) return;
      setActivityLog((prev) => appendActivityLine(prev, entry));
    },
    onSendComplete: () => {
      if (sendingFromAPIRef.current) {
        releaseSendGuards();
      }
    },
    onSendError: (err) => {
      if (sendingFromAPIRef.current) {
        releaseSendGuards();
        setError(err);
      }
    },
  });

  // Agent reachability
  useEffect(() => {
    const reachable = data.status === "online" || data.status === "degraded";
    setAgentReachable(reachable);
    if (reachable) {
      setError(null);
      clearSendError();
    } else {
      setError(`Agent is ${data.status}`);
    }
  }, [data.status, clearSendError]);

  // Scroll behavior across messages updates:
  //  - Prepend (loadOlder landed)  → restore the user's saved
  //    distance-from-bottom so their reading position is unchanged.
  //  - Append / initial            → pin to latest bubble.
  // useLayoutEffect (not useEffect) so scroll restoration runs BEFORE
  // paint — otherwise the user sees the page jump for one frame.
  useLayoutEffect(() => {
    const container = containerRef.current;
    const anchor = history.scrollAnchorRef.current;
    if (
      anchor &&
      container &&
      history.messages.length > 0 &&
      history.messages[0].id !== anchor.expectFirstIdNotEqual
    ) {
      container.scrollTop = container.scrollHeight - anchor.savedDistanceFromBottom;
      history.scrollAnchorRef.current = null;
      return;
    }
    if (!hasInitialScrollRef.current && history.messages.length > 0) {
      hasInitialScrollRef.current = true;
      bottomRef.current?.scrollIntoView({ behavior: "instant" as ScrollBehavior });
      return;
    }
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [history.messages, history.scrollAnchorRef]);

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

  // IntersectionObserver on the top sentinel. Fires loadOlder() the
  // moment the user scrolls within 200px of the top. AbortController
  // unwires cleanly on workspace switch / unmount; root is the
  // scrollable container so we observe only what's visible inside it.
  const hasMessages = history.messages.length > 0;
  useEffect(() => {
    const top = topRef.current;
    const container = containerRef.current;
    if (!top || !container) return;
    if (!history.hasMore) return;
    const ac = new AbortController();
    const io = new IntersectionObserver(
      (entries) => {
        if (ac.signal.aborted) return;
        if (entries[0]?.isIntersecting) history.loadOlder();
      },
      { root: container, rootMargin: "200px 0px 0px 0px", threshold: 0 },
    );
    io.observe(top);
    ac.signal.addEventListener("abort", () => io.disconnect());
    return () => ac.abort();
  }, [history.loadOlder, history.hasMore, hasMessages]);

  const handleSend = async () => {
    const text = input.trim();
    const files = pendingFiles;
    if ((!text && files.length === 0) || !agentReachable || sending || uploading) return;
    setInput("");
    setPendingFiles([]);
    clearSendError();
    setError(null);
    await sendMessage(text, files);
  };

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

  const mimeToExt = (mime: string): string => {
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
      const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
      const seq = pasteCounterRef.current++;
      const fname = `pasted-${stamp}-${seq}-${i}.${ext}`;
      imageFiles.push(new File([file], fname, { type: file.type }));
    }
    if (imageFiles.length === 0) return;
    e.preventDefault();
    addPastedFiles(imageFiles);
  };

  const addPastedFiles = (files: File[]) => {
    setPendingFiles((prev) => {
      const keyed = new Set(prev.map((f) => `${f.name}:${f.size}`));
      return [...prev, ...files.filter((f) => !keyed.has(`${f.name}:${f.size}`))];
    });
  };

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
      {/* talk_to_user disabled banner — shown when the workspace has
           talk_to_user_enabled=false. The agent cannot send canvas messages;
           the user can re-enable the ability from here without opening settings. */}
      {data.talkToUserEnabled === false && (
        <div className="flex items-center gap-2 px-3 py-2 bg-surface-sunken border-b border-line/40 shrink-0">
          <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true" className="shrink-0 text-ink-mid">
            <path d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1Zm0 10.5a.75.75 0 1 1 0-1.5.75.75 0 0 1 0 1.5ZM8 4a.75.75 0 0 1 .75.75v4a.75.75 0 0 1-1.5 0v-4A.75.75 0 0 1 8 4Z" fill="currentColor"/>
          </svg>
          <span className="text-[10px] text-ink-mid flex-1">
            Agent is not enabled to chat with you.
          </span>
          <button
            onClick={async () => {
              try {
                await api.patch(`/workspaces/${workspaceId}/abilities`, { talk_to_user_enabled: true });
                useCanvasStore.getState().updateNodeData(workspaceId, { talkToUserEnabled: true });
              } catch {
                // ignore — user will see no change and can retry
              }
            }}
            className="px-2 py-0.5 text-[10px] font-medium bg-accent/10 hover:bg-accent/20 text-accent rounded border border-accent/30 transition-colors shrink-0"
          >
            Enable
          </button>
        </div>
      )}
      {/* Messages */}
      <div ref={containerRef} className="flex-1 overflow-y-auto p-3 space-y-3">
        {history.loading && (
          <div className="text-xs text-ink-mid text-center py-4">Loading chat history...</div>
        )}
        {!history.loading && history.loadError !== null && history.messages.length === 0 && (
          <div
            role="alert"
            className="mx-2 mt-2 rounded-lg border border-red-800/50 bg-red-950/30 px-3 py-2.5"
          >
            <p className="text-[11px] text-bad mb-1.5">
              Failed to load chat history: {history.loadError}
            </p>
            <button
              onClick={history.loadInitial}
              className="text-[10px] px-2 py-0.5 rounded bg-red-800 text-red-200 hover:bg-red-700 transition-colors"
            >
              Retry
            </button>
          </div>
        )}
        {!history.loading && history.loadError === null && history.messages.length === 0 && (
          <div className="text-xs text-ink-mid text-center py-8">
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
        {history.hasMore && history.messages.length > 0 && (
          <div ref={topRef} className="text-xs text-ink-mid text-center py-1">
            {history.loadingOlder ? "Loading older messages…" : " "}
          </div>
        )}
        {history.messages.map((msg) => (
          <div key={msg.id} className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}>
            <div
              className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
                msg.role === "user"
                  // Blue-600 on white = 3.0:1 (WCAG AA FAIL) in light mode.
                  // Blue-700 on white = 4.5:1 (PASS). In dark mode, blue-600
                  // on zinc-800 = 4.9:1 (PASS). So: blue-700 light, blue-600 dark.
                  ? "bg-blue-700 text-white border border-blue-800 dark:bg-blue-600 dark:border-blue-700 shadow-sm"
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
                  <ReactMarkdown
                    remarkPlugins={[remarkGfm]}
                    components={{
                      // Default ReactMarkdown renders `<a href="...">`
                      // with no target and no scheme handling, so:
                      //
                      //   1. http/https links navigate the canvas tab
                      //      itself away — user loses canvas state.
                      //   2. workspace://, file://, and bare /workspace/
                      //      paths from agent-authored markdown produce
                      //      an unhandled-protocol click → browser ends
                      //      up at about:blank with no download (the
                      //      reported bug from 2026-05-05).
                      //
                      // Override: external URLs open in a new tab with
                      // rel="noopener noreferrer"; in-container paths
                      // route through downloadChatFile so the browser
                      // gets a real Blob with proper auth headers.
                      a: ({ href, children, ...rest }) => {
                        const url = String(href ?? "");
                        // Use the SSOT helper isPlatformAttachment so
                        // the markdown link override and the chip
                        // download path agree on which schemes need
                        // auth-routed download. Pre-fix this list was
                        // duplicated and missed `platform-pending:`,
                        // producing about:blank for poll-mode uploads.
                        if (isPlatformAttachment(url)) {
                          return (
                            <a
                              href={url}
                              {...rest}
                              onClick={(e) => {
                                e.preventDefault();
                                // Construct a synthetic ChatAttachment
                                // and route through the same
                                // authenticated download path the
                                // download chips use. Filename is the
                                // last path segment so Save-As prefills
                                // sensibly.
                                const name = url.split(/[\\/]/).pop() || "download";
                                downloadChatFile(workspaceId, {
                                  uri: url,
                                  name,
                                }).catch((err) => {
                                  setError(
                                    err instanceof Error
                                      ? `Download failed: ${err.message}`
                                      : "Download failed",
                                  );
                                });
                              }}
                            >
                              {children}
                            </a>
                          );
                        }
                        // External (http(s) / mailto / unknown scheme):
                        // open in new tab so canvas state survives.
                        return (
                          <a
                            href={url}
                            target="_blank"
                            rel="noopener noreferrer"
                            {...rest}
                          >
                            {children}
                          </a>
                        );
                      },
                    }}
                  >{msg.content}</ReactMarkdown>
                </div>
              )}
              {msg.attachments && msg.attachments.length > 0 && (
                <div className={`flex flex-wrap gap-1 ${msg.content ? "mt-1.5" : ""}`}>
                  {msg.attachments.map((att, i) => (
                    <AttachmentPreview
                      key={`${msg.id}-${i}`}
                      workspaceId={workspaceId}
                      attachment={att}
                      onDownload={downloadAttachment}
                      tone={msg.role === "user" ? "user" : "agent"}
                    />
                  ))}
                </div>
              )}
              <div className={`text-[9px] mt-1 ${msg.role === "user" ? "text-white/80" : "text-ink-mid"}`}>
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
                <div className="mt-1.5 text-[9px] text-ink-mid space-y-0.5">
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
      {displayError && (
        <div className="px-3 py-2 bg-red-900/20 border-t border-red-800/30">
          <div className="flex items-center justify-between">
            <span className="text-[10px] text-red-300">{displayError}</span>
            {!isOnline && (
              <button
                onClick={() => setConfirmRestart(true)}
                className="text-[11px] px-2 py-0.5 bg-red-800 text-red-200 rounded hover:bg-red-700"
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
              // IME-safe send: while a CJK / Japanese / Korean IME is
              // composing, Enter accepts the candidate selection — not a
              // newline, not a send. `e.nativeEvent.isComposing` is the
              // standard signal (modern WebKit/Blink/Gecko); the keyCode
              // 229 fallback covers older Safari / WebKit-based mobile
              // browsers that delay setting isComposing on the
              // composition-end Enter. Reported 2026-05-05: typing
              // Chinese with the system IME, pressing Enter to commit
              // a candidate would inadvertently send the half-typed
              // message.
              if (
                e.key === "Enter" &&
                !e.shiftKey &&
                !e.nativeEvent.isComposing &&
                e.keyCode !== 229
              ) {
                e.preventDefault();
                handleSend();
              }
            }}
            onPaste={onPasteIntoComposer}
            placeholder={agentReachable ? "Send a message... (Shift+Enter for new line, paste images to attach)" : `Agent is ${data.status}`}
            disabled={!agentReachable || sending}
            rows={1}
            className="flex-1 bg-surface-card border border-line rounded-lg px-3 py-2 text-xs text-ink placeholder-ink-soft dark:bg-zinc-800 dark:border-zinc-600 dark:placeholder-zinc-500 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 resize-none disabled:opacity-50"
          />
          <button
            onClick={handleSend}
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
