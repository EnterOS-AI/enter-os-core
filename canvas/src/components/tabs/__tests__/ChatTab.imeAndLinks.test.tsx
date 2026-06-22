// @vitest-environment jsdom
//
// Pins two regressions reported on production 2026-05-05:
//
// 1. IME composition + Enter key: typing Chinese (or any CJK / IME-
//    composed text) and pressing Enter to commit the candidate
//    selection used to send the half-typed message. The fix checks
//    `event.nativeEvent.isComposing` (and a `keyCode === 229`
//    fallback for older WebKit) before treating Enter as send.
//
// 2. Markdown link clicks: the agent's ReactMarkdown-rendered links
//    used to:
//       - http/https → navigate canvas tab away (user lost canvas state)
//       - workspace://path / file:///workspace/... / /workspace/... →
//         browser hit about:blank (unhandled protocol).
//    Fix: external links get target="_blank" + noopener; in-container
//    paths route through downloadChatFile (same auth path as chips).

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// Mock the api module so render doesn't try to talk to a real CP.
const apiGet = vi.fn((_path: string): Promise<unknown> => Promise.resolve([]));
const apiPost = vi.fn((_path: string, _body: unknown): Promise<unknown> => Promise.resolve({}));
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

// Capture the downloadChatFile call so the markdown-link test can
// assert in-container paths route through the authenticated download
// path rather than the browser's bare anchor click.
const downloadChatFileMock = vi.fn((_workspaceId: string, _att: { uri: string; name: string }) => Promise.resolve());
vi.mock("../chat/uploads", async () => {
  const actual = await vi.importActual<typeof import("../chat/uploads")>("../chat/uploads");
  return {
    ...actual,
    downloadChatFile: (workspaceId: string, att: { uri: string; name: string }) =>
      downloadChatFileMock(workspaceId, att),
  };
});

beforeEach(() => {
  apiGet.mockClear();
  apiPost.mockClear();
  downloadChatFileMock.mockClear();
  // jsdom doesn't implement scrollIntoView; ChatTab calls it after
  // every render with a new message.
  Element.prototype.scrollIntoView = vi.fn();
  // Stub IntersectionObserver — the lazy-history sentinel uses it.
  class FakeIO {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
  (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
});

import { ChatTab } from "../ChatTab";

const minimalData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: null,
} as unknown as Parameters<typeof ChatTab>[0]["data"];

describe("ChatTab — IME-safe Enter key", () => {
  it("does NOT send the message when Enter fires during IME composition (isComposing)", async () => {
    render(<ChatTab workspaceId="ws-ime" data={minimalData} />);

    // Find the textarea by its aria-label.
    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "你好" } });

    // Simulate the Enter that commits an IME selection: isComposing=true.
    fireEvent.keyDown(textarea, { key: "Enter", isComposing: true });

    // sendMessage POSTs via api.post; assert it was NOT called.
    await waitFor(() => {
      expect(apiPost).not.toHaveBeenCalled();
    });
    // And the input is preserved — ChatTab clears it only on actual send.
    expect((textarea as HTMLTextAreaElement).value).toBe("你好");
  });

  it("does NOT send when keyCode is 229 (older Safari IME fallback)", async () => {
    render(<ChatTab workspaceId="ws-ime2" data={minimalData} />);
    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "한국어" } });

    // keyCode 229 is the older-Safari signal that an IME is composing.
    // Some mobile WebKit-based browsers delay setting isComposing on
    // the composition-end Enter; the keyCode fallback covers that.
    fireEvent.keyDown(textarea, { key: "Enter", keyCode: 229 });

    await waitFor(() => {
      expect(apiPost).not.toHaveBeenCalled();
    });
  });

  it("DOES send on a non-composing Enter (the happy path stays intact)", async () => {
    render(<ChatTab workspaceId="ws-ok" data={minimalData} />);
    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "hello world" } });

    fireEvent.keyDown(textarea, { key: "Enter" /* no isComposing, no 229 */ });

    // The api.post for /a2a fires inside sendMessage. waitFor since
    // the call goes through several effects.
    await waitFor(() => {
      expect(apiPost).toHaveBeenCalled();
    });
  });

  it("Shift+Enter inserts newline regardless (no send)", async () => {
    render(<ChatTab workspaceId="ws-shift" data={minimalData} />);
    const textarea = await screen.findByLabelText(/Message to agent/i);
    fireEvent.change(textarea, { target: { value: "line 1" } });

    fireEvent.keyDown(textarea, { key: "Enter", shiftKey: true });

    await waitFor(() => {
      expect(apiPost).not.toHaveBeenCalled();
    });
  });
});
