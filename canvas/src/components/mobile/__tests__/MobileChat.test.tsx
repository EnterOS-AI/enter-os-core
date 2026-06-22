// @vitest-environment jsdom
/**
 * MobileChat — mobile message thread + composer + sub-tabs.
 *
 * Per spec §04: wired to /workspaces/:id/a2a (method message/send).
 * Slimmer surface than desktop ChatTab: no attachments, no topology overlay.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { MobileChat } from "../MobileChat";

// ─── Mock API ─────────────────────────────────────────────────────────────────
// vi.mock without a factory auto-mocks the module. In tests, we configure
// api.get / api.post directly (they are vi.fn() from the auto-mock).
// Tests that need specific behaviour use mockResolvedValueOnce on the
// auto-mocked functions.
vi.mock("@/lib/api");
import { api } from "@/lib/api";

// AgentCommsPanel (mounted by the Agent Comms sub-tab, #231) subscribes
// to the global socket via useSocketEvent. Stub it to a no-op so the
// panel mounts without the real ReconnectingSocket — the parity tests
// only assert the panel renders (vs the old static placeholder).
vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: vi.fn(),
}));

// ─── Mock store ───────────────────────────────────────────────────────────────

const mockAgentId = "ws-chat-test";
const mockOnBack = vi.fn();

// Module-level mutable state for the mock store.
const mockStoreState = {
  nodes: [] as Array<{
    id: string;
    position: { x: number; y: number };
    data: Record<string, unknown>;
    width?: number;
    height?: number;
  }>,
  agentMessages: {} as Record<string, Array<{ id: string; content: string; timestamp: string }>>,
  consumeAgentMessages: () => [],
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((sel?: (state: typeof mockStoreState) => unknown) => {
      if (sel) return sel(mockStoreState);
      return mockStoreState;
    }),
    {
      getState: () => mockStoreState,
      subscribe: vi.fn(() => vi.fn()),
    },
  ),
  summarizeWorkspaceCapabilities: vi.fn((data: Record<string, unknown>) => {
    const agentCard = data.agentCard as Record<string, unknown> | null;
    const skills = Array.isArray(agentCard?.skills)
      ? (agentCard.skills as Array<Record<string, unknown>>).map(
          (s) => String(s.name || s.id || ""),
        ).filter(Boolean)
      : [];
    return {
      runtime: (typeof data.runtime === "string" && data.runtime)
        ? data.runtime
        : (typeof agentCard?.runtime === "string" ? String(agentCard.runtime) : null),
      skills,
      skillCount: skills.length,
      currentTask: String(data.currentTask ?? ""),
      hasActiveTask: String(data.currentTask ?? "").trim().length > 0,
    };
  }),
}));

// ─── Fixtures ────────────────────────────────────────────────────────────────

const onlineNode = {
  id: mockAgentId,
  position: { x: 0, y: 0 },
  data: {
    name: "Chat Agent",
    status: "online",
    tier: 2,
    agentCard: {
      runtime: "claude-code",
      skills: [{ name: "web-search" }],
    },
    currentTask: "",
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    parentId: null,
    runtime: "claude-code",
    needsRestart: false,
  },
};

const offlineNode = {
  id: "ws-offline",
  position: { x: 0, y: 0 },
  data: {
    name: "Offline Agent",
    status: "offline",
    tier: 1,
    agentCard: null,
    currentTask: "",
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    parentId: null,
    runtime: "claude-code",
    needsRestart: false,
  },
};

const degradedNode = {
  id: "ws-degraded",
  position: { x: 0, y: 0 },
  data: {
    name: "Degraded Agent",
    status: "degraded",
    tier: 3,
    agentCard: null,
    currentTask: "",
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    parentId: null,
    runtime: "claude-code",
    needsRestart: false,
  },
};

// ─── Helpers ─────────────────────────────────────────────────────────────────

function renderChat(agentId: string, dark = false) {
  return render(
    <MobileChat
      agentId={agentId}
      dark={dark}
      onBack={mockOnBack}
    />,
  );
}

// ─── Setup / teardown ─────────────────────────────────────────────────────────

beforeEach(() => {
  mockOnBack.mockClear();
  mockStoreState.nodes = [];
  mockStoreState.agentMessages = {};
  // jsdom doesn't implement scrollIntoView. The Agent Comms tab now
  // mounts AgentCommsPanel (#231), which scrolls its feed to bottom on
  // arrival; a no-op stub keeps the panel from throwing under jsdom
  // (same stub AgentCommsPanel's own render test installs).
  Element.prototype.scrollIntoView =
    vi.fn() as unknown as Element["scrollIntoView"];
  // Set up spies on the real api methods. Tests override these per-call.
  const getSpy = vi.spyOn(api, "get");
  const postSpy = vi.spyOn(api, "post");
  getSpy.mockResolvedValue({ messages: [], reached_end: true });
  postSpy.mockResolvedValue({ result: { parts: [] } });
});

afterEach(() => {
  vi.restoreAllMocks();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ─── Not found ───────────────────────────────────────────────────────────────

describe("MobileChat — agent not found", () => {
  it('renders "Agent not found." when node is absent', () => {
    mockStoreState.nodes = [onlineNode];
    const { container } = renderChat("nonexistent-id");
    expect(container.textContent ?? "").toContain("Agent not found.");
  });
});

// ─── Header ──────────────────────────────────────────────────────────────────

describe("MobileChat — header", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders Back button with aria-label", () => {
    const { container } = renderChat(mockAgentId);
    const backBtn = container.querySelector('[aria-label="Back"]');
    expect(backBtn).toBeTruthy();
  });

  it("Back button calls onBack", () => {
    const { container } = renderChat(mockAgentId);
    const backBtn = container.querySelector('[aria-label="Back"]') as HTMLButtonElement;
    backBtn.click();
    expect(mockOnBack).toHaveBeenCalledTimes(1);
  });

  it("renders agent name in header", () => {
    const { container } = renderChat(mockAgentId);
    expect(container.textContent ?? "").toContain("Chat Agent");
  });

  it("renders a More button", () => {
    const { container } = renderChat(mockAgentId);
    const moreBtn = container.querySelector('[aria-label="More"]');
    expect(moreBtn).toBeTruthy();
  });

  it("renders footer with agentId", () => {
    const { container } = renderChat(mockAgentId);
    expect(container.textContent ?? "").toContain(mockAgentId);
  });
});

// ─── Composer ────────────────────────────────────────────────────────────────

describe("MobileChat — composer", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders a textarea for message input", () => {
    const { container } = renderChat(mockAgentId);
    const textarea = container.querySelector("textarea");
    expect(textarea).toBeTruthy();
  });

  it("textarea has placeholder text", () => {
    const { container } = renderChat(mockAgentId);
    const textarea = container.querySelector("textarea") as HTMLTextAreaElement;
    expect(textarea.placeholder).toBeTruthy();
    expect(textarea.placeholder).toContain("Send a message");
  });

  it("renders a Send button with aria-label", () => {
    const { container } = renderChat(mockAgentId);
    const sendBtn = container.querySelector('[aria-label="Send"]');
    expect(sendBtn).toBeTruthy();
  });

  it("Send button is disabled when textarea is empty (no draft)", async () => {
    const { container } = renderChat(mockAgentId);
    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    expect(sendBtn.disabled).toBe(true);
  });

  // Regression #2762-follow-up: the send cursor used `pendingFiles.length === 0`,
  // so attaching a file without typing text left the button enabled but showed a
  // `not-allowed` cursor. It must be consistent with the disabled prop.
  it("Send button is enabled and shows pointer cursor when files are attached but draft is empty", async () => {
    const { container } = renderChat(mockAgentId);
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(["hello"], "note.txt", { type: "text/plain" });
    await act(async () => {
      fireEvent.change(fileInput, { target: { files: [file] } });
    });

    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    expect(sendBtn.disabled).toBe(false);
    expect(sendBtn.style.cursor).toBe("pointer");
  });

  // Regression #224: the composer textarea must render with font-size
  // ≥ 16px. iOS Safari and PWAs auto-zoom the viewport when a focused
  // input has a computed font-size below 16px — the layout jumps and
  // the page looks broken until the user pinches back. Same class as
  // desktop #1434 / sibling MobileSpawn #225.
  it("composer textarea renders at font-size 16px or greater (iOS focus-zoom regression #224)", () => {
    const { container } = renderChat(mockAgentId);
    const textarea = container.querySelector("textarea") as HTMLTextAreaElement;
    expect(textarea).toBeTruthy();
    const fs = Number.parseFloat(textarea.style.fontSize);
    expect(Number.isFinite(fs)).toBe(true);
    expect(fs).toBeGreaterThanOrEqual(16);
  });
});

// ─── Tabs ─────────────────────────────────────────────────────────────────────

describe("MobileChat — tabs", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders My Chat and Agent Comms tab labels", () => {
    const { container } = renderChat(mockAgentId);
    const text = container.textContent ?? "";
    expect(text).toContain("My Chat");
    expect(text).toContain("Agent Comms");
  });

  it("defaults to My Chat tab", () => {
    const { container } = renderChat(mockAgentId);
    // My Chat is the default; if there are no messages it should show the empty state
    expect(container.textContent ?? "").toContain("My Chat");
  });
});

// ─── Empty state ─────────────────────────────────────────────────────────────

describe("MobileChat — empty state", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it('shows "Send a message to start chatting." when no messages', async () => {
    // History fetch resolves immediately in tests (mockResolvedValue).
    // act() flushes the microtask queue so the component reaches its
    // post-load state before we assert.
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("Send a message to start chatting.");
  });

  it("shows no messages when agentMessages[agentId] is absent (undefined)", async () => {
    // Explicitly set to empty to simulate no stored messages
    mockStoreState.agentMessages = {};
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("Send a message to start chatting.");
  });
});

// ─── Agent status ────────────────────────────────────────────────────────────

describe("MobileChat — agent status", () => {
  it("renders composer for online agent", () => {
    mockStoreState.nodes = [onlineNode];
    const { container } = renderChat(mockAgentId);
    expect(container.querySelector("textarea")).toBeTruthy();
  });

  it("renders composer for offline agent (with status text)", () => {
    mockStoreState.nodes = [offlineNode];
    const { container } = renderChat("ws-offline");
    const textarea = container.querySelector("textarea") as HTMLTextAreaElement;
    // Offline agent: textarea should be disabled
    expect(textarea.disabled).toBe(true);
  });

  it("renders composer for degraded agent", () => {
    mockStoreState.nodes = [degradedNode];
    const { container } = renderChat("ws-degraded");
    expect(container.querySelector("textarea")).toBeTruthy();
  });

  it("offline agent shows agent name", () => {
    mockStoreState.nodes = [offlineNode];
    const { container } = renderChat("ws-offline");
    expect(container.textContent ?? "").toContain("Offline Agent");
  });
});

// ─── Dark mode ───────────────────────────────────────────────────────────────

describe("MobileChat — dark mode", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders without crashing in dark mode", () => {
    const { container } = renderChat(mockAgentId, true);
    expect(container.querySelector('[aria-label="Back"]')).toBeTruthy();
  });
});

// ─── Chat history loading ────────────────────────────────────────────────────

describe("MobileChat — chat history", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("calls GET /workspaces/:id/chat-history on mount", async () => {
    await act(async () => {
      renderChat(mockAgentId);
    });
    expect(api.get).toHaveBeenCalledWith(
      expect.stringContaining(`/workspaces/${mockAgentId}/chat-history`),
    );
  });

  it("shows loading state while history is fetching", () => {
    // Do NOT await — check the pre-resolve state.
    const { container } = renderChat(mockAgentId);
    expect(container.textContent ?? "").toContain("Loading chat history…");
  });

  it("shows empty state after history resolves with no messages", async () => {
    // beforeEach already sets api.get to resolve with empty — no override needed.
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("Send a message to start chatting.");
  });

  it("renders messages from history response", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce({
      messages: [
        {
          id: "msg-1",
          role: "user",
          content: "Hello agent",
          timestamp: "2026-04-25T10:00:00Z",
        },
        {
          id: "msg-2",
          role: "agent",
          content: "Hello back",
          timestamp: "2026-04-25T10:00:01Z",
        },
      ],
      reached_end: true,
    });
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("Hello agent");
    expect(container.textContent ?? "").toContain("Hello back");
  });

  it("maps user role from API correctly", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce({
      messages: [
        {
          id: "msg-u",
          role: "user",
          content: "user message",
          timestamp: "2026-04-25T10:00:00Z",
        },
      ],
      reached_end: true,
    });
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    // User messages render right-aligned. The text content check is sufficient
    // to confirm the message appeared.
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("user message");
  });

  it("shows error state when history fetch fails", async () => {
    vi.spyOn(api, "get").mockRejectedValue(new Error("Network error"));
    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;
    expect(container.textContent ?? "").toContain("Could not load chat history.");
    expect(container.textContent ?? "").toContain("Retry");
  });

  it("Retry button re-fetches history after error", async () => {
    // Make the initial mount call fail so the Retry button appears, then
    // make the retry call succeed so we can verify the full flow.
    const getSpy = vi.spyOn(api, "get");
    getSpy
      .mockRejectedValueOnce(new Error("Network error"))
      .mockResolvedValueOnce({ messages: [], reached_end: true });

    let renderResult: ReturnType<typeof renderChat>;
    await act(async () => {
      renderResult = renderChat(mockAgentId);
    });
    const { container } = renderResult!;

    // Error state should be shown with Retry button.
    expect(container.textContent ?? "").toContain("Could not load chat history.");
    expect(container.textContent ?? "").toContain("Retry");

    // Click Retry — the button's onClick fires api.get again.
    // The second mockResolvedValueOnce makes it succeed.
    const retryBtn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Retry",
    );
    expect(retryBtn).toBeTruthy();
    await act(async () => {
      retryBtn?.click();
    });

    // waitFor polls until the retry resolves and component re-renders.
    await waitFor(() => {
      expect(container.textContent ?? "").toContain("Send a message to start chatting.");
    });
    // Initial call + retry = 2.
    expect(getSpy).toHaveBeenCalledTimes(2);
  });
});

// ─── #232 · Attachment render parity with desktop ChatTab ────────────────────
//
// Regression for the CTO-reported mobile bug: MobileChat used to render
// only m.content (no attachment surface), so files sent/received in a
// conversation were invisible on mobile while desktop showed them. The
// fix routes m.attachments through the same AttachmentPreview the
// desktop ChatTab bubble uses.

describe("MobileChat — attachment render parity (#232)", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders an attachment from a history message via AttachmentPreview", async () => {
    const getSpy = vi.spyOn(api, "get");
    // useChatHistory reads { messages, reached_end }.
    getSpy.mockResolvedValueOnce({
      messages: [
        {
          id: "m-att-1",
          role: "agent",
          content: "Here is the report",
          attachments: [
            {
              name: "report.csv",
              uri: "workspace://out/report.csv",
              mimeType: "text/csv",
              size: 2048,
            },
          ],
          timestamp: new Date().toISOString(),
        },
      ],
      reached_end: true,
    });

    let rr: ReturnType<typeof renderChat>;
    await act(async () => {
      rr = renderChat(mockAgentId);
    });
    const { container } = rr!;

    // A non-image attachment renders the AttachmentChip download button
    // with title="Download <name>" — same component the desktop bubble
    // dispatches through AttachmentPreview.
    await waitFor(() => {
      const chip = container.querySelector('[title="Download report.csv"]');
      expect(chip).toBeTruthy();
    });
    expect(container.textContent ?? "").toContain("report.csv");
  });
});

// ─── #231 · Agent Comms (A2A/peer) render parity with desktop ChatTab ────────
//
// Regression for the CTO-reported mobile bug: the Agent Comms sub-tab
// rendered a static placeholder string ("peer-to-peer A2A traffic
// surfaces in the Comms tab") instead of the real feed. The fix mounts
// the same AgentCommsPanel the desktop ChatTab agent-comms tabpanel
// uses, so peer/A2A + delegation activity is visible on mobile.

describe("MobileChat — Agent Comms render parity (#231)", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("mounts AgentCommsPanel on the Agent Comms tab (not the old placeholder)", async () => {
    const getSpy = vi.spyOn(api, "get");
    // 1st GET: useChatHistory (My Chat) on mount.
    getSpy.mockResolvedValueOnce({ messages: [], reached_end: true });
    // 2nd GET: AgentCommsPanel's activity load when the tab is shown.
    // Empty list → panel renders its own empty state, which still
    // proves AgentCommsPanel mounted (vs. the removed placeholder).
    getSpy.mockResolvedValueOnce([]);

    let rr: ReturnType<typeof renderChat>;
    await act(async () => {
      rr = renderChat(mockAgentId);
    });
    const { container } = rr!;

    const commsTab = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Agent Comms",
    );
    expect(commsTab).toBeTruthy();
    await act(async () => {
      commsTab!.click();
    });

    await waitFor(() => {
      const text = container.textContent ?? "";
      // The panel's empty state — proves AgentCommsPanel mounted.
      expect(text).toContain("No agent-to-agent communications yet.");
    });
    // The old hard-coded placeholder must be gone.
    expect(container.textContent ?? "").not.toContain(
      "peer-to-peer A2A traffic surfaces in the Comms tab",
    );
    // The panel hit its activity endpoint.
    expect(getSpy).toHaveBeenCalledWith(
      expect.stringContaining(`/workspaces/${mockAgentId}/activity`),
    );
  });

  it("renders a peer message on the Agent Comms tab", async () => {
    const getSpy = vi.spyOn(api, "get");
    getSpy.mockResolvedValueOnce({ messages: [], reached_end: true });
    // a2a_receive from a peer → AgentCommsPanel.toCommMessage maps it
    // to an inbound bubble with the request text.
    getSpy.mockResolvedValueOnce([
      {
        id: "act-1",
        activity_type: "a2a_receive",
        source_id: "peer-ws-uuid",
        target_id: mockAgentId,
        method: "message/send",
        summary: "peer asked something",
        request_body: { task: "Please review PR 42" },
        response_body: null,
        status: "ok",
        created_at: new Date().toISOString(),
      },
    ]);

    let rr: ReturnType<typeof renderChat>;
    await act(async () => {
      rr = renderChat(mockAgentId);
    });
    const { container } = rr!;

    const commsTab = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Agent Comms",
    );
    await act(async () => {
      commsTab!.click();
    });

    await waitFor(() => {
      expect(container.textContent ?? "").toContain("Please review PR 42");
    });
  });
});

describe("MobileChat — thinking indicator (core#2720/#2745 parity)", () => {
  it("shows the thinking indicator when the workspace reports a currentTask", () => {
    const busy = { ...onlineNode, data: { ...onlineNode.data, currentTask: "downloading assets" } };
    mockStoreState.nodes = [busy];
    const { container } = renderChat(mockAgentId);
    expect(container.querySelector('[data-testid="mobile-thinking-indicator"]')).not.toBeNull();
  });

  it("hides the thinking indicator when idle (online, no currentTask, not sending)", () => {
    mockStoreState.nodes = [onlineNode];
    const { container } = renderChat(mockAgentId);
    expect(container.querySelector('[data-testid="mobile-thinking-indicator"]')).toBeNull();
  });
});

describe("MobileChat — multi-send tap path (CR2 #2762)", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("Send button stays ENABLED during an in-flight send (tap multi-send)", async () => {
    // First send hangs → sending stays true.
    vi.spyOn(api, "post").mockReturnValueOnce(new Promise(() => {}));
    const { container } = renderChat(mockAgentId);
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = () => container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    await act(async () => {
      fireEvent.change(ta, { target: { value: "first" } });
    });
    expect(sendBtn().disabled).toBe(false);
    await act(async () => {
      sendBtn().click(); // starts the hanging send → sending=true
    });
    // Type a follow-up; the button MUST remain tappable (pre-fix it disabled on `sending`).
    await act(async () => {
      fireEvent.change(ta, { target: { value: "second" } });
    });
    expect(sendBtn().disabled).toBe(false);
  });

  it("Attach button stays ENABLED during an in-flight send (core#2762 follow-up)", async () => {
    // First send hangs → sending stays true.
    vi.spyOn(api, "post").mockReturnValueOnce(new Promise(() => {}));
    const { container } = renderChat(mockAgentId);
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    const attachBtn = container.querySelector('[aria-label="Attach"]') as HTMLButtonElement;

    await act(async () => {
      fireEvent.change(ta, { target: { value: "first" } });
    });
    await act(async () => {
      sendBtn.click();
    });

    // Attach must remain usable while the send is in flight.
    expect(attachBtn.disabled).toBe(false);

    // Selecting a file while sending should add it to the pending list.
    const fileInput = container.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(["hello"], "note.txt", { type: "text/plain" });
    await act(async () => {
      fireEvent.change(fileInput, { target: { files: [file] } });
    });
    expect(container.textContent ?? "").toContain("note.txt");
  });

  it("clears the send-error banner when a follow-up send starts (multi-send banner-clear)", async () => {
    const postSpy = vi.spyOn(api, "post");
    // First send fails → error banner.
    postSpy.mockRejectedValueOnce(new Error("boom"));
    // Second send succeeds → banner must clear.
    postSpy.mockResolvedValueOnce({ result: { parts: [] } });

    const { container } = renderChat(mockAgentId);
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;

    await act(async () => {
      fireEvent.change(ta, { target: { value: "first" } });
    });
    await act(async () => {
      sendBtn.click();
    });
    await waitFor(() => {
      expect(container.textContent ?? "").toContain("unreachable");
    });

    // Start a follow-up send; the banner should clear as soon as the new
    // send is dispatched (send() calls clearError before awaiting the POST).
    await act(async () => {
      fireEvent.change(ta, { target: { value: "second" } });
    });
    await act(async () => {
      sendBtn.click();
    });
    await waitFor(() => {
      expect(container.textContent ?? "").not.toContain("unreachable");
    });
  });

  it("restores composer draft after a genuine send error (mc#2908 F7)", async () => {
    vi.spyOn(api, "post").mockRejectedValueOnce(new Error("boom"));

    const { container } = renderChat(mockAgentId);
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;

    await act(async () => {
      fireEvent.change(ta, { target: { value: "retry me" } });
    });
    await act(async () => {
      sendBtn.click();
    });
    await waitFor(() => {
      expect(container.textContent ?? "").toContain("unreachable");
    });

    // Composer should be restored so the user can retry without retyping.
    expect(ta.value).toBe("retry me");
  });
});

describe("MobileChat — tool-call chain (#231 desktop parity)", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders the tool-call chain from an agent message's tool_trace", async () => {
    // useChatHistory maps the API's snake_case tool_trace → toolTrace.
    vi.spyOn(api, "get").mockResolvedValueOnce({
      messages: [
        {
          id: "m-trace",
          role: "agent",
          content: "done",
          timestamp: "2026-04-25T10:00:01Z",
          tool_trace: [
            { tool: "Bash", input: "ls -la" },
            { tool: "Read", input: "/tmp/x" },
          ],
        },
      ],
      reached_end: true,
    });
    let r: ReturnType<typeof renderChat>;
    await act(async () => {
      r = renderChat(mockAgentId);
    });
    const { container } = r!;
    // Collapsed-by-default: the count header is shown (previously mobile
    // dropped the whole chain — the reported "missing tool call chain").
    expect(container.textContent ?? "").toContain("2 tools used");
    // Expand → the individual tool entries appear.
    const toggle = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent ?? "").includes("tools used"),
    ) as HTMLButtonElement;
    expect(toggle).toBeTruthy();
    await act(async () => {
      toggle.click();
    });
    expect(container.textContent ?? "").toContain("Bash");
    expect(container.textContent ?? "").toContain("Read");
  });

  it("shows no tool-chain affordance for an agent message without tool_trace", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce({
      messages: [
        { id: "m-plain", role: "agent", content: "hi", timestamp: "2026-04-25T10:00:01Z" },
      ],
      reached_end: true,
    });
    let r: ReturnType<typeof renderChat>;
    await act(async () => {
      r = renderChat(mockAgentId);
    });
    expect(r!.container.textContent ?? "").not.toContain("tools used");
  });
});

describe("MobileChat — send-error banner suppressed while working (report: banner under ●●● 148s)", () => {
  const busyNode = {
    ...onlineNode,
    id: "ws-busy",
    data: { ...onlineNode.data, currentTask: "downloading 1100 files" },
  };

  it("does NOT show the 'unreachable' banner while the agent is working (currentTask set)", async () => {
    mockStoreState.nodes = [busyNode];
    // The send POST fails with a 504 (non-524) → useChatSend sets the
    // "agent may be unreachable" error. But the workspace reports an in-flight
    // task, so thinking=true and the banner must stay HIDDEN (the agent is
    // provably reachable — the exact contradiction in the screenshot).
    vi.spyOn(api, "post").mockRejectedValue(
      Object.assign(new Error("API POST /workspaces/ws-busy/a2a: 504 "), { status: 504 }),
    );
    const { container } = renderChat("ws-busy");
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = () =>
      container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    await act(async () => {
      fireEvent.change(ta, { target: { value: "hi" } });
    });
    await act(async () => {
      sendBtn().click();
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    // No "unreachable" banner while currentTask drives the thinking indicator.
    expect(container.textContent ?? "").not.toMatch(/unreachable/i);
    // …and the thinking indicator IS present (proving thinking=true, the state
    // under which the old code wrongly showed the banner).
    expect(
      container.querySelector('[data-testid="mobile-thinking-indicator"]'),
    ).not.toBeNull();
  });

  it("DOES show the banner when a send fails and the agent is NOT working", async () => {
    mockStoreState.nodes = [onlineNode]; // currentTask: "" → not thinking once send settles
    vi.spyOn(api, "post").mockRejectedValue(
      Object.assign(new Error("API POST /workspaces/ws-chat-test/a2a: 522 "), { status: 522 }),
    );
    const { container } = renderChat(mockAgentId);
    const ta = container.querySelector("textarea") as HTMLTextAreaElement;
    const sendBtn = () =>
      container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    await act(async () => {
      fireEvent.change(ta, { target: { value: "hi" } });
    });
    await act(async () => {
      sendBtn().click();
    });
    await waitFor(() => {
      expect(container.textContent ?? "").toMatch(/unreachable/i);
    });
  });
});
