// @vitest-environment jsdom
//
// Pins the lazy-loading chat-history pagination.
//
// PR-C-2 (RFC #2945): canvas was migrated from /activity?type=a2a_receive
// to /chat-history. Server now returns typed ChatMessage[] in
// display-ready oldest-first order. These tests guard the canvas-side
// pagination invariants against the new endpoint surface.
//
// Pinned branches:
//   1. Initial fetch carries `limit=10` and NO before_ts (newest-first
//      slice). Pre-fix this was limit=50.
//   2. Server returning fewer than `limit` rows clears `hasMore` so the
//      top sentinel is removed and the IO observer disconnects — no
//      "Loading older messages…" spinner on a short conversation.
//   3. Server returning exactly `limit` rows on the first batch keeps
//      hasMore=true so the sentinel mounts (verified indirectly by
//      asserting the rendered bubble count matches the full page).
//   4. The retry button after a failed initial load uses the same
//      INITIAL_HISTORY_LIMIT (10), not the legacy 50.
//   5. before_ts cursor is the OLDEST timestamp from the current page,
//      passed verbatim to walk backward.
//   6. Inflight guard rejects duplicate IO triggers while a loadOlder
//      fetch is in flight.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// Both ChatTab sub-panels (MyChat + AgentComms) mount simultaneously so
// keyboard tab order and aria-controls land on a real DOM. MyChat's
// loadMessagesFromDB hits /chat-history; AgentComms's polling hits a
// different URL. Route the mock by URL so each gets a sensible default
// and only MyChat's calls land in the assertion array.
const myChatHistoryCalls: string[] = [];
let myChatNextResponse:
  | { ok: true; messages: unknown[]; reachedEnd?: boolean }
  | { ok: false; err: Error } = { ok: true, messages: [] };

const apiGet = vi.fn((path: string): Promise<unknown> => {
  if (path.includes("/chat-history")) {
    myChatHistoryCalls.push(path);
    if (myChatNextResponse.ok) {
      const reached_end =
        myChatNextResponse.reachedEnd !== undefined
          ? myChatNextResponse.reachedEnd
          : myChatNextResponse.messages.length < 10;
      return Promise.resolve({
        messages: myChatNextResponse.messages,
        reached_end,
      });
    }
    return Promise.reject(myChatNextResponse.err);
  }
  // AgentComms / heartbeat / anything else — empty array safe default.
  return Promise.resolve([]);
});
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

// Capture IntersectionObserver instances so tests can drive callbacks
// directly (jsdom has no layout, so nothing crosses thresholds on its
// own) AND assert observer-instance count to pin the perf invariant
// that live-message churn doesn't tear down + re-arm the observer.
type IOInstance = {
  callback: IntersectionObserverCallback;
  observed: Element[];
  disconnected: boolean;
};
const ioInstances: IOInstance[] = [];

beforeEach(() => {
  apiGet.mockClear();
  apiPost.mockReset();
  myChatHistoryCalls.length = 0;
  myChatNextResponse = { ok: true, messages: [] };
  ioInstances.length = 0;
  class FakeIO {
    private inst: IOInstance;
    constructor(cb: IntersectionObserverCallback) {
      this.inst = { callback: cb, observed: [], disconnected: false };
      ioInstances.push(this.inst);
    }
    observe(el: Element) {
      this.inst.observed.push(el);
    }
    unobserve() {}
    disconnect() {
      this.inst.disconnected = true;
    }
  }
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
  (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = FakeIO;
  Element.prototype.scrollIntoView = vi.fn();
});

function triggerIntersection(instanceIdx = -1) {
  const inst = ioInstances.at(instanceIdx);
  if (!inst) throw new Error(`no IO instance at ${instanceIdx}`);
  inst.callback(
    [{ isIntersecting: true, target: inst.observed[0] } as IntersectionObserverEntry],
    inst as unknown as IntersectionObserver,
  );
}

import { ChatTab } from "../ChatTab";

// makeMessagePair returns a (user, agent) pair sharing a timestamp,
// matching the wire shape /chat-history emits per activity_logs row.
// Server-side reverseRowChunks ensures the wire is oldest-first across
// rows but [user, agent] within each row.
function makeMessagePair(seq: number): unknown[] {
  // Zero-pad seq into the minute slot so seq=10 produces a valid
  // timestamp (00:10:00Z, not 00:010:00Z).
  const mm = String(seq).padStart(2, "0");
  const ts = `2026-05-05T00:${mm}:00Z`;
  return [
    { id: `u-${seq}`, role: "user", content: `user msg ${seq}`, timestamp: ts },
    { id: `a-${seq}`, role: "agent", content: `agent reply ${seq}`, timestamp: ts },
  ];
}

// pageOldestFirst builds a wire-shape page (oldest-first within page)
// of `count` row-pairs starting at seq=`start`. Mirrors the server's
// post-reverseRowChunks emission order.
function pageOldestFirst(start: number, count: number): unknown[] {
  const out: unknown[] = [];
  for (let i = 0; i < count; i++) {
    out.push(...makeMessagePair(start + i));
  }
  return out;
}

const minimalData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: null,
} as unknown as Parameters<typeof ChatTab>[0]["data"];

describe("ChatTab lazy history pagination", () => {
  it("initial fetch carries limit=10 (not the legacy 50) and hits /chat-history", async () => {
    myChatNextResponse = { ok: true, messages: makeMessagePair(1) };
    render(<ChatTab workspaceId="ws-1" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    const url = myChatHistoryCalls[0];
    expect(url).toContain("/chat-history");
    expect(url).toContain("limit=10");
    expect(url).not.toContain("limit=50");
    // before_ts should NOT be set on the initial fetch — that's the
    // newest-first slice the user lands on.
    expect(url).not.toContain("before_ts");
    // /chat-history filters source-canvas server-side; client should
    // NOT pass type/source params (they belonged to /activity).
    expect(url).not.toContain("type=a2a_receive");
    expect(url).not.toContain("source=canvas");
  });

  it("hides the top sentinel when initial fetch returns fewer than the limit", async () => {
    // 3 < 10 → server says "no more older history exists"; sentinel
    // should NOT mount and the "Loading older messages…" line should
    // never appear.
    myChatNextResponse = { ok: true, messages: pageOldestFirst(1, 3) };
    render(<ChatTab workspaceId="ws-2" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => {
      expect(screen.queryByText(/Loading chat history/i)).toBeNull();
    });
    expect(screen.queryByText(/Loading older messages/i)).toBeNull();
  });

  it("renders all messages when initial fetch returns exactly the limit", async () => {
    // limit=10 row-pairs → 20 ChatMessages. reachedEnd should be FALSE
    // so the sentinel mounts. Verified by bubble counts.
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(1, 10),
      reachedEnd: false,
    };
    render(<ChatTab workspaceId="ws-3" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => {
      expect(screen.queryByText(/Loading chat history/i)).toBeNull();
    });
    expect(screen.getAllByText(/user msg/).length).toBe(10);
    expect(screen.getAllByText(/agent reply/).length).toBe(10);
  });

  it("retry-after-failure uses limit=10, not the legacy 50", async () => {
    myChatNextResponse = { ok: false, err: new Error("network down") };
    render(<ChatTab workspaceId="ws-4" data={minimalData} />);
    const retry = await screen.findByText(/Retry/);
    myChatNextResponse = { ok: true, messages: makeMessagePair(1) };
    fireEvent.click(retry);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(2));
    const retryUrl = myChatHistoryCalls[1];
    expect(retryUrl).toContain("/chat-history");
    expect(retryUrl).toContain("limit=10");
    expect(retryUrl).not.toContain("limit=50");
  });

  it("loadOlder fetches limit=20 with before_ts=oldest.timestamp", async () => {
    // Initial page = 10 row-pairs in oldest-first order (seq 1..10).
    // The oldest (and so the cursor for loadOlder) is seq=1's
    // timestamp 2026-05-05T00:01:00Z.
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(1, 10),
      reachedEnd: false,
    };
    render(<ChatTab workspaceId="ws-load-older" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => expect(ioInstances.length).toBeGreaterThan(0));

    // Stage older-batch response, then fire IO callback.
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(0, 1),
      reachedEnd: true,
    };
    triggerIntersection();

    await waitFor(() => expect(myChatHistoryCalls.length).toBe(2));
    const olderUrl = myChatHistoryCalls[1];
    expect(olderUrl).toContain("/chat-history");
    expect(olderUrl).toContain("limit=20");
    expect(olderUrl).toContain("before_ts=");
    expect(decodeURIComponent(olderUrl)).toContain("before_ts=2026-05-05T00:01:00Z");
  });

  it("inflight guard rejects a second IO trigger while first loadOlder is in flight", async () => {
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(1, 10),
      reachedEnd: false,
    };
    render(<ChatTab workspaceId="ws-inflight" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => expect(ioInstances.length).toBeGreaterThan(0));

    // Hold the next loadOlder fetch open with a manual deferred so we
    // can fire the second trigger while the first is in-flight.
    let release!: (resp: unknown) => void;
    const deferred = new Promise<unknown>((res) => {
      release = res;
    });
    apiGet.mockImplementationOnce((path: string): Promise<unknown> => {
      myChatHistoryCalls.push(path);
      return deferred;
    });

    triggerIntersection(); // start loadOlder #1
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(2));

    // Second IO trigger lands while #1 is still pending.
    triggerIntersection();
    triggerIntersection();
    triggerIntersection();
    // Without the inflight guard, each of these would have started a
    // new fetch. With the guard, none of them do — call count stays 2.
    await new Promise((r) => setTimeout(r, 10));
    expect(myChatHistoryCalls.length).toBe(2);

    // Release the first fetch with a valid wire response shape.
    release({ messages: [], reached_end: true });
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(2));
  });

  it("empty older response clears the scroll anchor and unmounts the sentinel", async () => {
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(1, 10),
      reachedEnd: false,
    };
    render(<ChatTab workspaceId="ws-anchor" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => expect(ioInstances.length).toBeGreaterThan(0));

    myChatNextResponse = {
      ok: true,
      messages: [],
      reachedEnd: true,
    };
    triggerIntersection();
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(2));

    await waitFor(() => {
      expect(screen.queryByText(/Loading older messages/i)).toBeNull();
    });
  });

  it("IntersectionObserver does not churn when older messages prepend", async () => {
    myChatNextResponse = {
      ok: true,
      messages: pageOldestFirst(1, 10),
      reachedEnd: false,
    };
    render(<ChatTab workspaceId="ws-stable-io" data={minimalData} />);
    await waitFor(() => expect(myChatHistoryCalls.length).toBe(1));
    await waitFor(() => expect(ioInstances.length).toBeGreaterThan(0));

    const observerBefore = ioInstances.at(-1);
    expect(observerBefore).toBeDefined();
    expect(observerBefore!.disconnected).toBe(false);

    // Trigger three older-batch prepends. Each batch returns the full
    // OLDER_HISTORY_BATCH (20 row-pairs = 40 messages) so reachedEnd
    // stays false and the sentinel keeps mounting.
    for (let batch = 0; batch < 3; batch++) {
      myChatNextResponse = {
        ok: true,
        messages: pageOldestFirst(-(batch + 1) * 20, 20),
        reachedEnd: false,
      };
      const callsBefore = myChatHistoryCalls.length;
      triggerIntersection();
      await waitFor(() => expect(myChatHistoryCalls.length).toBe(callsBefore + 1));
    }

    // The original observer is still the live one — no churn.
    expect(observerBefore!.disconnected).toBe(false);
    expect(ioInstances.at(-1)).toBe(observerBefore);
  });
});
