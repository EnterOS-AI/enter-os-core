"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { type ChatMessage, appendMessageDeduped as appendMessageDedupedFn } from "../types";

const INITIAL_HISTORY_LIMIT = 10;
const OLDER_HISTORY_BATCH = 20;
// Mobile-chat audit F4: prevent the in-memory message buffer from growing
// without bound on long-lived chats. Server-side pagination still works;
// we just discard the oldest messages client-side to bound re-render cost
// and scroll jank on low-end mobile. A virtualized list is the larger fix.
const MAX_MESSAGES = 500;

async function loadMessagesFromDB(
  workspaceId: string,
  limit: number,
  beforeTs?: string,
): Promise<{ messages: ChatMessage[]; error: string | null; reachedEnd: boolean }> {
  try {
    const params = new URLSearchParams({ limit: String(limit) });
    if (beforeTs) params.set("before_ts", beforeTs);
    // The server emits ChatMessage with a snake_case `tool_trace` field
    // (Go json tag). Map it onto the camelCase `toolTrace` the renderer
    // reads so a rehydrated agent turn shows its tool chain (core#2636).
    const resp = await api.get<{
      messages: (ChatMessage & { tool_trace?: ChatMessage["toolTrace"] })[];
      reached_end: boolean;
    }>(`/workspaces/${workspaceId}/chat-history?${params.toString()}`);
    const messages: ChatMessage[] = (resp.messages ?? []).map((m) =>
      m.tool_trace?.length ? { ...m, toolTrace: m.tool_trace } : m,
    );
    return {
      messages,
      error: null,
      reachedEnd: resp.reached_end,
    };
  } catch (err) {
    return {
      messages: [],
      error: err instanceof Error ? err.message : "Failed to load chat history",
      reachedEnd: true,
    };
  }
}

export interface ScrollAnchor {
  savedDistanceFromBottom: number;
  expectFirstIdNotEqual: string | null;
}

export function useChatHistory(
  workspaceId: string,
  containerRef?: React.RefObject<HTMLDivElement | null>,
) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [hasMore, setHasMore] = useState(true);

  const fetchTokenRef = useRef(0);
  const oldestMessageRef = useRef<ChatMessage | null>(null);
  const hasMoreRef = useRef(true);
  const inflightRef = useRef(false);
  const scrollAnchorRef = useRef<ScrollAnchor | null>(null);

  useEffect(() => {
    oldestMessageRef.current = messages[0] ?? null;
  }, [messages]);

  useEffect(() => {
    hasMoreRef.current = hasMore;
  }, [hasMore]);

  const loadInitial = useCallback(() => {
    setLoading(true);
    setLoadError(null);
    setHasMore(true);
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    return loadMessagesFromDB(workspaceId, INITIAL_HISTORY_LIMIT).then(
      ({ messages: msgs, error: fetchErr, reachedEnd }) => {
        if (fetchTokenRef.current !== myToken) return;
        setMessages(msgs);
        setLoadError(fetchErr);
        setHasMore(!reachedEnd);
        setLoading(false);
      },
    );
  }, [workspaceId]);

  useEffect(() => {
    loadInitial();
  }, [loadInitial]);

  const loadOlder = useCallback(async () => {
    if (inflightRef.current || !hasMoreRef.current) return;
    const oldest = oldestMessageRef.current;
    if (!oldest) return;
    const container = containerRef?.current;
    // Scroll anchoring is only possible when a container ref is wired;
    // otherwise still load older messages instead of silently no-oping
    // (mc#2908 F9).
    if (container) {
      scrollAnchorRef.current = {
        savedDistanceFromBottom: container.scrollHeight - container.scrollTop,
        expectFirstIdNotEqual: oldest.id,
      };
    } else {
      scrollAnchorRef.current = null;
    }
    inflightRef.current = true;
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    setLoadingOlder(true);
    try {
      const { messages: older, reachedEnd } = await loadMessagesFromDB(
        workspaceId,
        OLDER_HISTORY_BATCH,
        oldest.timestamp,
      );
      if (fetchTokenRef.current !== myToken) {
        scrollAnchorRef.current = null;
        return;
      }
      if (older.length > 0) {
        setMessages((prev) => [...older, ...prev].slice(-MAX_MESSAGES));
      } else {
        scrollAnchorRef.current = null;
      }
      setHasMore(!reachedEnd);
    } finally {
      setLoadingOlder(false);
      inflightRef.current = false;
    }
  }, [workspaceId, containerRef]);

  return {
    messages,
    loading,
    loadError,
    loadingOlder,
    hasMore,
    loadInitial,
    loadOlder,
    appendMessageDeduped: (msg: ChatMessage) =>
      setMessages((prev) => appendMessageDedupedFn(prev, msg).slice(-MAX_MESSAGES)),
    setMessages,
    scrollAnchorRef,
  };
}
