// @vitest-environment jsdom
//
// Pins internal#212 — the chat error banner must:
//
//   1. Render the secret-safe failure reason (e.g. the provider's own
//      "403 oauth_org_not_allowed: ..." string), NOT the opaque
//      hardcoded "Agent error (Exception) — see workspace logs for
//      details." that points at a workspace-logs tab that doesn't
//      exist.
//
//   2. Offer a working "View activity log" affordance that navigates
//      the user to the Activity tab where the full row lives.
//
// Tested at the banner-component seam (ChatErrorBanner). The
// hook-level path is pinned separately by
// chat/hooks/__tests__/useChatSocket.test.tsx — together they cover
// wire-payload → callback → render without each test needing to drive
// the full ChatTab send-state machinery.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";

afterEach(cleanup);

const mocks = vi.hoisted(() => ({
  setPanelTabMock: vi.fn(),
}));

vi.mock("@/store/canvas", () => {
  const state = {
    setPanelTab: mocks.setPanelTabMock,
    panelTab: "chat",
  };
  const hook = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  hook.getState = () => state;
  return { useCanvasStore: hook };
});

beforeEach(() => {
  mocks.setPanelTabMock.mockClear();
});

import { ChatErrorBanner } from "../chat/ChatErrorBanner";

describe("ChatErrorBanner — surfaces actionable reason (internal#212)", () => {
  it("renders the secret-safe failure reason verbatim, not a hardcoded opaque message", () => {
    const reason =
      "Anthropic 403 oauth_org_not_allowed: Your organization has disabled Claude subscription access for Claude Code — use an Anthropic API key or ask your admin to enable access.";
    render(<ChatErrorBanner message={reason} isOnline={true} onRestart={() => {}} />);
    expect(screen.getByText(/oauth_org_not_allowed/i)).toBeDefined();
    expect(screen.getByText(/disabled Claude subscription access/i)).toBeDefined();
    // The legacy boilerplate must NOT leak through when a real reason
    // is provided.
    expect(screen.queryByText(/see workspace logs for details/i)).toBeNull();
  });

  it("falls back to the message when it IS the legacy boilerplate (older ws-server)", () => {
    // Graceful degradation: an older ws-server passes through the
    // hardcoded text; the banner still renders SOMETHING — never
    // silently swallow.
    render(
      <ChatErrorBanner
        message="Agent error (Exception) — see workspace logs for details."
        isOnline={true}
        onRestart={() => {}}
      />,
    );
    expect(
      screen.getByText(/Agent error \(Exception\) — see workspace logs for details\./),
    ).toBeDefined();
  });

  it("offers a 'View activity log' button that calls setPanelTab('activity')", () => {
    render(
      <ChatErrorBanner message="kimi 401 invalid_api_key" isOnline={true} onRestart={() => {}} />,
    );
    const btn = screen.getByRole("button", { name: /view activity log/i });
    fireEvent.click(btn);
    expect(mocks.setPanelTabMock).toHaveBeenCalledWith("activity");
  });

  it("still shows the Restart button when offline (existing behavior preserved)", () => {
    const onRestart = vi.fn();
    render(
      <ChatErrorBanner message="Agent is offline" isOnline={false} onRestart={onRestart} />,
    );
    const btn = screen.getByRole("button", { name: /^restart$/i });
    fireEvent.click(btn);
    expect(onRestart).toHaveBeenCalledTimes(1);
  });

  it("renders nothing when message is null", () => {
    const { container } = render(
      <ChatErrorBanner message={null} isOnline={true} onRestart={() => {}} />,
    );
    expect(container.textContent).toBe("");
  });
});
