// @vitest-environment jsdom
//
// core#2697 — a stale "Failed to send — agent may be unreachable" banner
// must clear once the agent demonstrably replies. Reported on JRS: a turn
// that hit a context-overflow 400 set the banner, the runtime auto-healed
// (reset session + retried), the retry's reply landed and tools streamed at
// "●●● Ns" — yet the red "unreachable" banner stayed up (contradictory UI).
//
// Fix: ChatTab's onAgentMessage + onSendComplete callbacks clear the
// send-error (the hook's clearError) because a reply landing proves
// reachability. This test mocks useChatSend to expose a clearError spy,
// captures ChatTab's real socket callbacks, invokes them, and asserts the
// reachability callbacks clear the error.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup, act } from "@testing-library/react";

afterEach(cleanup);

vi.mock("@/lib/api", () => ({
  api: { get: () => Promise.resolve([]), post: () => Promise.resolve({}), del: vi.fn(), patch: vi.fn(), put: vi.fn() },
}));
vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((s?: (x: unknown) => unknown) =>
    s ? s({ agentMessages: {}, consumeAgentMessages: () => [] }) : {}),
}));

// Mock useChatSend so we control `error` + spy on clearError.
const clearErrorSpy = vi.fn();
vi.mock("../chat/hooks/useChatSend", () => ({
  useChatSend: () => ({
    sending: false,
    uploading: false,
    sendMessage: vi.fn(),
    error: "Failed to send message — agent may be unreachable",
    clearError: clearErrorSpy,
    releaseSendGuards: vi.fn(),
    sendingFromAPIRef: { current: false },
  }),
  extractReplyText: () => "",
}));

// Capture the callbacks ChatTab passes to useChatSocket.
let captured: Record<string, (arg?: unknown) => void> = {};
vi.mock("../chat/hooks/useChatSocket", () => ({
  useChatSocket: (_ws: string, cbs: Record<string, (arg?: unknown) => void>) => { captured = cbs; },
}));

beforeEach(() => {
  captured = {};
  clearErrorSpy.mockClear();
  Element.prototype.scrollIntoView = vi.fn();
  class FakeIO { observe() {} unobserve() {} disconnect() {} }
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
  (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
});

import { ChatTab } from "../ChatTab";

const data = { status: "online" as const, runtime: "claude-code", currentTask: null } as unknown as Parameters<typeof ChatTab>[0]["data"];

describe("ChatTab — stale error banner clears on a successful reply (core#2697)", () => {
  it("onAgentMessage clears the send-error (agent reply proves reachability)", () => {
    render(<ChatTab workspaceId="ws-clear" data={data} />);
    clearErrorSpy.mockClear(); // ignore any clears during mount
    act(() => {
      captured.onAgentMessage?.({ id: "m1", role: "agent", content: "back on a fresh session", timestamp: new Date().toISOString() });
    });
    expect(clearErrorSpy).toHaveBeenCalled();
  });

  it("onSendComplete (poll-mode reply done) also clears the send-error", () => {
    render(<ChatTab workspaceId="ws-clear2" data={data} />);
    clearErrorSpy.mockClear();
    act(() => { captured.onSendComplete?.(); });
    expect(clearErrorSpy).toHaveBeenCalled();
  });

  it("clears the 'unreachable' banner while the agent is THINKING (currentTask set), before any reply", () => {
    // The reported bug: banner shown beside a live "●●● 102s" timer on a long
    // poll-mode turn that hadn't replied yet. data.currentTask set => thinking
    // => the agent is reachable => the unreachable banner must clear on its own.
    const busy = { status: "online" as const, runtime: "claude-code", currentTask: "downloading assets" } as unknown as Parameters<typeof ChatTab>[0]["data"];
    render(<ChatTab workspaceId="ws-thinking" data={busy} />);
    // Mount with currentTask set => the thinking-clears-error effect fires.
    expect(clearErrorSpy).toHaveBeenCalled();
  });
});
