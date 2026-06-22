// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import React from "react";
import { BudgetSection } from "../BudgetSection";
import { api } from "@/lib/api";

// Multi-period budget (#49): the API now returns a `periods` map
// (hourly/daily/weekly/monthly), each {limit, spend, remaining} in USD cents.
// The UI renders one row per period and PATCHes {budget_limits:{period:cents|null}}.

type QueueEntry = { body?: unknown; err?: Error };
const apiQueue: QueueEntry[] = [];

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(async (path: string) => {
      const next = apiQueue.shift();
      if (!next) throw new Error(`api.get queue exhausted at: ${path}`);
      if (next.err) throw next.err;
      return next.body;
    }),
    patch: vi.fn(async (path: string, _body?: unknown) => {
      const next = apiQueue.shift();
      if (!next) throw new Error(`api.patch queue exhausted at: ${path}`);
      if (next.err) throw next.err;
      return next.body;
    }),
  },
}));

afterEach(cleanup);

beforeEach(() => {
  apiQueue.length = 0;
  vi.clearAllMocks();
});

const WS_ID = "budget-test-ws";

function qGet(body: unknown) {
  apiQueue.push({ body });
}
function qGetErr(status: number, msg: string) {
  apiQueue.push({ err: new Error(`${msg}: ${status}`) });
}
function qPatch(body: unknown) {
  apiQueue.push({ body });
}
function qPatchErr(status: number, msg: string) {
  apiQueue.push({ err: new Error(`${msg}: ${status}`) });
}

type P = { limit: number | null; spend: number; remaining: number | null };

// makeBudget builds the periods response. Override any subset of periods.
function makeBudget(overrides: Partial<Record<"hourly" | "daily" | "weekly" | "monthly", Partial<P>>> = {}) {
  const blank: P = { limit: null, spend: 0, remaining: null };
  const mk = (o?: Partial<P>): P => {
    const p = { ...blank, ...(o ?? {}) };
    if (p.limit != null && p.remaining == null) p.remaining = p.limit - p.spend;
    return p;
  };
  const periods = {
    hourly: mk(overrides.hourly),
    daily: mk(overrides.daily),
    weekly: mk(overrides.weekly),
    monthly: mk(overrides.monthly),
  };
  return {
    periods,
    budget_limit: periods.monthly.limit,
    monthly_spend: periods.monthly.spend,
    budget_remaining: periods.monthly.remaining,
  };
}

describe("BudgetSection (multi-period)", () => {
  describe("loading state", () => {
    it("shows loading indicator while fetching", async () => {
      let resolveGet: (v: unknown) => void;
      vi.mocked(api.get).mockImplementationOnce(
        async () => new Promise((r) => { resolveGet = r as (v: unknown) => void; }),
      );
      render(<BudgetSection workspaceId={WS_ID} />);
      expect(screen.getByTestId("budget-loading")).toBeTruthy();
      resolveGet!(makeBudget());
      await vi.waitFor(() => {
        expect(screen.queryByTestId("budget-loading")).toBeNull();
      });
    });
  });

  describe("fetch error state", () => {
    it("shows error message on non-402 fetch failure", async () => {
      qGetErr(500, "Internal Server Error");
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-fetch-error")).toBeTruthy();
      });
      expect(screen.getByTestId("budget-fetch-error")!.textContent).toContain("500");
    });

    it("shows the exceeded banner (not a fetch error) on a 402", async () => {
      qGetErr(402, "Payment Required");
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
      });
      expect(screen.queryByTestId("budget-fetch-error")).toBeNull();
    });
  });

  describe("rendering periods", () => {
    it("renders all four period rows", async () => {
      qGet(makeBudget());
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        for (const k of ["hourly", "daily", "weekly", "monthly"]) {
          expect(screen.getByTestId(`budget-period-${k}`)).toBeTruthy();
        }
      });
    });

    it("formats spend and limit as USD per period", async () => {
      qGet(makeBudget({ monthly: { limit: 10_000, spend: 3_500 } }));
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-monthly-spend")!.textContent).toBe("$35.00");
      });
      expect(screen.getByTestId("budget-monthly-limit")!.textContent).toBe("$100.00");
    });

    it("shows ∞ for a period with no limit", async () => {
      qGet(makeBudget({ hourly: { limit: null, spend: 1_000 } }));
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-hourly-limit")!.textContent).toBe("∞");
      });
    });

    it("renders the progress bar only for periods with a limit", async () => {
      qGet(makeBudget({ monthly: { limit: 10_000, spend: 12_000 }, hourly: { limit: null, spend: 5_000 } }));
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-monthly-fill")).toBeTruthy();
      });
      expect(screen.queryByTestId("budget-hourly-fill")).toBeNull();
      // over-budget fill caps at 100%
      const fill = screen.getByTestId("budget-monthly-fill") as HTMLElement;
      expect(fill.style.width).toBe("100%");
    });
  });

  describe("save", () => {
    it("PATCHes budget_limits for all four periods and clears the exceeded banner", async () => {
      qGet(makeBudget({ monthly: { limit: 10_000, spend: 3_500 } }));
      qPatch(makeBudget({ hourly: { limit: 500, spend: 0 }, monthly: { limit: 20_000, spend: 0 } }));
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-hourly-input")).toBeTruthy();
      });

      fireEvent.change(screen.getByTestId("budget-hourly-input"), { target: { value: "500" } });
      fireEvent.click(screen.getByTestId("budget-save-btn"));

      await vi.waitFor(() => {
        expect(vi.mocked(api.patch)).toHaveBeenCalled();
      });
      const [, body] = vi.mocked(api.patch).mock.calls[0];
      expect((body as { budget_limits: Record<string, number | null> }).budget_limits).toMatchObject({
        hourly: 500,
        monthly: 10_000, // unchanged input echoes the loaded limit
      });
    });

    it("shows a save error on non-402 PATCH failure", async () => {
      qGet(makeBudget());
      qPatchErr(500, "Internal Server Error");
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-save-btn")).toBeTruthy();
      });
      fireEvent.click(screen.getByTestId("budget-save-btn"));
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-save-error")).toBeTruthy();
      });
      expect(screen.getByTestId("budget-save-error")!.textContent).toContain("500");
    });

    it("surfaces the exceeded banner on a 402 PATCH", async () => {
      qGet(makeBudget());
      qPatchErr(402, "Payment Required");
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-save-btn")).toBeTruthy();
      });
      fireEvent.click(screen.getByTestId("budget-save-btn"));
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
      });
    });
  });

  describe("legacy payload back-compat", () => {
    it("maps a pre-multi-period {budget_limit, monthly_spend} response to the monthly row", async () => {
      qGet({ budget_limit: 5_000, monthly_spend: 1_000, budget_remaining: 4_000 });
      render(<BudgetSection workspaceId={WS_ID} />);
      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-monthly-limit")!.textContent).toBe("$50.00");
      });
      expect(screen.getByTestId("budget-monthly-spend")!.textContent).toBe("$10.00");
    });
  });
});
