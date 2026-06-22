import { describe, it, expect } from "vitest";
import { formatCredits, pillTone, bannerKind } from "@/lib/credits";

describe("formatCredits", () => {
  it("renders raw numbers under 10k", () => {
    expect(formatCredits(0)).toBe("0");
    expect(formatCredits(42)).toBe("42");
    expect(formatCredits(9999)).toBe("9999");
  });
  it("compacts 10k+ with one decimal", () => {
    expect(formatCredits(12345)).toBe("12.3k");
    expect(formatCredits(30000)).toBe("30.0k");
  });
});

describe("pillTone", () => {
  it("neutral surface for healthy balance", () => {
    expect(pillTone({ credits_balance: 5000, plan_monthly_credits: 9000 })).toContain("surface-card");
  });
  it("amber when under 10% of monthly", () => {
    expect(pillTone({ credits_balance: 500, plan_monthly_credits: 9000 })).toContain("amber");
  });
  it("red at zero or negative", () => {
    expect(pillTone({ credits_balance: 0, plan_monthly_credits: 9000 })).toContain("red");
    expect(pillTone({ credits_balance: -1, plan_monthly_credits: 9000 })).toContain("red");
  });
  it("trial (monthly=0) is healthy until balance hits zero", () => {
    // No paid plan → no ratio reference; only "0" means empty.
    // Healthy pill resolves to the warm-paper neutral surface (was zinc
    // pre-v4 migration); empty pill stays red because tinted state colours
    // are kept literal so the warning reads in both themes.
    expect(pillTone({ credits_balance: 50, plan_monthly_credits: 0 })).toContain("surface-card");
    expect(pillTone({ credits_balance: 0, plan_monthly_credits: 0 })).toContain("red");
  });
});

describe("bannerKind", () => {
  it("overage wins when overage_used > 0", () => {
    // Even a healthy balance gets "overage" so the banner reminds the
    // paying customer that extra charges are accruing.
    expect(bannerKind({ credits_balance: 3000, plan_monthly_credits: 9000, overage_used_credits: 500 }))
      .toBe("overage");
  });
  it("out-of-credits when balance <= 0 and no overage", () => {
    expect(bannerKind({ credits_balance: 0, plan_monthly_credits: 9000 })).toBe("out-of-credits");
  });
  it("trial-tail when plan is free and balance is low", () => {
    expect(bannerKind({ credits_balance: 50, plan_monthly_credits: 0 })).toBe("trial-tail");
  });
  it("none for healthy paid balance", () => {
    expect(bannerKind({ credits_balance: 8000, plan_monthly_credits: 9000 })).toBe("none");
  });
  it("none for a trial that still has plenty of credits", () => {
    expect(bannerKind({ credits_balance: 400, plan_monthly_credits: 0 })).toBe("none");
  });
});
