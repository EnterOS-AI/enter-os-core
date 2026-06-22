// @vitest-environment jsdom
//
// core#2725 — concurrent sends must not drop, misroute, or starve the
// thinking indicator. The hook now tracks every in-flight request by a
// unique token (Set). Replies are processed only when their token is still
// in the set, and `sending` stays true while ANY token is pending.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

const apiPostMock = vi.fn<
  (url: string, body?: unknown, opts?: unknown) => Promise<unknown>
>();
vi.mock("@/lib/api", () => ({
  api: {
    post: (url: string, body?: unknown, opts?: unknown) =>
      apiPostMock(url, body, opts),
    get: vi.fn(),
  },
}));

vi.mock("../../uploads", () => ({
  uploadChatFiles: vi.fn(),
  FileTooLargeError: class FileTooLargeError extends Error {},
}));

import { useChatSend } from "../useChatSend";

beforeEach(() => {
  apiPostMock.mockReset();
});

function deferred<T = unknown>() {
  let resolve: (value: T) => void = () => {};
  let reject: (reason?: unknown) => void = () => {};
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("useChatSend — concurrent replies (core#2725)", () => {
  it("routes each HTTP reply to the correct send when replies resolve out of order", async () => {
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const onAgentMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", {
        getHistoryMessages: () => [],
        onAgentMessage,
      }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve(); // let the setup guard release
      result.current.sendMessage("second");
      await Promise.resolve();
    });

    // Resolve the SECOND send before the first.
    await act(async () => {
      second.resolve({ result: { parts: [{ kind: "text", text: "reply-two" }] } });
      await Promise.resolve();
    });

    // Resolve the FIRST send afterwards.
    await act(async () => {
      first.resolve({ result: { parts: [{ kind: "text", text: "reply-one" }] } });
      await Promise.resolve();
    });

    expect(onAgentMessage).toHaveBeenCalledTimes(2);
    const contents = onAgentMessage.mock.calls.map(
      (c) => (c[0] as { content: string }).content,
    );
    expect(contents).toContain("reply-one");
    expect(contents).toContain("reply-two");
  });

  it("keeps the thinking indicator active until the LAST pending send completes", async () => {
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve(); // let the setup guard release
      result.current.sendMessage("second");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    await act(async () => {
      first.resolve({ result: { parts: [{ kind: "text", text: "reply-one" }] } });
      await Promise.resolve();
    });
    // Send #2 is still pending → spinner stays up.
    expect(result.current.sending).toBe(true);

    await act(async () => {
      second.resolve({ result: { parts: [{ kind: "text", text: "reply-two" }] } });
      await Promise.resolve();
    });
    // Last send done → spinner off.
    expect(result.current.sending).toBe(false);
  });

  it("cleans up the poll-mode token when releaseSendGuards is called", async () => {
    apiPostMock.mockResolvedValue({
      status: "queued",
      delivery_mode: "poll",
      method: "message/send",
    });

    const { result } = renderHook(() =>
      useChatSend("ws-poll", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("poll me");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // The AGENT_MESSAGE / onSendComplete WS event arrives.
    act(() => {
      result.current.releaseSendGuards();
    });

    expect(result.current.sending).toBe(false);
  });

  it("keeps the spinner up when releaseSendGuards prunes one poll-mode send but another is still in flight", async () => {
    const pushSend = deferred();
    apiPostMock
      .mockImplementationOnce(() => pushSend.promise)
      .mockResolvedValueOnce({
        status: "queued",
        delivery_mode: "poll",
        method: "message/send",
      });

    const { result } = renderHook(() =>
      useChatSend("ws-mixed", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("push-mode send");
      result.current.sendMessage("poll-mode send");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Poll-mode send completes via WS; push-mode send is still HTTP pending.
    act(() => {
      result.current.releaseSendGuards();
    });

    expect(result.current.sending).toBe(true);

    await act(async () => {
      pushSend.resolve({ result: { parts: [{ kind: "text", text: "push reply" }] } });
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
  });

  it("finishes a late timeout/524 for a token whose WS completion already fired (CR2 #11463)", async () => {
    // The WS onAgentMessage/onSendComplete can arrive BEFORE the HTTP request
    // finally terminates with a timeout/524. With token-specific completion,
    // finishSendByMessageId(messageId) removes the exact token; the late
    // timeout/524 then sees the token is gone and drops instead of re-pending.
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-race", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("long turn");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    const messageId = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;

    // WS completion arrives first — finish the SPECIFIC token.
    act(() => {
      result.current.finishSendByMessageId?.(messageId);
    });
    expect(result.current.sending).toBe(false);

    // Late client timeout lands.
    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      send.reject(timeoutErr);
      await Promise.resolve();
    });

    // Spinner stays off; no error banner.
    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("does not contaminate an unrelated concurrent send on token-specific WS completion (CR2 #11466)", async () => {
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-mixed", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve();
      result.current.sendMessage("second");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    const firstMessageId = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;

    // WS completion arrives ONLY for the first send.
    act(() => {
      result.current.finishSendByMessageId?.(firstMessageId);
    });

    // Second send is still in-flight → spinner stays up.
    expect(result.current.sending).toBe(true);

    // Second send later times out (no WS reply for it).
    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      second.reject(timeoutErr);
      await Promise.resolve();
    });

    // It moves to pending-WS (not finished), so spinner stays up until its
    // own WS completion or releaseSendGuards.
    expect(result.current.sending).toBe(true);

    // Its own WS completion finally arrives.
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(false);
  });

  it("legacy fallback: releaseSendGuards() with no messageId still handles late timeout/524 (CR2 #11470)", async () => {
    // Older ws-server builds do not broadcast messageId. releaseSendGuards()
    // marks all tracked tokens as WS-completed so a subsequent late
    // timeout/524 finishes itself rather than re-pending forever.
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-legacy", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("long turn");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // WS completion arrives without a messageId (legacy path).
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    // Late client timeout lands.
    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      send.reject(timeoutErr);
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("does not finish an unrelated concurrent send on token-specific error completion (Researcher #11471)", async () => {
    // Simulates the ACTIVITY_LOGGED status=error path with message_id: an
    // error completion for send A must finish only A's token, leaving send B
    // pending until its own completion arrives.
    apiPostMock
      .mockResolvedValueOnce({
        status: "queued",
        delivery_mode: "poll",
        method: "message/send",
      })
      .mockResolvedValueOnce({
        status: "queued",
        delivery_mode: "poll",
        method: "message/send",
      });

    const { result } = renderHook(() =>
      useChatSend("ws-poll-two", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("poll one");
      await Promise.resolve();
      result.current.sendMessage("poll two");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    const firstMessageId = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;

    // Error completion arrives ONLY for the first send.
    act(() => {
      result.current.finishSendByMessageId?.(firstMessageId);
    });

    // Second send is still pending-WS → spinner stays up.
    expect(result.current.sending).toBe(true);

    // Second send completes via legacy fallback.
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(false);
  });
});

describe("useChatSend — legacy no-messageId fallback is exact-one-token conservative (core#2775)", () => {
  it("legacy releaseSendGuards() with no messageId still handles late timeout/524 for a single tracked token (CR2 #11470)", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-legacy", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("long turn");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // WS completion arrives without a messageId (legacy path). Exactly one
    // token is tracked, so the fallback marks it as WS-completed.
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    // Late client timeout lands.
    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      send.reject(timeoutErr);
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("does not release any token when a no-messageId completion arrives while multiple tokens are tracked (CR2 #11466 regression)", async () => {
    // This is the key safety property: without a messageId we cannot know
    // which send completed, so we must not guess. Two concurrent sends are
    // in-flight; a legacy no-id completion arrives out of order. Neither
    // token should be finished or marked WS-completed.
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-legacy-two-inflight", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve();
      result.current.sendMessage("second");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Out-of-order legacy completion arrives while both sends are tracked.
    act(() => {
      result.current.releaseSendGuards();
    });

    // Neither token should be touched: spinner still up and both HTTP requests
    // are still pending.
    expect(result.current.sending).toBe(true);

    // First HTTP reply lands; it should still resolve normally via the
    // messageId-aware path if one is provided, or move to pending-WS.
    await act(async () => {
      first.resolve({ result: { parts: [{ kind: "text", text: "reply-one" }] } });
      await Promise.resolve();
    });

    // First send done, second still in-flight.
    expect(result.current.sending).toBe(true);

    await act(async () => {
      second.resolve({ result: { parts: [{ kind: "text", text: "reply-two" }] } });
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
  });

  it("does not release any token when two sends are pending-WS and a no-messageId completion arrives", async () => {
    // Both sends entered the WS-pending state via timeout/524/queued. A legacy
    // no-id completion must not drain one of them, because it could be the
    // wrong one.
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-legacy-two-pending", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve();
      result.current.sendMessage("second");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Both HTTP requests fail into pending-WS.
    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      first.reject(timeoutErr);
      second.reject(Object.assign(new Error("cf 524"), { status: 524 }));
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Legacy no-id completion is ignored because two tokens are pending.
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    // The only safe resolution is a messageId-aware completion for each send.
    const firstMessageId = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;
    const secondMessageId = (apiPostMock.mock.calls[1][1] as any).params.message.messageId;

    act(() => {
      result.current.finishSendByMessageId?.(firstMessageId);
    });
    expect(result.current.sending).toBe(true);

    act(() => {
      result.current.finishSendByMessageId?.(secondMessageId);
    });
    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("late queued response still finishes its own token after a legacy early release (no re-pend)", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-queued-late", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("queued late");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Legacy early release (no messageId) marks the only in-flight token.
    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    // HTTP eventually returns the queued envelope.
    await act(async () => {
      send.resolve({
        status: "queued",
        delivery_mode: "poll",
        method: "message/send",
      });
      await Promise.resolve();
    });

    // The queued path must finish the token instead of moving it back to
    // pending-WS; otherwise the token leaks and the spinner never drops.
    expect(result.current.sending).toBe(false);
  });

  it("late 524 response still finishes its own token after a legacy early release", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-524-late", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("524 late");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    const err = Object.assign(new Error("cf 524"), { status: 524 });
    await act(async () => {
      send.reject(err);
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("late timeout response still finishes its own token after a legacy early release", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-timeout-late", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("timeout late");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    act(() => {
      result.current.releaseSendGuards();
    });
    expect(result.current.sending).toBe(true);

    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      send.reject(timeoutErr);
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("a send whose token was already finished by messageId drops a late timeout without re-pending (no leak)", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-messageid-late", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("messageId late");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    const messageId = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;
    act(() => {
      result.current.releaseSendGuards(messageId);
    });
    expect(result.current.sending).toBe(false);

    const timeoutErr = new Error("signal timed out") as Error & { name: string };
    timeoutErr.name = "TimeoutError";
    await act(async () => {
      send.reject(timeoutErr);
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });
});

describe("useChatSend — push-mode reply is not dropped by a racing WS completion (core#2786)", () => {
  it("renders the push-mode echo reply even when onSendComplete finishes the token first", async () => {
    const send = deferred();
    apiPostMock.mockImplementationOnce(() => send.promise);

    const onAgentMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("push-echo-ws-race", {
        getHistoryMessages: () => [],
        onAgentMessage,
      }),
    );

    await act(async () => {
      await result.current.sendMessage("echo me");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    const messageId = (apiPostMock.mock.calls[0][1] as { params: { message: { messageId: string } } }).params.message.messageId;

    // A WebSocket completion event (ACTIVITY_LOGGED status=ok with messageId,
    // or AGENT_MESSAGE) can arrive before the HTTP 200 on a fast echo/reply.
    // It finishes the token — the spinner must drop.
    act(() => {
      result.current.finishSendByMessageId?.(messageId);
    });
    expect(result.current.sending).toBe(false);

    // The late HTTP push-mode reply still carries the actual content and MUST
    // be rendered; otherwise the echo bubble never appears (core#2786).
    await act(async () => {
      send.resolve({ result: { parts: [{ kind: "text", text: "Echo: echo me" }] } });
      await Promise.resolve();
    });

    expect(onAgentMessage).toHaveBeenCalledTimes(1);
    expect((onAgentMessage.mock.calls[0][0] as { content: string }).content).toBe(
      "Echo: echo me",
    );
  });
});
