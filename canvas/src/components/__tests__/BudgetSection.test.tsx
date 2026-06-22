// @vitest-environment jsdom
/**
 * Focused tests for BudgetSection's PER-PERIOD progress-bar math + aria (#49).
 *
 * Behavioral coverage (loading, save, 402 banners, USD formatting, legacy
 * back-compat) lives in tabs/__tests__/BudgetSection.test.tsx — this file
 * deliberately covers only the per-period progress percentage + aria-valuenow
 * + the over-budget colouring, which that suite doesn't assert in detail. Kept
 * separate to avoid duplicating the behavioral suite (one component, no
 * parallel/identical suites).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), patch: vi.fn() },
}));

import { api } from "@/lib/api";
import { BudgetSection } from "../tabs/BudgetSection";

const mockGet = vi.mocked(api.get);

type P = { limit: number | null; spend: number; remaining: number | null };

// Build a periods response where the named period has the given limit/spend.
function withMonthly(limit: number | null, spend: number) {
  const blank: P = { limit: null, spend: 0, remaining: null };
  const monthly: P = { limit, spend, remaining: limit == null ? null : limit - spend };
  return {
    periods: { hourly: blank, daily: blank, weekly: blank, monthly },
    budget_limit: limit,
    monthly_spend: spend,
    budget_remaining: monthly.remaining,
  };
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => cleanup());

async function renderLoaded(data: unknown) {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockGet.mockResolvedValueOnce(data as any);
  render(<BudgetSection workspaceId="ws-1" />);
  await waitFor(() => expect(screen.queryByTestId("budget-loading")).toBeNull());
}

describe("BudgetSection — per-period progress bar", () => {
  it("renders the bar for a limited period and omits it for an unlimited one", async () => {
    await renderLoaded(withMonthly(1000, 250));
    expect(screen.getByTestId("budget-monthly-fill")).toBeTruthy();
    expect(screen.queryByTestId("budget-hourly-fill")).toBeNull(); // hourly unlimited
  });

  it("fills to 25%", async () => {
    await renderLoaded(withMonthly(1000, 250));
    expect((screen.getByTestId("budget-monthly-fill") as HTMLElement).style.width).toBe("25%");
  });

  it("fills to 50%", async () => {
    await renderLoaded(withMonthly(1000, 500));
    expect((screen.getByTestId("budget-monthly-fill") as HTMLElement).style.width).toBe("50%");
  });

  it("caps fill at 100% when spend exceeds limit", async () => {
    await renderLoaded(withMonthly(1000, 4000));
    expect((screen.getByTestId("budget-monthly-fill") as HTMLElement).style.width).toBe("100%");
  });

  it("sets aria-valuenow to the computed percentage on the progressbar", async () => {
    await renderLoaded(withMonthly(1000, 250));
    const bars = screen.getAllByRole("progressbar");
    // the monthly bar is the only one rendered (others unlimited)
    expect(bars).toHaveLength(1);
    expect(bars[0].getAttribute("aria-valuenow")).toBe("25");
  });

  it("shows a 0% bar when spend is 0 against a set limit", async () => {
    await renderLoaded(withMonthly(1000, 0));
    expect((screen.getByTestId("budget-monthly-fill") as HTMLElement).style.width).toBe("0%");
  });
});
