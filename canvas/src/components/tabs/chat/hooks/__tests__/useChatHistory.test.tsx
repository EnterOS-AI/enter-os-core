// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useChatHistory } from "../useChatHistory";

const apiGet = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => apiGet(...args),
  },
}));

function makeMsg(id: number, ts?: string): {
  id: string;
  role: "user" | "agent" | "system";
  content: string;
  timestamp: string;
} {
  return {
    id: `msg-${id}`,
    role: "user",
    content: `content ${id}`,
    timestamp: ts ?? new Date(2026, 0, 1, 0, 0, id).toISOString(),
  };
}

function mockContainer(): React.RefObject<HTMLDivElement | null> {
  const el = document.createElement("div");
  return { current: el as HTMLDivElement };
}

describe("useChatHistory — message buffer cap (mobile-chat audit F4)", () => {
  beforeEach(() => {
    apiGet.mockReset();
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it("caps the in-memory message buffer at MAX_MESSAGES when loading older history", async () => {
    // Seed initial messages so hasMore is true and there is an oldest timestamp.
    const initial = Array.from({ length: 10 }, (_, i) => makeMsg(i + 1));
    apiGet.mockResolvedValueOnce({
      messages: initial,
      reached_end: false,
    });

    const { result } = renderHook(() => useChatHistory("ws-1", mockContainer()));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(10);

    // Each older-load prepends 20 messages with timestamps further in the
    // past. After enough loads the buffer should exceed 500 and the oldest
    // (front) ones must be discarded while the most recent messages stay.
    const baseTime = new Date("2026-01-01T00:00:00.000Z").getTime();
    let nextId = 1000;
    for (let load = 0; load < 30; load++) {
      const older = Array.from({ length: 20 }, (_, i) => {
        const id = nextId++;
        // Earlier loads are more recent; later loads are older history.
        const ts = new Date(baseTime - (load * 1000) - i).toISOString();
        return makeMsg(id, ts);
      });
      apiGet.mockResolvedValueOnce({
        messages: older,
        reached_end: false,
      });
      await act(async () => {
        await result.current.loadOlder();
      });
    }

    expect(result.current.messages.length).toBeLessThanOrEqual(500);
    // The most recent messages (the initial seed) must still be present.
    expect(result.current.messages.some((m) => m.id === "msg-1")).toBe(true);
    // Some of the oldest loaded history should have been evicted by the cap.
    expect(result.current.messages.some((m) => m.id === "msg-1580")).toBe(false);
  });

  it("caps the in-memory message buffer at MAX_MESSAGES on live append", async () => {
    // Seed a small initial history.
    const initial = Array.from({ length: 10 }, (_, i) => makeMsg(i + 1));
    apiGet.mockResolvedValueOnce({
      messages: initial,
      reached_end: true,
    });

    const { result } = renderHook(() => useChatHistory("ws-1"));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(10);

    // Append enough distinct messages to exceed MAX_MESSAGES.
    await act(async () => {
      for (let i = 11; i <= 520; i++) {
        result.current.appendMessageDeduped(makeMsg(i));
      }
    });

    expect(result.current.messages.length).toBeLessThanOrEqual(500);
    // The newest appended messages must survive the cap.
    expect(result.current.messages.some((m) => m.id === "msg-520")).toBe(true);
    expect(result.current.messages.some((m) => m.id === "msg-519")).toBe(true);
    // The oldest initial messages should have been evicted.
    expect(result.current.messages.some((m) => m.id === "msg-1")).toBe(false);
    expect(result.current.messages.some((m) => m.id === "msg-10")).toBe(false);
  });
});
