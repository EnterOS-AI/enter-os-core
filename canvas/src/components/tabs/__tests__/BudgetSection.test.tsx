// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import React from "react";
import { BudgetSection } from "../BudgetSection";
import { api } from "@/lib/api";

// Queue-based mock for the api module. Each api call shifts from the queue.
// Tests push with qGet/qPatch and the module-level mockImplementation
// reads from the queue.
type QueueEntry = { body?: unknown; err?: Error };
const apiQueue: QueueEntry[] = [];

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(async (_path: string) => {
      const next = apiQueue.shift();
      if (!next) throw new Error("api.get queue exhausted");
      if (next.err) throw next.err;
      return next.body;
    }),
    patch: vi.fn(async (_path: string, _body?: unknown) => {
      const next = apiQueue.shift();
      if (!next) throw new Error("api.patch queue exhausted");
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

function makeBudget(overrides: Partial<{
  budget_limit: number | null;
  budget_used: number;
  budget_remaining: number | null;
}> = {}) {
  return {
    budget_limit: 10_000,
    budget_used: 3_500,
    budget_remaining: 6_500,
    ...overrides,
  };
}

describe("BudgetSection", () => {
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

    it("shows 402 as exceeded banner, not fetch error", async () => {
      qGetErr(402, "Payment Required");

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
      });
      expect(screen.queryByTestId("budget-fetch-error")).toBeNull();
    });
  });

  describe("budget loaded — display", () => {
    it("renders used / limit stats row", async () => {
      qGet(makeBudget({ budget_limit: 10_000, budget_used: 3_500 }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-used-value")!.textContent).toBe("3,500");
      });
      expect(screen.getByTestId("budget-limit-value")!.textContent).toBe("10,000");
    });

    it("renders 'Unlimited' when budget_limit is null", async () => {
      qGet(makeBudget({ budget_limit: null, budget_used: 1_000, budget_remaining: null }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-limit-value")!.textContent).toBe("Unlimited");
      });
    });

    it("renders remaining credits when present", async () => {
      qGet(makeBudget({ budget_limit: 10_000, budget_used: 3_500, budget_remaining: 6_500 }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-remaining")!.textContent).toContain("6,500");
        expect(screen.getByTestId("budget-remaining")!.textContent).toContain("credits remaining");
      });
    });

    it("omits remaining credits when budget_remaining is null", async () => {
      qGet(makeBudget({ budget_limit: 10_000, budget_used: 3_500, budget_remaining: null }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.queryByTestId("budget-remaining")).toBeNull();
      });
    });

    it("caps progress bar at 100% when used > limit", async () => {
      qGet(makeBudget({ budget_limit: 10_000, budget_used: 12_000, budget_remaining: null }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        const fill = screen.getByTestId("budget-progress-fill");
        expect(fill.getAttribute("style")).toContain("100%");
      });
    });

    it("omits progress bar when budget_limit is null (unlimited)", async () => {
      qGet(makeBudget({ budget_limit: null, budget_used: 5_000, budget_remaining: null }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.queryByTestId("budget-progress-fill")).toBeNull();
      });
    });
  });

  describe("budget exceeded (402)", () => {
    it("shows exceeded banner when load returns 402", async () => {
      qGetErr(402, "Payment Required");

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
        expect(screen.getByTestId("budget-exceeded-banner")!.textContent).toContain("Budget exceeded");
      });
    });

    it("clears exceeded banner after successful save", async () => {
      qGetErr(402, "Payment Required");
      qPatch(makeBudget({ budget_limit: 50_000, budget_used: 0, budget_remaining: 50_000 }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
      });

      const input = screen.getByTestId("budget-limit-input");
      fireEvent.change(input, { target: { value: "50000" } });

      const saveBtn = screen.getByTestId("budget-save-btn");
      fireEvent.click(saveBtn);

      await vi.waitFor(() => {
        expect(screen.queryByTestId("budget-exceeded-banner")).toBeNull();
      });
    });
  });

  describe("save flow", () => {
    it("shows save error on non-402 patch failure", async () => {
      qGet(makeBudget());
      qPatchErr(500, "Internal Server Error");

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-limit-input")).toBeTruthy();
      });

      const saveBtn = screen.getByTestId("budget-save-btn");
      fireEvent.click(saveBtn);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-save-error")).toBeTruthy();
        expect(screen.getByTestId("budget-save-error")!.textContent).toContain("500");
      });
    });

    it("updates input to new limit value after successful save", async () => {
      qGet(makeBudget({ budget_limit: 10_000 }));
      qPatch(makeBudget({ budget_limit: 20_000 }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.queryByTestId("budget-loading")).toBeNull();
      });

      const input = screen.getByTestId("budget-limit-input") as HTMLInputElement;
      expect(input.value).toBe("10000");
      expect(screen.getByTestId("budget-limit-value")!.textContent).toBe("10,000");

      fireEvent.change(input, { target: { value: "20000" } });
      expect(input.value).toBe("20000");

      fireEvent.click(screen.getByTestId("budget-save-btn"));

      await vi.waitFor(() => {
        expect((screen.getByTestId("budget-limit-input") as HTMLInputElement).value).toBe("20000");
      });
    });

    it("sends null when input is cleared (unlimited)", async () => {
      qGet(makeBudget({ budget_limit: 10_000 }));
      qPatch(makeBudget({ budget_limit: null }));

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-limit-input")).toBeTruthy();
      });

      const input = screen.getByTestId("budget-limit-input") as HTMLInputElement;
      fireEvent.change(input, { target: { value: "" } });
      fireEvent.click(screen.getByTestId("budget-save-btn"));

      await vi.waitFor(() => {
        expect(input.value).toBe("");
      });
    });

    it("shows saving state on button while patch is in flight", async () => {
      qGet(makeBudget());
      let resolvePatch: (v: unknown) => void;
      vi.mocked(api.patch).mockImplementationOnce(
        async () => new Promise((r) => { resolvePatch = r as (v: unknown) => void; }),
      );

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-limit-input")).toBeTruthy();
      });

      fireEvent.change(screen.getByTestId("budget-limit-input"), { target: { value: "50000" } });
      fireEvent.click(screen.getByTestId("budget-save-btn"));

      const btn = screen.getByTestId("budget-save-btn");
      expect(btn.textContent).toContain("Saving");

      resolvePatch!(makeBudget({ budget_limit: 50_000 }));
      await vi.waitFor(() => {
        expect(btn.textContent).toContain("Save");
      });
    });
  });

  describe("isApiError402 — regression coverage", () => {
    it("classifies ': 402' with space as 402", async () => {
      qGetErr(402, "Payment Required");
      qPatch(makeBudget());

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-exceeded-banner")).toBeTruthy();
      });
    });

    it("classifies non-402 error messages as regular fetch errors", async () => {
      qGetErr(503, "Service Unavailable");

      render(<BudgetSection workspaceId={WS_ID} />);

      await vi.waitFor(() => {
        expect(screen.getByTestId("budget-fetch-error")).toBeTruthy();
      });
      expect(screen.queryByTestId("budget-exceeded-banner")).toBeNull();
    });
  });
});
