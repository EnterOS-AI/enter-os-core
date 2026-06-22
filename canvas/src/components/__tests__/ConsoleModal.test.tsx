// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

import { api } from "@/lib/api";
import { ConsoleModal } from "../ConsoleModal";

const mockGet = vi.mocked(api.get);

beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

describe("ConsoleModal", () => {
  it("returns null when closed — no fetch triggered", () => {
    const { container } = render(
      <ConsoleModal workspaceId="ws-1" open={false} onClose={() => {}} />,
    );
    expect(container.firstChild).toBeNull();
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("fetches console output when opened", async () => {
    mockGet.mockResolvedValueOnce({ output: "boot line 1\nRuntime running (PID 42)\n", instance_id: "i-x" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    await waitFor(() =>
      expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/console"),
    );
    await waitFor(() => {
      const out = screen.getByTestId("console-output");
      expect(out.textContent).toContain("Runtime running (PID 42)");
    });
  });

  it("renders a friendly message on 501 (non-CP deploy)", async () => {
    mockGet.mockRejectedValueOnce(new Error("GET /workspaces/ws-1/console: 501 Not Implemented"));
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    await waitFor(() => {
      const err = screen.getByTestId("console-error");
      expect(err.textContent).toMatch(/only available on cloud/i);
    });
  });

  it("renders a specific message on 404 (instance terminated)", async () => {
    mockGet.mockRejectedValueOnce(new Error("GET /workspaces/ws-1/console: 404 Not Found"));
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    await waitFor(() => {
      const err = screen.getByTestId("console-error");
      expect(err.textContent).toMatch(/No EC2 instance found/i);
    });
  });

  it("Close button invokes onClose", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    const onClose = vi.fn();
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={onClose} />);
    await waitFor(() => screen.getByText("Close"));
    fireEvent.click(screen.getByText("Close"));
    expect(onClose).toHaveBeenCalled();
  });

  it("Escape key invokes onClose", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    const onClose = vi.fn();
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={onClose} />);
    await waitFor(() => screen.getByText("Close"));
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });
});

// ── WCAG 2.1 dialog accessibility ─────────────────────────────────────────────

describe("ConsoleModal — WCAG 2.1 dialog accessibility", () => {
  it("renders role=dialog when open", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeTruthy());
  });

  it("dialog has aria-modal='true' (WCAG 2.1 SC 1.3.2)", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("dialog has aria-labelledby pointing to the title", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    const labelledBy = dialog.getAttribute("aria-labelledby");
    expect(labelledBy).toBeTruthy();
    const titleEl = document.getElementById(labelledBy!);
    expect(titleEl?.textContent?.trim()).toBe("EC2 console output");
  });

  it("backdrop div has aria-label for screen readers (WCAG 2.4.6)", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    const backdrop = document.querySelector('[aria-label="Close terminal"]');
    expect(backdrop).toBeTruthy();
    expect(backdrop?.className).toContain("bg-black");
  });

  it("error div has role=alert (WCAG 4.1.3)", async () => {
    mockGet.mockRejectedValueOnce(new Error("GET /workspaces/ws-1/console: 404 Not Found"));
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    const alert = await waitFor(() => screen.getByRole("alert"));
    expect(alert).toBeTruthy();
    expect(alert.textContent).toMatch(/No EC2 instance found/i);
  });

  it("Close button has accessible name via aria-label", async () => {
    mockGet.mockResolvedValueOnce({ output: "" });
    render(<ConsoleModal workspaceId="ws-1" open={true} onClose={() => {}} />);
    // Two close buttons: X icon (aria-label="Close") and text "Close" button
    const closeBtns = await waitFor(() => screen.getAllByRole("button", { name: /close/i }));
    expect(closeBtns.length).toBeGreaterThanOrEqual(1);
  });
});
