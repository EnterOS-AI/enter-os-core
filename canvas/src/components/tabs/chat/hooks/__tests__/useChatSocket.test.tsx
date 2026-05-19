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
  const state = {
    nodes: [
      { id: "ws-self", data: { name: "Self" } },
      { id: "ws-peer", data: { name: "Peer Agent" } },
    ],
    agentMessages: {} as Record<string, unknown[]>,
    consumeAgentMessages: () => [],
  };
  const hook = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  hook.getState = () => state;
  return { useCanvasStore: hook };
});

import { useChatSocket } from "../useChatSocket";

beforeEach(() => {
  capturedHandler = null;
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
