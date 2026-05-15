"use client";

import { useCallback, useEffect, useRef } from "react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { createMessage, type ChatMessage } from "../types";

export interface UseChatSocketCallbacks {
  onAgentMessage?: (msg: ChatMessage) => void;
  onActivityLog?: (entry: string) => void;
  onSendComplete?: () => void;
  onSendError?: (error: string) => void;
}

export function useChatSocket(
  workspaceId: string,
  callbacks: UseChatSocketCallbacks,
): void {
  const callbacksRef = useRef(callbacks);
  callbacksRef.current = callbacks;

  // Agent push messages from global store
  const pendingAgentMsgs = useCanvasStore((s) => s.agentMessages[workspaceId]);
  useEffect(() => {
    if (!pendingAgentMsgs || pendingAgentMsgs.length === 0) return;
    const consume = useCanvasStore.getState().consumeAgentMessages;
    const msgs = consume(workspaceId);
    for (const m of msgs) {
      callbacksRef.current.onAgentMessage?.(
        createMessage("agent", m.content, m.attachments),
      );
    }
    if (msgs.length > 0) {
      callbacksRef.current.onSendComplete?.();
    }
  }, [pendingAgentMsgs, workspaceId]);

  const resolveWorkspaceName = useCallback((id: string) => {
    const nodes = useCanvasStore.getState().nodes;
    const node = nodes.find((n) => n.id === id);
    return (node?.data as WorkspaceNodeData)?.name || id.slice(0, 8);
  }, []);

  useSocketEvent((msg) => {
    try {
      if (msg.event === "ACTIVITY_LOGGED") {
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
            const own = (targetId || msg.workspace_id) === workspaceId;
            if (own) callbacksRef.current.onSendComplete?.();
          } else if (status === "error") {
            line = `⚠ ${targetName} error`;
            const own = (targetId || msg.workspace_id) === workspaceId;
            if (own) {
              callbacksRef.current.onSendComplete?.();
              callbacksRef.current.onSendError?.(
                "Agent error (Exception) — see workspace logs for details.",
              );
            }
          }
        } else if (type === "a2a_send") {
          const targetName = resolveWorkspaceName(targetId);
          line = `→ Delegating to ${targetName}...`;
        } else if (type === "task_update") {
          if (summary) line = `⟳ ${summary}`;
        } else if (type === "agent_log") {
          if (summary) line = summary;
        }

        if (line) {
          callbacksRef.current.onActivityLog?.(line);
        }
      } else if (
        msg.event === "TASK_UPDATED" &&
        msg.workspace_id === workspaceId
      ) {
        const task = (msg.payload?.current_task as string) || "";
        if (task) {
          callbacksRef.current.onActivityLog?.(`⟳ ${task}`);
        }
      }
    } catch {
      /* ignore */
    }
  });
}
