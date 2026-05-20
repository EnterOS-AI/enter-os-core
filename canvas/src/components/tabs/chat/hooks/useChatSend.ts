"use client";

import { useCallback, useRef, useState } from "react";
import { api } from "@/lib/api";
import { uploadChatFiles, FileTooLargeError } from "../uploads";
import { createMessage, type ChatMessage, type ChatAttachment } from "../types";
import { extractFilesFromTask } from "../message-parser";

interface A2APart {
  kind: string;
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
  };
}

export function extractReplyText(resp: A2AResponse): string {
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
  const sendInFlightRef = useRef(false);
  const sendingFromAPIRef = useRef(false);
  const sendTokenRef = useRef(0);
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const releaseSendGuards = useCallback(() => {
    setSending(false);
    sendingFromAPIRef.current = false;
    sendInFlightRef.current = false;
  }, []);

  const clearError = useCallback(() => setError(null), []);

  const sendMessage = useCallback(
    async (text: string, files: File[] = []) => {
      const trimmed = text.trim();
      if ((!trimmed && files.length === 0) || sending || uploading) return;
      if (sendInFlightRef.current) return;
      sendInFlightRef.current = true;

      let uploaded: ChatAttachment[] = [];
      if (files.length > 0) {
        setUploading(true);
        try {
          uploaded = await uploadChatFiles(workspaceId, files);
        } catch (e) {
          setUploading(false);
          sendInFlightRef.current = false;
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

      const userMsg = createMessage("user", trimmed, uploaded);
      optionsRef.current.onUserMessage?.(userMsg);

      setSending(true);
      sendingFromAPIRef.current = true;
      setError(null);
      const myToken = ++sendTokenRef.current;

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
                messageId: crypto.randomUUID(),
                parts,
              },
              metadata: { history },
            },
          },
          { timeoutMs: 120_000 },
        )
        .then((resp) => {
          if (sendTokenRef.current !== myToken) return;
          if (!sendingFromAPIRef.current) {
            sendInFlightRef.current = false;
            return;
          }
          const replyText = extractReplyText(resp);
          const replyFiles = extractFilesFromTask(
            (resp?.result ?? {}) as Record<string, unknown>,
          );
          if (replyText || replyFiles.length > 0) {
            optionsRef.current.onAgentMessage?.(
              createMessage("agent", replyText, replyFiles),
            );
          }
          releaseSendGuards();
        })
        .catch(() => {
          if (sendTokenRef.current !== myToken) return;
          if (!sendingFromAPIRef.current) {
            sendInFlightRef.current = false;
            return;
          }
          releaseSendGuards();
          setError("Failed to send message — agent may be unreachable");
        });
    },
    [workspaceId, sending, uploading],
  );

  return {
    sending,
    uploading,
    sendMessage,
    error,
    clearError,
    releaseSendGuards,
    sendingFromAPIRef,
  };
}
