// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

// Capture the handler so we can drive WS events from tests. useSocketEvent
// stores the latest handler in a ref under the hood, but since we mock
// the hook entirely, just remember the last passed-in handler.
let capturedHandler: ((msg: unknown) => void) | null = null;
vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: (h: (msg: unknown) => void) => {
    capturedHandler = h;
  },
}));

// Canvas store mock — useChatSocket calls
// useCanvasStore.getState().nodes for peer name resolution and reads
// agentMessages via the selector form. Support both.
vi.mock("@/store/canvas", () => {
  const mockAgentMessages: Record<string, unknown[]> = {};
  const state = {
    nodes: [
      { id: "ws-self", data: { name: "Self" } },
      { id: "ws-peer", data: { name: "Peer Agent" } },
    ],
    agentMessages: mockAgentMessages,
    consumeAgentMessages: (workspaceId: string) => {
      const msgs = mockAgentMessages[workspaceId] ?? [];
      mockAgentMessages[workspaceId] = [];
      return msgs;
    },
  };
  const hook = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  hook.getState = () => state;
  return { useCanvasStore: hook };
});

import { useChatSocket } from "../useChatSocket";
import { useCanvasStore } from "@/store/canvas";

function getMockAgentMessages(): Record<string, unknown[]> {
  return useCanvasStore.getState().agentMessages as Record<string, unknown[]>;
}

beforeEach(() => {
  capturedHandler = null;
  const msgs = getMockAgentMessages();
  for (const key of Object.keys(msgs)) {
    delete msgs[key];
  }
});

afterEach(() => {
  vi.clearAllMocks();
});

// Helper: assemble an ACTIVITY_LOGGED a2a_receive error event the way
// the ws-server emits one when a peer call errors out. Fields mirror
// workspace-server/internal/handlers/activity.go::logActivityExec
// broadcast payload shape.
function makeActivityErrorEvent(opts: { workspaceId: string; targetId?: string; errorDetail?: string | undefined }) {
  return {
    event: "ACTIVITY_LOGGED",
    workspace_id: opts.workspaceId,
    payload: {
      activity_type: "a2a_receive",
      method: "message/send",
      status: "error",
      target_id: opts.targetId ?? opts.workspaceId,
      duration_ms: 1500,
      ...(opts.errorDetail !== undefined ? { error_detail: opts.errorDetail } : {}),
    },
    timestamp: "2026-05-18T00:00:00Z",
  };
}

describe("useChatSocket — surface error_detail to onSendError (internal#212)", () => {
  it("forwards the secret-safe error_detail from the broadcast as the onSendError reason", () => {
    const onSendError = vi.fn();
    const onSendComplete = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendError,
        onSendComplete,
      }),
    );

    expect(capturedHandler).not.toBeNull();
    act(() => {
      capturedHandler!(
        makeActivityErrorEvent({
          workspaceId: "ws-self",
          errorDetail:
            "Anthropic 403 oauth_org_not_allowed: Your organization has disabled Claude subscription access for Claude Code",
        }),
      );
    });

    // The hook must NOT fall back to the opaque hardcoded
    // "Agent error (Exception) — see workspace logs for details." —
    // that was internal#212. When the broadcast carries an
    // error_detail, that string is the user-facing reason.
    expect(onSendError).toHaveBeenCalledTimes(1);
    const reason = onSendError.mock.calls[0][0] as string;
    expect(reason).toContain("403");
    expect(reason).toContain("oauth_org_not_allowed");
    expect(reason).toContain("disabled Claude subscription");
    expect(reason).not.toMatch(/see workspace logs for details/i);
  });

  it("gracefully degrades to the legacy opaque message when error_detail is absent (older ws-server)", () => {
    // An older ws-server doesn't include error_detail in the payload.
    // The hook must still fire onSendError with the legacy hardcoded
    // text so the chat banner has SOMETHING to show. The fix is
    // additive — never depend on the new field's presence.
    const onSendError = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendError,
      }),
    );

    act(() => {
      capturedHandler!(makeActivityErrorEvent({ workspaceId: "ws-self" }));
    });

    expect(onSendError).toHaveBeenCalledTimes(1);
    const reason = onSendError.mock.calls[0][0] as string;
    // Legacy boilerplate is the floor — never silently swallow.
    expect(reason.length).toBeGreaterThan(0);
  });

  // Task #227 — external/MCP (poll-mode) workspace progress UX.
  //
  // ws-server's `proxyA2ARequest` poll-mode short-circuit fires the
  // ACTIVITY_LOGGED a2a_receive with status="ok" and NO duration_ms (no
  // reply yet — the request is queued for the agent's next poll). Before
  // task #227 the (status==="ok" && durationMs) guard silently dropped
  // this row, so the chat UI had ZERO progress signal between "user
  // typed" and "agent eventually polled and replied". Lock the queued
  // line in so future refactors don't regress to the silent-drop state.
  it("emits a 'queued — will pick up on next poll' activity line when a2a_receive status=ok has no duration_ms (poll-mode)", () => {
    const onActivityLog = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onActivityLog,
      }),
    );

    expect(capturedHandler).not.toBeNull();
    act(() => {
      capturedHandler!({
        event: "ACTIVITY_LOGGED",
        workspace_id: "ws-self",
        payload: {
          activity_type: "a2a_receive",
          method: "message/send",
          status: "ok",
          target_id: "ws-self",
          // No duration_ms — this is the queued-for-poll signal.
        },
        timestamp: "2026-05-20T00:00:00Z",
      });
    });

    expect(onActivityLog).toHaveBeenCalledTimes(1);
    const line = onActivityLog.mock.calls[0][0] as string;
    // The line MUST be present (not the empty-string silent-drop pattern)
    // and MUST mention the queued state so the user has actionable signal.
    expect(line.length).toBeGreaterThan(0);
    expect(line.toLowerCase()).toMatch(/queued|poll/);
  });

  // Pair with the above: poll-mode acknowledgement must NOT prematurely
  // call onSendComplete — the spinner has to stay up until the actual
  // AGENT_MESSAGE reply lands. (The reply-success path with duration_ms
  // still calls onSendComplete; that's the push-mode case.)
  it("does NOT call onSendComplete on a poll-mode queued a2a_receive (spinner must persist)", () => {
    const onSendComplete = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendComplete,
      }),
    );

    act(() => {
      capturedHandler!({
        event: "ACTIVITY_LOGGED",
        workspace_id: "ws-self",
        payload: {
          activity_type: "a2a_receive",
          method: "message/send",
          status: "ok",
          target_id: "ws-self",
          // No duration_ms.
        },
        timestamp: "2026-05-20T00:00:00Z",
      });
    });

    expect(onSendComplete).not.toHaveBeenCalled();
  });

  it("ignores errors targeted at a different workspace's peer", () => {
    // Defense against a race where the WS hub fans out to all clients —
    // each chat panel must only react when target_id matches its own
    // workspace.
    const onSendError = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendError,
      }),
    );
    act(() => {
      capturedHandler!(
        makeActivityErrorEvent({
          workspaceId: "ws-self",
          targetId: "ws-someone-else",
          errorDetail: "irrelevant",
        }),
      );
    });
    expect(onSendError).not.toHaveBeenCalled();
  });
});

describe("useChatSocket — token-specific completion on all paths (CR2 #11466 / Researcher #11471)", () => {
  it("passes message_id through the ACTIVITY_LOGGED error branch so onSendComplete/error are token-specific", () => {
    const onSendComplete = vi.fn();
    const onSendError = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendComplete,
        onSendError,
      }),
    );

    act(() => {
      capturedHandler!({
        event: "ACTIVITY_LOGGED",
        workspace_id: "ws-self",
        payload: {
          activity_type: "a2a_receive",
          method: "message/send",
          status: "error",
          target_id: "ws-self",
          duration_ms: 1500,
          message_id: "msg-a",
          error_detail: "Provider 503",
        },
        timestamp: "2026-05-18T00:00:00Z",
      });
    });

    expect(onSendComplete).toHaveBeenCalledTimes(1);
    expect(onSendComplete).toHaveBeenCalledWith("msg-a");
    expect(onSendError).toHaveBeenCalledTimes(1);
    expect(onSendError.mock.calls[0][1]).toBe("msg-a");
  });

  it("falls back to legacy no-messageId completion only when message_id is absent", () => {
    const onSendComplete = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onSendComplete,
      }),
    );

    act(() => {
      capturedHandler!({
        event: "ACTIVITY_LOGGED",
        workspace_id: "ws-self",
        payload: {
          activity_type: "a2a_receive",
          method: "message/send",
          status: "error",
          target_id: "ws-self",
          duration_ms: 1500,
          // message_id intentionally absent — old ws-server build.
        },
        timestamp: "2026-05-18T00:00:00Z",
      });
    });

    expect(onSendComplete).toHaveBeenCalledTimes(1);
    expect(onSendComplete.mock.calls[0][0]).toBeUndefined();
  });

  it("completes EVERY consumed agent message by its message_id, not just the first (batch consume)", () => {
    getMockAgentMessages()["ws-self"] = [
      { id: "1", content: "first", timestamp: new Date().toISOString(), messageId: "msg-1" },
      { id: "2", content: "second", timestamp: new Date().toISOString(), messageId: "msg-2" },
    ];

    const onAgentMessage = vi.fn();
    const onSendComplete = vi.fn();
    renderHook(() =>
      useChatSocket("ws-self", {
        onAgentMessage,
        onSendComplete,
      }),
    );

    expect(onAgentMessage).toHaveBeenCalledTimes(2);
    expect(onSendComplete).toHaveBeenCalledTimes(2);
    expect(onSendComplete.mock.calls[0][0]).toBe("msg-1");
    expect(onSendComplete.mock.calls[1][0]).toBe("msg-2");
  });
});
