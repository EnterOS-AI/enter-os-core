// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";

// API mock — tests can override per case via apiGetMock.mockImplementationOnce.
const apiGetMock = vi.fn<(url: string) => Promise<unknown>>();
vi.mock("@/lib/api", () => ({
  api: {
    get: (url: string) => apiGetMock(url),
  },
}));

// useSocketEvent — no-op for these render tests; live updates aren't
// what we're verifying here.
vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: () => {},
}));

// Canvas store — peer name resolution.
vi.mock("@/store/canvas", () => ({
  useCanvasStore: {
    getState: () => ({
      nodes: [
        { id: "ws-self", data: { name: "Self" } },
        { id: "ws-peer", data: { name: "Peer Agent" } },
      ],
    }),
  },
}));

// Toaster shim — AgentCommsPanel imports showToast.
vi.mock("../../Toaster", () => ({
  showToast: vi.fn(),
}));

import { AgentCommsPanel } from "../AgentCommsPanel";

// jsdom doesn't implement scrollIntoView. Tests that observe the call
// install a spy here; tests that don't care still need a no-op stub
// so the component doesn't throw.
const scrollSpy = vi.fn<(opts?: ScrollIntoViewOptions | boolean) => void>();
beforeEach(() => {
  apiGetMock.mockReset();
  scrollSpy.mockReset();
  Element.prototype.scrollIntoView = scrollSpy as unknown as Element["scrollIntoView"];
});

afterEach(() => {
  vi.clearAllMocks();
});

describe("AgentCommsPanel — initial-state parity with ChatTab my-chat", () => {
  it("shows loading text while history fetch is in flight", () => {
    apiGetMock.mockReturnValueOnce(new Promise(() => { /* never resolves */ }));
    render(<AgentCommsPanel workspaceId="ws-self" />);
    expect(screen.getByText("Loading agent communications...")).toBeDefined();
  });

  it("renders error UI with a Retry button when the history fetch rejects", async () => {
    apiGetMock.mockRejectedValueOnce(new Error("network down"));
    render(<AgentCommsPanel workspaceId="ws-self" />);

    // Wait for the error state to render — loading→error transition is async.
    const alert = await waitFor(() => screen.getByRole("alert"));
    expect(alert.textContent).toMatch(/Failed to load agent communications/);
    expect(alert.textContent).toMatch(/network down/);

    // Retry button must be present and trigger a refetch.
    const retry = screen.getByRole("button", { name: "Retry" });
    apiGetMock.mockResolvedValueOnce([]); // success on retry
    fireEvent.click(retry);

    // Two calls total: initial load + retry. Pin via mock call count.
    await waitFor(() => expect(apiGetMock.mock.calls.length).toBe(2));
  });

  it("falls back to empty-state copy when load succeeds with zero rows", async () => {
    apiGetMock.mockResolvedValueOnce([]);
    render(<AgentCommsPanel workspaceId="ws-self" />);
    await waitFor(() =>
      expect(screen.getByText("No agent-to-agent communications yet.")).toBeDefined(),
    );
  });

  it("scrollIntoView is called with behavior=instant on the first message arrival", async () => {
    apiGetMock.mockResolvedValueOnce([
      {
        id: "act-1",
        activity_type: "a2a_send",
        source_id: "ws-self",
        target_id: "ws-peer",
        method: "message/send",
        summary: "Delegating",
        request_body: { message: { parts: [{ text: "hi" }] } },
        response_body: null,
        status: "ok",
        created_at: "2026-04-25T18:00:00Z",
      },
    ]);
    render(<AgentCommsPanel workspaceId="ws-self" />);

    // useLayoutEffect is what makes the first call instant — wait for
    // the panel to render at least one message.
    await waitFor(() => expect(scrollSpy.mock.calls.length).toBeGreaterThan(0));

    // The pinned contract: SOME call uses behavior: "instant" — the
    // first-arrival case. Subsequent appends use "smooth", but those
    // can't fire here (no live update yet).
    const sawInstant = scrollSpy.mock.calls.some((args) => {
      const opts = args[0];
      return typeof opts === "object" && opts !== null && "behavior" in opts && opts.behavior === "instant";
    });
    expect(sawInstant).toBe(true);
  });
});
