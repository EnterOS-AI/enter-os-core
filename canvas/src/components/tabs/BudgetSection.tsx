'use client';

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface BudgetData {
  budget_limit: number | null;
  budget_used?: number; // optional — provisioning-stuck workspaces return partial shapes
  budget_remaining: number | null;
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

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

/**
 * BudgetSection — dedicated "Budget" section in the workspace details panel.
 *
 * - Fetches GET /workspaces/:id/budget on mount for live usage stats
 * - Shows a progress bar (budget_used / budget_limit, blue-500, capped 100%)
 * - Allows updating budget_limit via PATCH /workspaces/:id/budget
 * - Shows a 402-specific "Budget exceeded" amber banner for any blocked state
 */
export function BudgetSection({ workspaceId }: Props) {
  const [budget, setBudget] = useState<BudgetData | null>(null);
  const [loading, setLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);

  const [limitInput, setLimitInput] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  /** True when a 402 has been seen from any API call in this section. */
  const [budgetExceeded, setBudgetExceeded] = useState(false);

  // ── Fetch current budget data ─────────────────────────────────────────────

  const loadBudget = useCallback(async () => {
    setLoading(true);
    setFetchError(null);
    try {
      const data = await api.get<BudgetData>(`/workspaces/${workspaceId}/budget`);
      setBudget(data);
      setLimitInput(data.budget_limit != null ? String(data.budget_limit) : "");
    } catch (e) {
      if (isApiError402(e)) {
        setBudgetExceeded(true);
      } else {
        setFetchError(e instanceof Error ? e.message : "Failed to load budget");
      }
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    loadBudget();
  }, [loadBudget]);

  // ── Save handler ──────────────────────────────────────────────────────────

  const handleSave = async () => {
    setSaving(true);
    setSaveError(null);
    const raw = limitInput.trim();
    // Use explicit empty-string check (not falsy check) so that a
    // user-entered "0" is sent as budget_limit: 0, not null (unlimited).
    const parsedLimit = raw !== "" ? parseInt(raw, 10) : null;

    try {
      const updated = await api.patch<BudgetData>(`/workspaces/${workspaceId}/budget`, {
        budget_limit: parsedLimit,
      });
      setBudget(updated);
      setLimitInput(updated.budget_limit != null ? String(updated.budget_limit) : "");
      // Clear exceeded state if the save succeeded (limit was raised or removed)
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

  // ── Progress calculation ──────────────────────────────────────────────────

  const progressPct =
    budget && budget.budget_limit != null && budget.budget_limit > 0
      ? Math.min(100, Math.round(((budget.budget_used ?? 0) / budget.budget_limit) * 100))
      : 0;

  // ── Render ────────────────────────────────────────────────────────────────

  return (
    <div className="space-y-3" data-testid="budget-section">
      {/* Section header */}
      <div>
        <h3 className="text-xs font-semibold text-ink-mid uppercase tracking-wider">
          Budget
        </h3>
        <p className="text-[11px] text-ink-mid mt-0.5">
          Limit total message credits for this workspace
        </p>
      </div>

      {/* 402 exceeded banner */}
      {budgetExceeded && (
        <div
          role="alert"
          data-testid="budget-exceeded-banner"
          className="flex items-center gap-2 px-3 py-2 rounded-lg bg-surface border border-amber-700/50 text-warm text-xs font-medium"
        >
          <svg
            width="13"
            height="13"
            viewBox="0 0 13 13"
            fill="none"
            aria-hidden="true"
            className="shrink-0"
          >
            <path
              d="M6.5 1.5L11.5 10.5H1.5L6.5 1.5Z"
              stroke="currentColor"
              strokeWidth="1.4"
              strokeLinejoin="round"
            />
            <path
              d="M6.5 5.5V7.5M6.5 9.5h.01"
              stroke="currentColor"
              strokeWidth="1.4"
              strokeLinecap="round"
            />
          </svg>
          Budget exceeded — messages blocked
        </div>
      )}

      {/* Usage stats */}
      {loading ? (
        <p className="text-xs text-ink-soft" data-testid="budget-loading">
          Loading…
        </p>
      ) : fetchError ? (
        <p className="text-xs text-bad" data-testid="budget-fetch-error">
          {fetchError}
        </p>
      ) : budget ? (
        <div className="space-y-2">
          {/* Stats row */}
          <div className="flex items-baseline justify-between" data-testid="budget-stats-row">
            <span className="text-xs text-ink-mid">Credits used</span>
            <span className="text-xs font-mono text-ink-mid">
              <span data-testid="budget-used-value">{(budget.budget_used ?? 0).toLocaleString()}</span>
              <span className="text-ink-soft mx-1">/</span>
              <span data-testid="budget-limit-value">
                {budget.budget_limit != null
                  ? budget.budget_limit.toLocaleString()
                  : "Unlimited"}
              </span>
            </span>
          </div>

          {/* Progress bar (only when limit is set) */}
          {budget.budget_limit != null && (
            <div
              role="progressbar"
              aria-label="Budget usage"
              aria-valuenow={progressPct}
              aria-valuemin={0}
              aria-valuemax={100}
              className="h-1.5 w-full rounded-full bg-surface-card overflow-hidden"
            >
              <div
                data-testid="budget-progress-fill"
                className="h-full rounded-full bg-accent transition-all duration-300"
                style={{ width: `${progressPct}%` }}
              />
            </div>
          )}

          {/* Remaining credits */}
          {budget.budget_remaining != null && (
            <p className="text-[11px] text-ink-soft" data-testid="budget-remaining">
              {budget.budget_remaining.toLocaleString()} credits remaining
            </p>
          )}
        </div>
      ) : null}

      {/* Input + Save */}
      <div className="space-y-1.5 pt-1">
        <label
          htmlFor={`budget-limit-input-${workspaceId}`}
          className="text-[11px] text-ink-mid block"
        >
          Budget limit (credits)
        </label>
        <input
          id={`budget-limit-input-${workspaceId}`}
          type="number"
          min="0"
          step="1"
          value={limitInput}
          onChange={(e) => setLimitInput(e.target.value)}
          placeholder="e.g. 1000 — blank for unlimited"
          data-testid="budget-limit-input"
          className="w-full bg-surface-card border border-line rounded-lg px-3 py-2 text-sm text-ink-mid placeholder-zinc-500 focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/30 transition-colors"
        />
        <p className="text-xs text-ink-soft">Leave blank for unlimited</p>

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
          className="px-4 py-1.5 bg-accent-strong hover:bg-accent active:bg-accent-strong rounded-lg text-xs font-medium text-white disabled:opacity-50 transition-colors"
        >
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
