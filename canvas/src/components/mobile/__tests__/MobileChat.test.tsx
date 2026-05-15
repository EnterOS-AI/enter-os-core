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
import { act, cleanup, render, waitFor } from "@testing-library/react";
import React from "react";

import { MobileChat } from "../MobileChat";

// ─── Mock API ─────────────────────────────────────────────────────────────────
// vi.mock without a factory auto-mocks the module. In tests, we configure
// api.get / api.post directly (they are vi.fn() from the auto-mock).
// Tests that need specific behaviour use mockResolvedValueOnce on the
// auto-mocked functions.
vi.mock("@/lib/api");
import { api } from "@/lib/api";

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

  it("Send button is disabled when textarea is empty (no draft)", () => {
    const { container } = renderChat(mockAgentId);
    const sendBtn = container.querySelector('[aria-label="Send"]') as HTMLButtonElement;
    expect(sendBtn.disabled).toBe(true);
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
