// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";

// Mock api before importing the component
vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
  },
}));

import { api } from "@/lib/api";
import { WorkspaceUsage } from "../WorkspaceUsage";

const mockGet = vi.mocked(api.get);

const METRICS_RESPONSE = {
  input_tokens: 12345,
  output_tokens: 678,
  total_calls: 42,
  estimated_cost_usd: "0.123456",
  period_start: "2026-04-17T00:00:00Z",
  period_end: "2026-04-18T00:00:00Z",
};

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

describe("WorkspaceUsage", () => {
  it("renders the outer container without crashing", () => {
    // Keep fetch pending so we can check initial state
    mockGet.mockReturnValue(new Promise(() => {}));
    const { container } = render(<WorkspaceUsage workspaceId="ws-1" />);
    expect(container.firstChild).toBeTruthy();
  });

  it("renders the Usage heading", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<WorkspaceUsage workspaceId="ws-1" />);
    expect(screen.getByText("Usage")).toBeTruthy();
  });

  it("shows skeleton rows while loading", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<WorkspaceUsage workspaceId="ws-1" />);
    const skeletons = screen.getAllByTestId("usage-skeleton-row");
    expect(skeletons.length).toBe(3);
  });

  it("calls GET /workspaces/:id/metrics with the correct workspaceId", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    render(<WorkspaceUsage workspaceId="ws-abc-123" />);
    await waitFor(() => expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-abc-123/metrics"));
  });

  it("displays input tokens formatted with toLocaleString after load", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      const row = screen.getByTestId("usage-input-tokens");
      expect(row).toBeTruthy();
      // 12345 formatted — locale-dependent but always has digits + "tokens"
      expect(row.textContent).toContain("tokens");
      expect(row.textContent).toContain("12");
    });
  });

  it("displays output tokens formatted with toLocaleString after load", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      const row = screen.getByTestId("usage-output-tokens");
      expect(row).toBeTruthy();
      expect(row.textContent).toContain("tokens");
      expect(row.textContent).toContain("678");
    });
  });

  it("does not crash when token/cost fields are missing", async () => {
    // Regression: Details tab crashed with
    // "Cannot read properties of undefined (reading 'toLocaleString')"
    // when a workspace stuck in provisioning returned partial metrics.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue({} as any);
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByTestId("usage-input-tokens").textContent).toContain("0");
      expect(screen.getByTestId("usage-output-tokens").textContent).toContain("0");
      expect(screen.getByTestId("usage-estimated-cost").textContent).toBe("Estimated cost$0.000000");
    });
  });

  it("displays estimated cost formatted as $X.XXXXXX after load", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      const row = screen.getByTestId("usage-estimated-cost");
      expect(row).toBeTruthy();
      expect(row.textContent).toBe("Estimated cost$0.123456");
    });
  });

  it("shows the stat rows and hides skeletons after successful load", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.queryAllByTestId("usage-skeleton-row").length).toBe(0);
      expect(screen.getByTestId("usage-input-tokens")).toBeTruthy();
      expect(screen.getByTestId("usage-output-tokens")).toBeTruthy();
      expect(screen.getByTestId("usage-estimated-cost")).toBeTruthy();
    });
  });

  it("shows error message when fetch fails", async () => {
    mockGet.mockRejectedValue(new Error("API GET /workspaces/ws-1/metrics: 403 Forbidden"));
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      const err = screen.getByTestId("usage-error");
      expect(err).toBeTruthy();
      expect(err.textContent).toContain("403");
    });
  });

  it("does not show stat rows on error", async () => {
    mockGet.mockRejectedValue(new Error("network error"));
    render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.queryByTestId("usage-input-tokens")).toBeNull();
      expect(screen.queryByTestId("usage-output-tokens")).toBeNull();
      expect(screen.queryByTestId("usage-estimated-cost")).toBeNull();
    });
  });

  it("re-fetches when workspaceId prop changes", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(METRICS_RESPONSE as any);
    const { rerender } = render(<WorkspaceUsage workspaceId="ws-1" />);
    await waitFor(() => expect(mockGet).toHaveBeenCalledTimes(1));

    rerender(<WorkspaceUsage workspaceId="ws-2" />);
    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledTimes(2);
      expect(mockGet).toHaveBeenLastCalledWith("/workspaces/ws-2/metrics");
    });
  });

  it("renders the usage-stats container in all states", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<WorkspaceUsage workspaceId="ws-1" />);
    expect(screen.getByTestId("usage-stats")).toBeTruthy();
  });
});
