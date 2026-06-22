// @vitest-environment jsdom
//
// demo-auto, 2026-06-09 — "Failed to send message — agent may be unreachable"
// after 120s WHILE the agent visibly runs tools in the activity feed.
//
// Mechanism: the A2A proxy holds the POST open for the agent's whole turn;
// a long tool-calling turn outlives the 120s client budget and
// AbortSignal.timeout fires (DOMException name="TimeoutError"). The message
// WAS delivered — the timeout is a client-side stop-waiting, not transport
// failure. Pre-fix the catch-all released the guards and showed the
// unreachable banner (false alarm). Post-fix: a TimeoutError keeps the
// thinking state (reply + guard release arrive via the AGENT_MESSAGE WS
// event, the documented poll-mode contract); real transport errors keep
// the failure banner.

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

// AbortSignal.timeout rejects with a DOMException named "TimeoutError".
const timeoutError = () => {
  try {
    return new DOMException("signal timed out", "TimeoutError");
  } catch {
    // jsdom fallback — only the .name contract matters.
    const e = new Error("signal timed out");
    (e as Error & { name: string }).name = "TimeoutError";
    return e;
  }
};

beforeEach(() => {
  apiPostMock.mockReset();
});

describe("useChatSend — client timeout is NOT 'unreachable'", () => {
  it("keeps sending=true and shows NO error when the 120s client timeout fires (delivered, agent still working)", async () => {
    apiPostMock.mockRejectedValueOnce(timeoutError());

    const { result } = renderHook(() =>
      useChatSend("ws-long-turn", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("do a long multi-tool task");
      await Promise.resolve();
    });

    expect(result.current.error).toBeNull(); // no false "unreachable" banner
    expect(result.current.sending).toBe(true); // thinking persists until the WS reply
  });

  it("still fails loudly on a REAL transport error (non-timeout rejection)", async () => {
    apiPostMock.mockRejectedValueOnce(new Error("connect ECONNREFUSED"));

    const { result } = renderHook(() =>
      useChatSend("ws-dead", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("hello?");
      await Promise.resolve();
    });

    expect(result.current.error).toMatch(/unreachable/);
    expect(result.current.sending).toBe(false); // guards released for retry
  });
});
