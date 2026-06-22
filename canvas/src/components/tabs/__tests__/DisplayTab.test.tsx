// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const { mockGet, mockPost, mockRFBConstructor, mockRFBClipboardPasteFrom, mockRFBFocus, rfbInstances } = vi.hoisted(() => ({
  mockGet: vi.fn(),
  mockPost: vi.fn(),
  mockRFBConstructor: vi.fn(),
  mockRFBClipboardPasteFrom: vi.fn(),
  mockRFBFocus: vi.fn(),
  rfbInstances: [] as EventTarget[],
}));

vi.mock("@/lib/api", () => ({
  api: {
    get: mockGet,
    post: mockPost,
  },
}));

vi.mock("@novnc/novnc", () => ({
  default: class MockRFB extends EventTarget {
    scaleViewport = false;
    resizeSession = false;
    focusOnClick = false;
    target: HTMLElement;
    url: string;
    options?: { wsProtocols?: string[] };
    constructor(target: HTMLElement, url: string, options?: { wsProtocols?: string[] }) {
      super();
      this.target = target;
      this.url = url;
      this.options = options;
      mockRFBConstructor(target, url, options);
      rfbInstances.push(this);
    }
    clipboardPasteFrom(text: string) {
      mockRFBClipboardPasteFrom(text);
    }
    focus(options?: FocusOptions) {
      mockRFBFocus(options);
    }
    disconnect() {}
  },
}));

import { DisplayTab } from "../DisplayTab";

describe("DisplayTab", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockRFBConstructor.mockReset();
    mockRFBClipboardPasteFrom.mockReset();
    mockRFBFocus.mockReset();
    rfbInstances.length = 0;
  });

  it("renders unavailable state for non-display workspaces", async () => {
    mockGet.mockResolvedValueOnce({
      available: false,
      reason: "display_not_enabled",
    });

    render(<DisplayTab workspaceId="ws-no-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display is not enabled for this workspace.")).toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-no-display/display");
    expect(mockGet).not.toHaveBeenCalledWith("/workspaces/ws-no-display/display/control");
  });

  it("renders control acquisition for display-configured workspaces", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockResolvedValueOnce({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display/control");

    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    await waitFor(() => {
      expect(screen.getByText("Controlled by Admin")).toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/acquire", {
      controller: "user",
      ttl_seconds: 300,
    });
  });

  it("waits for takeover before opening a ready display stream", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      })
      .mockResolvedValueOnce({
        controller: "none",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Take control to open the desktop.")).toBeTruthy();
    });
    expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
  });

  it("opens the trusted noVNC client after takeover returns a stream URL", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockResolvedValueOnce({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
      session_url: "/workspaces/ws-display/display/session/websockify#token=signed",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    await waitFor(() => {
      expect(screen.getByTitle("Workspace desktop")).toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/acquire", {
      controller: "user",
      ttl_seconds: 300,
    });
    // Defensive: the noVNC constructor is async (dynamic import), so wait
    // for it to be called before asserting arguments (prevents flake in CI).
    await waitFor(() => {
      expect(mockRFBConstructor).toHaveBeenCalled();
    });
    expect(mockRFBConstructor).toHaveBeenCalledWith(
      expect.any(HTMLElement),
      expect.stringContaining("/workspaces/ws-display/display/session/websockify"),
      { wsProtocols: ["binary", "molecule-display-token.signed"] },
    );
    expect(mockRFBConstructor.mock.calls[0][1]).not.toContain("token=");
  });

  it("forwards browser paste events into the noVNC clipboard", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockResolvedValueOnce({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
      session_url: "/workspaces/ws-display/display/session/websockify#token=signed",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    const desktop = await screen.findByTitle("Workspace desktop");
    // Wait for the RFB instance to actually connect before pasting. The component
    // sets rfbRef.current synchronously right after `new RFB()` (which fires
    // mockRFBConstructor) INSIDE the async connect() — but the "Workspace desktop"
    // title renders before that await resolves. Firing paste immediately races
    // rfbRef.current===null, so the window paste handler's
    // `rfbRef.current?.clipboardPasteFrom(text)` no-ops (0 calls). This lost the
    // race under CI runner load; waiting for the constructor makes it deterministic.
    await waitFor(() => expect(mockRFBConstructor).toHaveBeenCalled());
    fireEvent.paste(desktop, {
      clipboardData: {
        getData: (type: string) => (type === "text/plain" ? "Paste Me" : ""),
      },
    });

    expect(mockRFBClipboardPasteFrom).toHaveBeenCalledWith("Paste Me");
    expect(mockRFBFocus).toHaveBeenCalledWith({ preventScroll: true });
  });

  it("releases user display control", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "novnc",
      })
      .mockResolvedValueOnce({
        controller: "user",
        controlled_by: "admin-token",
        expires_at: "2026-05-23T08:48:27Z",
      });
    mockPost.mockResolvedValueOnce({
      controller: "none",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Release" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "Release" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/release", {});
  });

  it("renders active display control locks as observe-only", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "agent",
        controlled_by: "sidecar",
        expires_at: "2026-05-23T08:48:27Z",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Controlled by Agent")).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Release" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("labels org-token display control locks as automation", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "user",
        controlled_by: "org-token:abc123",
        expires_at: "2026-05-23T08:48:27Z",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Controlled by Automation")).toBeTruthy();
    });
    expect(screen.queryByText("org-token:abc123")).toBeNull();
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
  });

  it("refreshes display control state after failed acquisition", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      })
      .mockResolvedValueOnce({
        controller: "agent",
        controlled_by: "sidecar",
        expires_at: "2026-05-23T08:48:27Z",
      });
    mockPost.mockRejectedValueOnce(new Error("API POST /workspaces/ws-display/display/control/acquire: 409 conflict"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    await waitFor(() => {
      expect(screen.getByText("Controlled by Agent")).toBeTruthy();
    });
    expect(screen.getByText("Failed to take control")).toBeTruthy();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display/control");
    expect(mockGet).toHaveBeenCalledTimes(3);
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/acquire", {
      controller: "user",
      ttl_seconds: 300,
    });
  });

  it("keeps display status visible without takeover actions when control status fails", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockRejectedValueOnce(new Error("API GET /workspaces/ws-display/display/control: 401 unauthorized"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display session is not ready.")).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
    expect(screen.getByText("Display control unavailable")).toBeTruthy();
  });

  it("does not render raw display status errors", async () => {
    mockGet.mockRejectedValueOnce(new Error("API GET /workspaces/ws-display/display: 500 secret backend details"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display status unavailable")).toBeTruthy();
    });
    expect(screen.queryByText(/secret backend details/)).toBeNull();
  });

  it("ignores stale acquire responses after workspace changes", async () => {
    const acquire = deferred<{ controller: "user"; controlled_by: string; expires_at: string }>();
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      })
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockReturnValueOnce(acquire.promise);

    const { rerender } = render(<DisplayTab workspaceId="ws-a" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    rerender(<DisplayTab workspaceId="ws-b" />);

    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-b/display/control");
    });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });

    acquire.resolve({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
    });
    await acquire.promise;

    await waitFor(() => {
      expect(screen.queryByText("Controlled by Admin")).toBeNull();
    });
    expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
  });

  it("auto-reconnects the desktop stream after an unclean disconnect but not a clean one", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      })
      .mockResolvedValueOnce({ controller: "none" });
    // Initial acquire returns token "signed"; the reconnect re-acquire mints a
    // FRESH token "signed2" (the lock/token is only ~300s — reconnecting with a
    // cached, possibly-expired token would 401 and never recover).
    mockPost
      .mockResolvedValueOnce({
        controller: "user",
        controlled_by: "admin-token",
        expires_at: "2026-05-23T08:48:27Z",
        session_url: "/workspaces/ws-display/display/session/websockify#token=signed",
      })
      .mockResolvedValue({
        controller: "user",
        controlled_by: "admin-token",
        expires_at: "2026-05-23T08:53:27Z",
        session_url: "/workspaces/ws-display/display/session/websockify#token=signed2",
      });

    render(<DisplayTab workspaceId="ws-display" />);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: "Take control" }));
    await waitFor(() => {
      expect(rfbInstances.length).toBe(1);
    });
    expect(mockRFBConstructor.mock.calls[0][2].wsProtocols).toContain("molecule-display-token.signed");

    // An idle/network drop closes the websocket uncleanly. The client must
    // re-acquire a fresh token and reconnect instead of giving up — this is the
    // "disconnects every ~5 min and stays dead" report.
    rfbInstances[0].dispatchEvent(new CustomEvent("disconnect", { detail: { clean: false } }));
    await waitFor(
      () => {
        expect(rfbInstances.length).toBe(2);
      },
      { timeout: 3000 },
    );
    // Reconnect dialed with the FRESH token, not the stale original.
    expect(mockRFBConstructor.mock.calls[1][2].wsProtocols).toContain("molecule-display-token.signed2");

    // A clean disconnect (the user released control) must NOT reconnect.
    rfbInstances[1].dispatchEvent(new CustomEvent("disconnect", { detail: { clean: true } }));
    await new Promise((resolve) => setTimeout(resolve, 1100));
    expect(rfbInstances.length).toBe(2);
  });
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}
