// credits.ts — small pure helpers for rendering credit state on /orgs.
// Kept out of page.tsx so unit tests can exercise the formatting +
// banner-kind logic in node (no jsdom) without needing to mount React.

export type CreditsBannerKind =
  | "none"
  | "overage"        // paid plan has started burning overage this period
  | "out-of-credits" // balance 0, not on a paid plan (trial ran out)
  | "trial-tail";    // balance low but not zero, no paid plan yet

export interface CreditsFields {
  credits_balance?: number;
  plan_monthly_credits?: number;
  overage_used_credits?: number;
}

// formatCredits renders an int as a compact string. 9999 → "9999",
// 12345 → "12.3k". Keeps the balance pill narrow enough to fit on one
// line next to the org slug even for the Scale plan's 30k grant.
export function formatCredits(n: number): string {
  if (n < 10_000) return String(n);
  return `${(n / 1000).toFixed(1)}k`;
}

// pillTone returns the tailwind classnames that color the balance pill.
// Empty / exhausted → red; within 10% of zero → amber; else zinc. The
// 10% threshold matches the banner trigger — one consistent "low"
// signal so the pill and banner agree.
export function pillTone(fields: CreditsFields): string {
  const balance = fields.credits_balance ?? 0;
  const monthly = fields.plan_monthly_credits ?? 0;
  if (balance <= 0) return "bg-red-950 text-red-200 border-red-800";
  const ratio = monthly > 0 ? balance / monthly : 1;
  if (ratio < 0.1) return "bg-amber-950 text-amber-200 border-amber-800";
  return "bg-surface-card text-ink border-line";
}

// bannerKind picks which (if any) banner to show under the balance
// pill. Precedence:
//   1. overage_used > 0 → "overage" (even if balance is refreshed)
//   2. balance <= 0      → "out-of-credits"
//   3. trial + low tail → "trial-tail"
//   4. otherwise         → "none"
export function bannerKind(fields: CreditsFields): CreditsBannerKind {
  const balance = fields.credits_balance ?? 0;
  const monthly = fields.plan_monthly_credits ?? 0;
  const overageUsed = fields.overage_used_credits ?? 0;

  if (overageUsed > 0) return "overage";
  if (balance <= 0) return "out-of-credits";
  if (monthly === 0 && balance < 100) return "trial-tail";
  return "none";
}
