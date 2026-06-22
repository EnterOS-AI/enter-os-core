// @vitest-environment jsdom
//
// core#2697 feature 2 — multi-send. The user MUST be able to fire a
// follow-up message while a prior message's agent reply is still pending
// (the original ship blocked this: sendMessage early-returned on
// `|| sending`, and held sendInFlightRef across the whole reply wait, so
// the 2nd message was silently dropped). The fix removes the `sending`
// gate and releases the re-entrancy guard the moment the POST is FIRED.
//
// These tests pin: (1) a 2nd send while the 1st is in flight goes through
// (user bubble + a 2nd POST); (2) the single-keystroke re-entrancy guard
// still prevents a literal double-fire within the same synchronous tick.

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

describe("useChatSend — multi-send (core#2697 feature 2)", () => {
  it("allows a SECOND send while the FIRST reply is still pending", async () => {
    // Both POSTs hang (agent hasn't replied) — simulates the real
    // "reply pending" window during which the old code dropped send #2.
    apiPostMock.mockImplementation(() => new Promise(() => {}));

    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );

    await act(async () => {
      await result.current.sendMessage("first message");
      await Promise.resolve(); // let the dispatch lock release post-fire
    });
    // After #1 fires, `sending` is true (reply pending) — the old gate.
    expect(result.current.sending).toBe(true);

    await act(async () => {
      await result.current.sendMessage("second message while first pending");
      await Promise.resolve();
    });

    // BOTH user bubbles rendered + BOTH POSTs dispatched — the 2nd was
    // NOT dropped despite `sending` being true.
    expect(onUserMessage).toHaveBeenCalledTimes(2);
    expect(apiPostMock).toHaveBeenCalledTimes(2);
    // Each send carries its own unique messageId.
    const id1 = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;
    const id2 = (apiPostMock.mock.calls[1][1] as any).params.message.messageId;
    expect(id1).not.toBe(id2);
  });

  it("prevents a literal double-fire within the same synchronous tick", async () => {
    // Click-spam / Enter held down should dispatch only ONE POST, even though
    // the sending gate no longer blocks follow-up messages. The brief setup
    // guard releases the moment the POST is fired.
    apiPostMock.mockImplementation(() => new Promise(() => {}));
    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );

    await act(async () => {
      // Fire twice without yielding — same synchronous tick.
      result.current.sendMessage("double-click");
      result.current.sendMessage("double-click");
      await Promise.resolve();
    });

    expect(onUserMessage).toHaveBeenCalledTimes(1);
    expect(apiPostMock).toHaveBeenCalledTimes(1);
  });

  it("does not block on `sending` (an empty/whitespace send is still a no-op)", async () => {
    apiPostMock.mockImplementation(() => new Promise(() => {}));
    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );
    await act(async () => {
      await result.current.sendMessage("   ");
      await Promise.resolve();
    });
    expect(onUserMessage).not.toHaveBeenCalled();
    expect(apiPostMock).not.toHaveBeenCalled();
  });
  it("treats a Cloudflare 524 gateway timeout as 'still processing' (no unreachable banner)", async () => {
    // A long turn outlives CF's ~100s edge limit → api.post throws an Error
    // with .status=524. The agent is still working; reply arrives via WS.
    const err = Object.assign(new Error("API POST /workspaces/ws-1/a2a: 524 "), { status: 524 });
    apiPostMock.mockRejectedValueOnce(err);
    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );
    await act(async () => {
      await result.current.sendMessage("long migrate task");
      await Promise.resolve(); await Promise.resolve();
    });
    // Spinner stays (sending true), NO error banner.
    expect(result.current.sending).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("a Cloudflare 522 (couldn't connect to origin) DOES surface the unreachable banner", async () => {
    // CR2 distinction: 522 = CF couldn't establish a connection to the origin
    // = genuinely unreachable. Unlike 524 (accepted + slow), 522 must NOT be
    // swallowed — show the error so the user knows the message didn't land.
    const err = Object.assign(new Error("API POST /workspaces/ws-1/a2a: 522 "), { status: 522 });
    apiPostMock.mockRejectedValueOnce(err);
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [] }),
    );
    await act(async () => {
      await result.current.sendMessage("hi");
      await Promise.resolve(); await Promise.resolve();
    });
    expect(result.current.error).toMatch(/unreachable/i);
  });

});
