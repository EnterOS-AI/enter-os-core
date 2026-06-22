// @vitest-environment jsdom
//
// Task #227 — external/MCP workspace progress UX parity.
//
// ws-server's `proxyA2ARequest` poll-mode short-circuit
// (workspace-server/internal/handlers/a2a_proxy.go:402-432) returns a
// synthetic `{status:"queued", delivery_mode:"poll", method:"message/send"}`
// HTTP 200 within ~50ms when the target workspace is registered with
// `delivery_mode=poll` — i.e. an operator's laptop running
// `molecule-mcp-claude-channel`, a hermes/codex MCP bridge, or a Cursor
// MCP client. The real agent reply arrives separately via the
// AGENT_MESSAGE WebSocket event after the agent's next
// `wait_for_message` poll (could be 1s, could be 60s).
//
// Pre-#227 behaviour: useChatSend treated the queued-200 as a successful
// round-trip — extractReplyText returned "", no agent bubble was
// created, `releaseSendGuards` flipped `sending` off, and the user saw
// dead silence between their user bubble and the eventual reply with
// NO progress indicator. That's the user-reported gap this task fixes.
//
// These tests pin the new behaviour: on a queued-200, the hook MUST NOT
// call onAgentMessage (no empty bubble) AND MUST NOT call
// releaseSendGuards (spinner persists). The eventual AGENT_MESSAGE WS
// event is what clears the spinner — that path is covered by
// useChatSocket.test.tsx already.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

// Capture the api.post invocations + control responses per-test.
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

// uploads — tests don't go through the upload path; stub the helpers
// useChatSend imports so the module loads.
vi.mock("../../uploads", () => ({
  uploadChatFiles: vi.fn(),
  FileTooLargeError: class FileTooLargeError extends Error {},
}));

// types — re-export the createMessage helper unchanged; only the
// uploads stub matters above.
import { useChatSend } from "../useChatSend";

beforeEach(() => {
  apiPostMock.mockReset();
});

describe("useChatSend — poll-mode (external/MCP) queued-200 handling — task #227", () => {
  it("does NOT call onAgentMessage when the synthetic {status:'queued'} response lands (no empty bubble)", async () => {
    // Mock the platform's poll-mode short-circuit response shape exactly
    // as ws-server's `proxyA2ARequest` returns it (a2a_proxy.go:420-431).
    apiPostMock.mockResolvedValueOnce({
      status: "queued",
      delivery_mode: "poll",
      method: "message/send",
    });

    const onUserMessage = vi.fn();
    const onAgentMessage = vi.fn();

    const { result } = renderHook(() =>
      useChatSend("ws-poll-target", {
        getHistoryMessages: () => [],
        onUserMessage,
        onAgentMessage,
      }),
    );

    await act(async () => {
      await result.current.sendMessage("hello external workspace");
      // Yield one microtask so the .then runs.
      await Promise.resolve();
    });

    // User bubble fires — the user typed, that part is unconditional.
    expect(onUserMessage).toHaveBeenCalledTimes(1);
    // CRITICAL: no agent bubble. extractReplyText on a queued envelope
    // returns "" — the pre-#227 code would still have hit the
    // "releaseSendGuards + no bubble" path, BUT it would have ended
    // `sending`. The new code returns early BEFORE that release, so the
    // contract under test is "no synthesised empty bubble".
    expect(onAgentMessage).not.toHaveBeenCalled();
  });

  it("keeps `sending` true after a queued-200 — the spinner must persist until the real AGENT_MESSAGE arrives", async () => {
    apiPostMock.mockResolvedValueOnce({
      status: "queued",
      delivery_mode: "poll",
      method: "message/send",
    });

    const { result } = renderHook(() =>
      useChatSend("ws-poll-target", {
        getHistoryMessages: () => [],
      }),
    );

    await act(async () => {
      await result.current.sendMessage("waiting for the operator laptop");
      await Promise.resolve();
    });

    // The spinner-driving state is `sending`. On a queued-200, it must
    // remain true — clearing it here is the exact bug task #227
    // resurfaces (collapsing the spinner before the agent has even seen
    // the message).
    expect(result.current.sending).toBe(true);
  });

  it("ALSO keeps `sending` true even after a follow-up microtask flush — guards against an accidental late release", async () => {
    // Defense: ensure no chained .then / .finally accidentally calls
    // releaseSendGuards on the queued path. Run several microtask
    // ticks and re-assert.
    apiPostMock.mockResolvedValueOnce({
      status: "queued",
      delivery_mode: "poll",
    });

    const { result } = renderHook(() =>
      useChatSend("ws-poll-target", {
        getHistoryMessages: () => [],
      }),
    );

    await act(async () => {
      await result.current.sendMessage("late-release-guard");
      // Flush multiple microtask ticks.
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(true);
  });

  it("push-mode appends agent bubble even if guards were already released (no early-return drop)", async () => {
    // Regression: an early-return on !sendingFromAPIRef.current dropped the
    // synchronous HTTP reply when the guards had already been released (e.g.
    // by a fast WebSocket event or an earlier timeout classification). The
    // reply must still be parsed and appended; releaseSendGuards is
    // idempotent and appendMessageDeduped collapses duplicates.
    let resolvePost: (value: unknown) => void = () => {};
    apiPostMock.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve;
        }),
    );

    const onAgentMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-push-early-release", {
        getHistoryMessages: () => [],
        onAgentMessage,
      }),
    );

    await act(async () => {
      result.current.sendMessage("release then reply");
      // Yield so the Promise is in flight and sendingFromAPIRef is true.
      await Promise.resolve();
    });

    // Simulate an external release before the HTTP response lands.
    act(() => {
      result.current.releaseSendGuards();
    });

    await act(async () => {
      resolvePost({
        result: {
          parts: [{ kind: "text", text: "reply after early release" }],
        },
      });
      await Promise.resolve();
    });

    expect(onAgentMessage).toHaveBeenCalledTimes(1);
    const msg = onAgentMessage.mock.calls[0][0] as {
      role: string;
      content: string;
    };
    expect(msg.role).toBe("agent");
    expect(msg.content).toBe("reply after early release");
  });
});
