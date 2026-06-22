'use client';

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Period keys MUST match the server SSOT (workspace-server budget_periods.go).
type BudgetPeriod = "hourly" | "daily" | "weekly" | "monthly";

const PERIODS: { key: BudgetPeriod; label: string }[] = [
  { key: "hourly", label: "Hourly" },
  { key: "daily", label: "Daily" },
  { key: "weekly", label: "Weekly" },
  { key: "monthly", label: "Monthly" },
];

interface PeriodBudget {
  limit: number | null; // USD cents; null = no limit
  spend: number; // rolling-window spend, USD cents
  remaining: number | null; // null when no limit
}

interface BudgetData {
  periods?: Partial<Record<BudgetPeriod, PeriodBudget>>;
  // legacy fields (pre-multi-period server) — tolerated for back-compat
  budget_limit?: number | null;
  monthly_spend?: number;
  budget_remaining?: number | null;
}

interface Props {
  workspaceId: string;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** True when an API error carries a 402 status code. */
function isApiError402(e: unknown): boolean {
  return e instanceof Error && /: 402( |$)/.test(e.message);
}

/** USD cents → "$X.XX". */
function fmtUSD(cents: number): string {
  return `$${(cents / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** Normalize the server payload (multi-period or legacy) into a period map. */
function periodsFrom(data: BudgetData | null): Record<BudgetPeriod, PeriodBudget> {
  const base: Record<BudgetPeriod, PeriodBudget> = {
    hourly: { limit: null, spend: 0, remaining: null },
    daily: { limit: null, spend: 0, remaining: null },
    weekly: { limit: null, spend: 0, remaining: null },
    monthly: { limit: null, spend: 0, remaining: null },
  };
  if (!data) return base;
  if (data.periods) {
    for (const { key } of PERIODS) {
      const p = data.periods[key];
      if (p) base[key] = { limit: p.limit ?? null, spend: p.spend ?? 0, remaining: p.remaining ?? null };
    }
    return base;
  }
  // legacy: map the single monthly limit/spend
  base.monthly = {
    limit: data.budget_limit ?? null,
    spend: data.monthly_spend ?? 0,
    remaining: data.budget_remaining ?? null,
  };
  return base;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

/**
 * BudgetSection — per-workspace LLM budget, four independent rolling windows
 * (hourly / daily / weekly / monthly). Each period has its own ceiling (USD);
 * spend is the rolling-window LLM cost. Crossing ANY period blocks new work
 * (server returns 402). Sends PATCH {budget_limits:{period:cents|null}}.
 */
export function BudgetSection({ workspaceId }: Props) {
  const [budget, setBudget] = useState<BudgetData | null>(null);
  const [loading, setLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);

  // One input per period, in USD cents (string for controlled inputs).
  const [limitInputs, setLimitInputs] = useState<Record<BudgetPeriod, string>>({
    hourly: "",
    daily: "",
    weekly: "",
    monthly: "",
  });
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [budgetExceeded, setBudgetExceeded] = useState(false);

  const syncInputs = useCallback((data: BudgetData | null) => {
    const p = periodsFrom(data);
    setLimitInputs({
      hourly: p.hourly.limit != null ? String(p.hourly.limit) : "",
      daily: p.daily.limit != null ? String(p.daily.limit) : "",
      weekly: p.weekly.limit != null ? String(p.weekly.limit) : "",
      monthly: p.monthly.limit != null ? String(p.monthly.limit) : "",
    });
  }, []);

  const loadBudget = useCallback(async () => {
    setLoading(true);
    setFetchError(null);
    try {
      const data = await api.get<BudgetData>(`/workspaces/${workspaceId}/budget`);
      setBudget(data);
      syncInputs(data);
    } catch (e) {
      if (isApiError402(e)) {
        setBudgetExceeded(true);
      } else {
        setFetchError(e instanceof Error ? e.message : "Failed to load budget");
      }
    } finally {
      setLoading(false);
    }
  }, [workspaceId, syncInputs]);

  useEffect(() => {
    loadBudget();
  }, [loadBudget]);

  const handleSave = async () => {
    setSaving(true);
    setSaveError(null);
    // Build the per-period map: blank → null (clear); a number → that ceiling.
    const budget_limits: Record<BudgetPeriod, number | null> = {
      hourly: null,
      daily: null,
      weekly: null,
      monthly: null,
    };
    for (const { key } of PERIODS) {
      const raw = limitInputs[key].trim();
      budget_limits[key] = raw !== "" ? parseInt(raw, 10) : null;
    }
    try {
      const updated = await api.patch<BudgetData>(`/workspaces/${workspaceId}/budget`, { budget_limits });
      setBudget(updated);
      syncInputs(updated);
      setBudgetExceeded(false);
    } catch (e) {
      if (isApiError402(e)) {
        setBudgetExceeded(true);
      } else {
        setSaveError(e instanceof Error ? e.message : "Failed to save budget");
      }
    } finally {
      setSaving(false);
    }
  };

  const periods = periodsFrom(budget);

  return (
    <div className="space-y-3" data-testid="budget-section">
      {/* Section header */}
      <div>
        <h3 className="text-xs font-semibold text-ink-mid uppercase tracking-wider">Budget</h3>
        <p className="text-[11px] text-ink-mid mt-0.5">
          Cap LLM spend for this workspace per period — crossing any limit pauses new work
        </p>
      </div>

      {/* 402 exceeded banner */}
      {budgetExceeded && (
        <div
          role="alert"
          data-testid="budget-exceeded-banner"
          className="flex items-center gap-2 px-3 py-2 rounded-lg bg-surface border border-amber-700/50 text-warm text-xs font-medium"
        >
          <svg width="13" height="13" viewBox="0 0 13 13" fill="none" aria-hidden="true" className="shrink-0">
            <path d="M6.5 1.5L11.5 10.5H1.5L6.5 1.5Z" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" />
            <path d="M6.5 5.5V7.5M6.5 9.5h.01" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
          </svg>
          Budget exceeded — new work paused
        </div>
      )}

      {loading ? (
        <p className="text-xs text-ink-mid" data-testid="budget-loading">
          Loading…
        </p>
      ) : fetchError ? (
        <p className="text-xs text-bad" data-testid="budget-fetch-error">
          {fetchError}
        </p>
      ) : (
        <div className="space-y-3">
          {PERIODS.map(({ key, label }) => {
            const p = periods[key];
            const pct =
              p.limit != null && p.limit > 0 ? Math.min(100, Math.round((p.spend / p.limit) * 100)) : 0;
            const over = p.limit != null && p.spend >= p.limit;
            return (
              <div key={key} className="space-y-1" data-testid={`budget-period-${key}`}>
                <div className="flex items-baseline justify-between">
                  <label htmlFor={`budget-${key}-${workspaceId}`} className="text-xs text-ink-mid">
                    {label}
                  </label>
                  <span className="text-[11px] font-mono text-ink-mid">
                    <span data-testid={`budget-${key}-spend`}>{fmtUSD(p.spend)}</span>
                    <span className="mx-1">/</span>
                    <span data-testid={`budget-${key}-limit`}>{p.limit != null ? fmtUSD(p.limit) : "∞"}</span>
                  </span>
                </div>
                {p.limit != null && (
                  <div
                    role="progressbar"
                    aria-label={`${label} budget usage`}
                    aria-valuenow={pct}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    className="h-1.5 w-full rounded-full bg-surface-card overflow-hidden"
                  >
                    <div
                      data-testid={`budget-${key}-fill`}
                      className={`h-full rounded-full transition-all duration-300 ${over ? "bg-bad" : "bg-accent"}`}
                      style={{ width: `${pct}%` }}
                    />
                  </div>
                )}
                <input
                  id={`budget-${key}-${workspaceId}`}
                  type="number"
                  min="0"
                  step="1"
                  value={limitInputs[key]}
                  onChange={(e) => setLimitInputs((s) => ({ ...s, [key]: e.target.value }))}
                  placeholder="USD cents — blank for unlimited"
                  data-testid={`budget-${key}-input`}
                  className="w-full bg-surface-card border border-line rounded-lg px-3 py-1.5 text-xs text-ink-mid placeholder-zinc-500 focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/30 transition-colors"
                />
              </div>
            );
          })}

          <p className="text-[11px] text-ink-mid">Limits are USD cents (e.g. 500 = $5.00). Blank = unlimited.</p>

          {saveError && (
            <div
              role="alert"
              data-testid="budget-save-error"
              className="px-3 py-1.5 rounded-lg bg-red-950/40 border border-red-800/50 text-xs text-bad"
            >
              {saveError}
            </div>
          )}

          <button
            onClick={handleSave}
            disabled={saving}
            data-testid="budget-save-btn"
            className="px-4 py-1.5 bg-accent-strong hover:bg-accent active:bg-accent-strong rounded-lg text-xs font-medium text-white disabled:opacity-50 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900"
          >
            {saving ? "Saving…" : "Save"}
          </button>
        </div>
      )}
    </div>
  );
}
