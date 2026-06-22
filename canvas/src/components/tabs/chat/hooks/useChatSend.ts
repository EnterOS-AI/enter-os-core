"use client";

import { useCallback, useRef, useState } from "react";
import { api } from "@/lib/api";
import { uploadChatFiles, FileTooLargeError } from "../uploads";
import { createMessage, type ChatMessage, type ChatAttachment } from "../types";
import { extractFilesFromTask } from "../message-parser";

interface A2APart {
  kind?: string;
  /** A2A v0.2 used `type` as the Part discriminator; v0.3 uses `kind`.
   *  Real runtimes and third-party agents still emit both, so accept
   *  either when `kind` is absent. */
  type?: string;
  text?: string;
  file?: {
    name?: string;
    mimeType?: string;
    uri?: string;
    size?: number;
  };
}

interface A2AResponse {
  result?: {
    parts?: A2APart[];
    artifacts?: Array<{ parts: A2APart[] }>;
    /** Standard A2A Task status; real runtimes place the agent's final
     *  reply inside status.message.parts. */
    status?: { message?: { parts?: A2APart[] } };
  };
  /** Set by ws-server's poll-mode short-circuit in `proxyA2ARequest`
   *  (a2a_proxy.go:416-431) when the target workspace is registered as
   *  `delivery_mode=poll` — e.g. an operator's laptop running
   *  `molecule-mcp-claude-channel`, a hermes/codex MCP bridge, or a
   *  Cursor MCP client. The HTTP 200 carries the synthetic envelope
   *  `{status:"queued", delivery_mode:"poll", method:"message/send"}`
   *  immediately (~50ms), BEFORE the agent has produced a reply.
   *
   *  Task #227 routing: when this field is "queued" the caller must NOT
   *  treat the 200 as "agent done" — there are no `result.parts` yet
   *  (the reply will arrive separately via the AGENT_MESSAGE WS event
   *  after the agent's next poll). Keep the spinner up; the eventual
   *  AGENT_MESSAGE flips `sending` off via the existing useChatSocket
   *  `onSendComplete` path. Without this distinction the spinner
   *  disappeared immediately and external/MCP workspaces had no progress
   *  UX between send and reply. */
  status?: string;
  /** Companion to `status` — "poll" when the queued short-circuit fired.
   *  Defensive: we key the poll-mode-skip decision on status==="queued"
   *  (the canonical signal) rather than on this field, but it's surfaced
   *  here so future debugging / tests can assert on the full envelope. */
  delivery_mode?: string;
}

export function extractReplyText(resp: A2AResponse): string {
  const collect = (parts: A2APart[] | undefined): string => {
    if (!parts) return "";
    return parts
      .filter((p) => p.kind === "text" || p.type === "text")
      .map((p) => p.text ?? "")
      .filter(Boolean)
      .join("\n");
  };
  const result = resp?.result;
  const collected: string[] = [];

  // Standard A2A JSON-RPC: {result: {parts: [{kind: "text", text: "..."}]}}
  const fromParts = collect(result?.parts);
  if (fromParts) collected.push(fromParts);

  // Standard A2A Task shape: the agent's reply can live in
  // result.status.message.parts rather than directly on result.parts.
  // Real runtimes (Claude Code, Hermes, etc.) commonly return this shape.
  const fromStatusMessage = collect(result?.status?.message?.parts);
  if (fromStatusMessage) collected.push(fromStatusMessage);

  if (result?.artifacts) {
    for (const a of result.artifacts) {
      const t = collect(a.parts);
      if (t) collected.push(t);
    }
  }
  return collected.join("\n");
}

/** Map a thrown error from `uploadChatFiles` to the user-facing reason
 *  shown in the chat error banner.
 *
 *  Cases (per `feedback_surface_actionable_failure_reason_to_user` —
 *  user-facing failures MUST tell the user WHY):
 *
 *    1. FileTooLargeError → use the error's message verbatim. The
 *       pre-flight already built the actionable string with the actual
 *       size + the cap; don't re-wrap it (which would prepend a
 *       redundant "Upload failed:" prefix).
 *
 *    2. DOMException name="TimeoutError" → AbortSignal.timeout fired
 *       during the fetch. Pre-flight already excluded file-size, so
 *       this CANNOT mean "file too large". Surface a connection-speed
 *       message — the user's actionable next step is retry or check
 *       network, NOT shrink the file.
 *
 *    3. Other Error → use the wrapped form so the server's reason
 *       (e.g. "upload failed: 413 ...") reaches the user instead of
 *       being swallowed.
 *
 *    4. Non-Error throw → generic fallback.
 *
 *  Exported for unit testing — the case-by-case mapping is the
 *  load-bearing contract this PR ships. */
export function mapUploadErrorToReason(e: unknown): string {
  if (e instanceof FileTooLargeError) {
    // Already a complete, user-facing sentence — surface verbatim.
    return e.message;
  }
  // DOMException with name="TimeoutError" is what AbortSignal.timeout
  // produces on abort. Browsers represent it as a DOMException, not a
  // regular Error subclass — feature-detect via .name to avoid coupling
  // to a global that's missing in test envs.
  if (
    e !== null && typeof e === "object" &&
    "name" in e && (e as { name: unknown }).name === "TimeoutError"
  ) {
    return "Upload timed out — your connection is too slow for this file. Try again, or reduce file size.";
  }
  if (e instanceof Error) {
    return `Upload failed: ${e.message}`;
  }
  return "Upload failed";
}

export interface UseChatSendOptions {
  getHistoryMessages: () => ChatMessage[];
  onUserMessage?: (msg: ChatMessage) => void;
  onAgentMessage?: (msg: ChatMessage) => void;
}

export function useChatSend(workspaceId: string, options: UseChatSendOptions) {
  const [sending, setSending] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // core#2725: concurrent sends. Track every dispatched request by a unique
  // token so replies are routed to the correct send and the spinner stays up
  // until the LAST pending send completes.
  //
  // Lifecycle:
  //   - inFlightTokensRef:    POST dispatched, no terminal HTTP reply yet.
  //   - pendingWSTokensRef:   HTTP replied in a "delivered, await WS" state
  //                           (queued poll-mode, client timeout, CF 524). The
  //                           spinner stays up; finishSendByMessageId or
  //                           releaseSendGuards prunes these.
  //   - messageIdToTokenRef:  Map from client-generated messageId to token.
  //                           Lets WebSocket completion events finish the
  //                           SPECIFIC send they belong to, avoiding cross-send
  //                           contamination (CR2 #11466).
  //   - wsCompletedTokensRef: Legacy fallback set for older ws-server builds
  //                           that do not broadcast a messageId. Without an id
  //                           we cannot safely correlate a completion to a
  //                           specific send when multiple sends are concurrent,
  //                           so the fallback is intentionally conservative:
  //                           it only acts when exactly one token is tracked.
  //                           With one tracked token it finishes a pending-WS
  //                           token or marks an in-flight token as
  //                           WS-completed so its own late timeout/524/queued
  //                           response can finish itself. With multiple tracked
  //                           tokens the no-id completion is ignored; the
  //                           modern messageId-aware path above is required for
  //                           concurrent sends and remains preferred.
  //   - setupGuardRef:        brief synchronous guard so a double-click in the
  //                           same tick dispatches only once. Released on the
  //                           next microtask after the POST is fired, so
  //                           distinct follow-up sends are never blocked.
  const inFlightTokensRef = useRef<Set<number>>(new Set());
  const pendingWSTokensRef = useRef<Set<number>>(new Set());
  const wsCompletedTokensRef = useRef<Set<number>>(new Set());
  const messageIdToTokenRef = useRef<Map<string, number>>(new Map());
  // Reverse index for O(1) cleanup when a token finishes (mc#2908 F8).
  const tokenToMessageIdRef = useRef<Map<number, string>>(new Map());
  const sendingFromAPIRef = useRef(false);
  const nextTokenRef = useRef(1);
  const setupGuardRef = useRef(false);
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const syncSendingState = useCallback(() => {
    const pending =
      inFlightTokensRef.current.size > 0 || pendingWSTokensRef.current.size > 0;
    setSending(pending);
    sendingFromAPIRef.current = pending;
  }, []);

  const finishSendToken = useCallback((token: number) => {
    inFlightTokensRef.current.delete(token);
    pendingWSTokensRef.current.delete(token);
    wsCompletedTokensRef.current.delete(token);
    // O(1) cleanup of the bidirectional messageId mapping (mc#2908 F8).
    const mid = tokenToMessageIdRef.current.get(token);
    if (mid !== undefined) {
      tokenToMessageIdRef.current.delete(token);
      messageIdToTokenRef.current.delete(mid);
    }
    syncSendingState();
  }, [syncSendingState]);

  const finishSendByMessageId = useCallback((messageId: string) => {
    const token = messageIdToTokenRef.current.get(messageId);
    if (token !== undefined) {
      finishSendToken(token);
    }
  }, [finishSendToken]);

  const releaseSendGuards = useCallback((messageId?: string) => {
    // Token-aware completion: modern ws-server builds echo the client-generated
    // messageId, so we can finish EXACTLY the send that completed (CR2 #11466,
    // #11454). This never touches unrelated concurrent sends.
    if (messageId) {
      finishSendByMessageId(messageId);
      return;
    }

    // Legacy fallback for older ws-server builds that do not broadcast a
    // messageId. Without an id we cannot safely correlate a completion to a
    // specific send when multiple sends are concurrent, so we only act when
    // exactly one token is tracked. In that single-token case we finish a
    // pending-WS token or mark the in-flight token as WS-completed so its own
    // late timeout/524/queued response can finish itself. If more than one
    // token is tracked we do nothing and rely on the modern messageId-aware
    // path above, which is the only safe way to route concurrent completions.
    const totalTracked =
      inFlightTokensRef.current.size + pendingWSTokensRef.current.size;
    if (totalTracked !== 1) {
      return;
    }

    const onlyPending = pendingWSTokensRef.current.values().next().value as
      | number
      | undefined;
    if (onlyPending !== undefined) {
      finishSendToken(onlyPending);
      return;
    }

    const onlyInFlight = inFlightTokensRef.current.values().next().value as
      | number
      | undefined;
    if (onlyInFlight !== undefined) {
      wsCompletedTokensRef.current.add(onlyInFlight);
      syncSendingState();
    }
  }, [finishSendByMessageId, finishSendToken, syncSendingState]);

  const pendSendTokenForWS = useCallback((token: number) => {
    // HTTP replied "queued / still alive via WS". Move from in-flight to the
    // WS-pending set so the spinner persists until finishSendByMessageId or
    // releaseSendGuards cleans it up. If the WS completion already fired for
    // this token (legacy fallback path), finish immediately instead of
    // re-pending forever (CR2 #11470).
    if (wsCompletedTokensRef.current.has(token)) {
      finishSendToken(token);
      return;
    }
    inFlightTokensRef.current.delete(token);
    pendingWSTokensRef.current.add(token);
    syncSendingState();
  }, [syncSendingState, finishSendToken]);

  const clearError = useCallback(() => setError(null), []);

  const sendMessage = useCallback(
    async (text: string, files: File[] = []) => {
      const trimmed = text.trim();
      // core#2725: do NOT block on an existing in-flight send. The server-side
      // A2A queue is durable and orders multiple user messages. We only skip
      // truly empty sends, concurrent uploads (the uploading flag is
      // intentionally global: concurrent file uploads would race the single
      // uploading UI state), and same-tick duplicate dispatch (brief guard).
      if ((!trimmed && files.length === 0) || uploading) return;
      if (setupGuardRef.current) return;
      setupGuardRef.current = true;

      let uploaded: ChatAttachment[] = [];
      if (files.length > 0) {
        setUploading(true);
        try {
          uploaded = await uploadChatFiles(workspaceId, files);
        } catch (e) {
          setUploading(false);
          setupGuardRef.current = false;
          // Error-reason routing (CTO 2026-05-19 on forensic a99ab0a1:
          // "if its file size issue, should have error that instead
          // saying timeout which is wrong"). Each cause maps to ITS
          // OWN message — NO conflation between file-size and
          // connection-too-slow.
          setError(mapUploadErrorToReason(e));
          return;
        }
        setUploading(false);
      }

      // One id, threaded through both the optimistic bubble and the A2A
      // payload's messageId, so the server's USER_MESSAGE broadcast echo
      // dedups against this bubble on the origin device (core#2697 —
      // otherwise the sender saw its own message twice).
      const messageId = crypto.randomUUID();
      const userMsg = createMessage("user", trimmed, uploaded, undefined, messageId);
      optionsRef.current.onUserMessage?.(userMsg);

      setError(null);
      const myToken = nextTokenRef.current++;
      inFlightTokensRef.current.add(myToken);
      messageIdToTokenRef.current.set(messageId, myToken);
      tokenToMessageIdRef.current.set(myToken, messageId);
      syncSendingState();

      const history = optionsRef.current
        .getHistoryMessages()
        .filter((m) => m.role === "user" || m.role === "agent")
        .slice(-20)
        .map((m) => ({
          role: m.role === "user" ? "user" : "agent",
          parts: [{ kind: "text", text: m.content }],
        }));

      const parts: A2APart[] = [];
      if (trimmed) parts.push({ kind: "text", text: trimmed });
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

      api
        .post<A2AResponse>(
          `/workspaces/${workspaceId}/a2a`,
          {
            method: "message/send",
            params: {
              message: {
                role: "user",
                messageId,
                parts,
              },
              metadata: { history },
            },
          },
          // 30min — must exceed the server-side canvas idle watchdog
          // (workspace-server/internal/handlers/a2a_proxy.go:1002 const
          // defaultIdleTimeoutDuration = 30 * time.Minute, core#2723 / #2727
          // raise). Pre-#2727 the server-side default was 5min; the 120s
          // client-side timeout was BELOW the server idle and the
          // isClientTimeout handler at useChatSend.ts:261-280 silently
          // swallowed it (delivered; reply (and guard release) arrives via
          // the AGENT_MESSAGE WS event). The 5min server raise moved the
          // window to 5min, but a 300s+ chain still died client-side at
          // 120s — half-fixed (#2723 ship didn't include the client
          // alignment). The 30min server raise in #2727 makes a 30min
          // client timeout safe, and the same isClientTimeout swallow
          // path means a client timeout still doesn't surface "agent may
          // be unreachable" — it just falls through to the WS-event
          // delivery.
          { timeoutMs: 30 * 60 * 1000 },
        )
        .then((resp) => {
          // Poll-mode queued short-circuit (external/MCP workspace): the
          // server returns `{status:"queued"}` immediately and the real reply
          // arrives later via the AGENT_MESSAGE WebSocket event.
          if (resp?.status === "queued") {
            if (
              !inFlightTokensRef.current.has(myToken) &&
              !pendingWSTokensRef.current.has(myToken)
            ) {
              // Already finished by a WS event (legacy fallback raced ahead).
              return;
            }
            if (wsCompletedTokensRef.current.has(myToken)) {
              finishSendToken(myToken);
            } else {
              pendSendTokenForWS(myToken);
            }
            return;
          }

          // Push-mode synchronous reply: process the agent message even if a
          // WebSocket completion event (ACTIVITY_LOGGED or AGENT_MESSAGE)
          // already finished this token. Without this, a fast echo/reply that
          // triggers a WS completion before the HTTP 200 lands would have its
          // token removed here and the reply bubble would never render
          // (core#2786 / #2759). Token cleanup is idempotent; ChatTab's
          // message dedup handles the rare case where both paths carry the
          // same content.
          const replyText = extractReplyText(resp);
          const replyFiles = extractFilesFromTask(
            (resp?.result ?? {}) as Record<string, unknown>,
          );
          if (replyText || replyFiles.length > 0) {
            optionsRef.current.onAgentMessage?.(
              createMessage("agent", replyText, replyFiles),
            );
          }
          finishSendToken(myToken);
        })
        .catch((e: unknown) => {
          if (
            !inFlightTokensRef.current.has(myToken) &&
            !pendingWSTokensRef.current.has(myToken)
          ) return;

          // CLIENT TIMEOUT ≠ UNREACHABLE (demo-auto, 2026-06-09). The A2A
          // proxy holds this POST open for the agent's WHOLE turn; a long
          // tool-calling turn routinely outlives the 120s client budget.
          // AbortSignal.timeout firing after the server ACCEPTED and held
          // the connection means the message was DELIVERED and the agent is
          // still working — showing "agent may be unreachable" here is a
          // false alarm (the user watches the agent run tools in the
          // activity feed while the chat claims failure). Keep the thinking
          // state up; the reply lands via the AGENT_MESSAGE WebSocket event,
          // which releases the guards — exactly the documented poll-mode
          // contract above. Genuine unreachability fails FAST (connection
          // refused / 4xx / 5xx) and still takes the error branch; a truly
          // dead agent is surfaced by the reactive-health path
          // (maybeMarkContainerDead), not by this client timeout.
          const isClientTimeout =
            e !== null && typeof e === "object" &&
            "name" in e && (e as { name: unknown }).name === "TimeoutError";
          // CLOUDFLARE 524 ≠ UNREACHABLE (demo-auto, 2026-06-13). The canvas→agent
          // A2A POST is held open for the whole turn; a turn that runs longer
          // than Cloudflare's ~100s edge limit gets a 524 ("A Timeout Occurred")
          // — the origin ACCEPTED the request and is still processing it (the
          // agent is visibly running tools), and its reply arrives via the
          // AGENT_MESSAGE WebSocket event, exactly like the client-timeout case.
          // ONLY 524: per CR2, 522 ("Connection Timed Out" — CF couldn't even
          // connect to the origin) and 504 mean the request was NOT accepted /
          // the origin is genuinely unreachable, so those MUST still surface the
          // error banner. Don't conflate "accepted + slow" (524) with "couldn't
          // connect" (522).
          const status = (e as { status?: number } | null)?.status;
          const isCloudflareHeldRequest = status === 524;
          if (isClientTimeout || isCloudflareHeldRequest) {
            pendSendTokenForWS(myToken);
            return; // delivered; reply (and guard release) arrives via WS
          }

          finishSendToken(myToken);
          setError("Failed to send message — agent may be unreachable");
        });

      // The POST is now in flight (the .then/.catch above run later, off the
      // microtask queue). Release the setup guard on the next microtask so
      // a same-tick double-click is still deduped, while distinct follow-up
      // sends in subsequent ticks proceed normally.
      Promise.resolve().then(() => {
        setupGuardRef.current = false;
      });
    },
    [workspaceId, uploading, syncSendingState, finishSendToken, pendSendTokenForWS],
  );

  return {
    sending,
    uploading,
    sendMessage,
    error,
    clearError,
    releaseSendGuards,
    finishSendByMessageId,
    sendingFromAPIRef,
  };
}
