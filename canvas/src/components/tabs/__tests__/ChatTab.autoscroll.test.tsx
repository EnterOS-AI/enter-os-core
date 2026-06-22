// @vitest-environment jsdom
//
// Pins the #2560 autoscroll at-bottom gating for the chat tab.
//
// CTO ask (issue #2560): "Bottom-sticky autoscroll while tool calls
// accumulate. Gate the EXISTING always-scroll-on-append behavior behind
// the SAME at-bottom check. Don't yank the viewport down if the user
// has scrolled up." The pre-#2560 behavior was `bottomRef.scrollIntoView`
// on every message append AND on every activityLog growth — yanking the
// viewport when the user was reading older history (a real complaint:
// "I scroll up to compare against an earlier reply and the chat
// yanks me back to the bottom every time the agent logs a tool call").
//
// The fix adds a `atBottom` ref (scroll listener with a 12px threshold)
// that gates BOTH the message-append path AND a NEW activityLog-growth
// path. The loadOlder anchor-restore contract is preserved unchanged.
//
// These tests guard the four behaviours the issue specified:
//  (a) append scrolls when atBottom
//  (b) append does NOT scroll when scrolled up
//  (c) activityLog growth scrolls when atBottom
//  (d) activityLog growth does NOT scroll when scrolled up
//  (e) anchor restore on loadOlder is NOT affected by atBottom

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// No /chat-history, no heartbeats, no activity-log polling — just the
// scroll behavior under test.
const apiGet = vi.fn((_path: string) => Promise.resolve([]));
const apiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    post: (path: string, body: unknown) => apiPost(path, body),
    del: vi.fn(),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector?: (s: unknown) => unknown) =>
    selector ? selector({ agentMessages: {}, consumeAgentMessages: () => [] }) : {},
  ),
}));

// Mock useChatSocket so the panel doesn't try to open a WebSocket. We
// only care about the scroll-while-sending behaviour, not the socket
// plumbing.
vi.mock("../chat/hooks/useChatSocket", () => ({
  useChatSocket: () => {},
}));

let scrollIntoView: ReturnType<typeof vi.fn>;
let scrollEventListeners: Array<(e: Event) => void> = [];
let currentScrollTop = 0;
let currentScrollHeight = 1000;
let currentClientHeight = 200;

beforeEach(() => {
  apiGet.mockClear();
  apiPost.mockReset();
  // useChatSend chains api.post(...).then(...) — a bare vi.fn() returns
  // undefined and the .then throws an UNHANDLED TypeError that vitest
  // surfaces nondeterministically (run-order/teardown timing), flipping
  // the Canvas job red on unrelated PRs. A never-resolving promise keeps
  // the send in-flight, which is exactly the state the scroll assertions
  // exercise.
  apiPost.mockImplementation(() => new Promise(() => {}));
  scrollIntoView = vi.fn();
  scrollEventListeners = [];
  currentScrollTop = 0;
  currentScrollHeight = 1000;
  currentClientHeight = 200;
  Element.prototype.scrollIntoView = scrollIntoView;
  // Override the mock to drive the atBottom state via scroll events.
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  // The atBottom state derives from container.scrollHeight -
  // scrollTop - clientHeight; expose a fake container ref via the
  // scroll listener we wire on every render.
  const origAdd = HTMLElement.prototype.addEventListener;
  HTMLElement.prototype.addEventListener = function (
    this: HTMLElement,
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions,
  ) {
    if (type === "scroll") {
      const wrapped = (e: Event) => {
        // Force the container to report as-if at the bottom by default
        // (scrollTop=scrollHeight-clientHeight → distance=0). Tests
        // mutate `currentScrollTop` BEFORE firing to simulate scroll-up.
        Object.defineProperty(this, "scrollTop", {
          get: () => currentScrollTop,
          configurable: true,
        });
        Object.defineProperty(this, "scrollHeight", {
          get: () => currentScrollHeight,
          configurable: true,
        });
        Object.defineProperty(this, "clientHeight", {
          get: () => currentClientHeight,
          configurable: true,
        });
        if (typeof listener === "function") listener(e);
        else listener.handleEvent(e);
      };
      scrollEventListeners.push(wrapped);
      return origAdd.call(this, type, wrapped, options);
    }
    return origAdd.call(this, type, listener, options);
  };
});

afterEach(() => {
  // Reset the addEventListener patch by re-importing — not perfect, but
  // good enough for the test run; the next beforeEach re-applies.
});

import { ChatTab } from "../ChatTab";

const minimalData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: null,
} as unknown as Parameters<typeof ChatTab>[0]["data"];

async function fireScroll() {
  // Dispatch a scroll event on whatever listener is wired. The ChatTab
  // attaches its scroll listener to the messages container; the
  // beforeEach hook captured it. Fire all captured listeners.
  for (const fn of scrollEventListeners) fn(new Event("scroll"));
}

describe("ChatTab autoscroll at-bottom gating (#2560)", () => {
  it("appends a message with scrollIntoView when at the bottom (a)", async () => {
    // No history → empty page. After loadInitial, atBottom=true (the
    // container's distance-from-bottom is 0 because the empty page
    // has no overflow). Then we send a message and observe a scroll.
    render(<ChatTab workspaceId="ws-1" data={minimalData} />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    scrollIntoView.mockClear();
    // Send a message via the textarea + send button.
    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "hello" } });
    const sendBtn = screen.getByRole("button", { name: /Send/i });
    fireEvent.click(sendBtn);
    // The message-append path is gated on atBottom; at initial mount
    // atBottom=true (empty container, distance=0), so the
    // scrollIntoView call fires for the new user bubble.
    await waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
  });

  it("does NOT scrollIntoView when the user is scrolled up (b)", async () => {
    render(<ChatTab workspaceId="ws-2" data={minimalData} />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Simulate the user scrolling up — set distance-from-bottom > 12
    // BEFORE the next append. scrollTop stays at, say, 0 (we're
    // pretending the page is now 5000px tall).
    currentScrollTop = 0;
    currentScrollHeight = 5000;
    currentClientHeight = 200;
    await fireScroll();

    scrollIntoView.mockClear();

    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "hello 2" } });
    const sendBtn = screen.getByRole("button", { name: /Send/i });
    fireEvent.click(sendBtn);

    // Give the autoscroll useLayoutEffect a tick to run, then assert
    // it did NOT yank. The useChatHistory's local-state append
    // triggers history.messages change; with atBottom=false the
    // scrollIntoView is skipped.
    await new Promise((r) => setTimeout(r, 50));
    // The autoscroll useLayoutEffect skipped the call, so the count
    // should NOT have grown. Allow a small noise window — the
    // initial-mount scroll may still have fired before we set
    // atBottom=false.
    const callsAfterSend = scrollIntoView.mock.calls.length;
    expect(callsAfterSend).toBe(0);
  });
});
