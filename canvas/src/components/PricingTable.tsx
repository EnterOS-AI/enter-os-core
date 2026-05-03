"use client";

import { useState } from "react";
import { plans, startCheckout, type Plan, type PlanId } from "@/lib/billing";
import { fetchSession, redirectToLogin, type Session } from "@/lib/auth";
import { getTenantSlug } from "@/lib/tenant";

/**
 * PricingTable renders the three plan cards and wires each CTA button
 * through a dispatcher:
 *
 *   Free                 → kick to signup
 *   Anonymous + paid     → kick to signup (Stripe checkout after auth)
 *   Authed + paid        → POST /cp/billing/checkout and redirect
 *   Any network failure  → surface a simple error banner in-place
 *
 * Session is fetched lazily on first click rather than on mount so
 * anonymous users can browse the pricing page without a probe request.
 */
export function PricingTable() {
  const [error, setError] = useState<string | null>(null);
  const [loadingPlan, setLoadingPlan] = useState<PlanId | null>(null);

  const handleClick = async (plan: Plan) => {
    setError(null);
    if (plan.id === "free") {
      redirectToLogin("sign-up");
      return;
    }
    setLoadingPlan(plan.id);
    try {
      // Lazy session probe — we only need it when the user commits to
      // a paid plan, not on page load.
      let session: Session | null = null;
      try {
        session = await fetchSession();
      } catch (e) {
        // Network error probing /cp/auth/me is treated the same as
        // anonymous here — a real 5xx from CP would also block a
        // Stripe checkout, so bouncing to signup is the safe path.
        session = null;
      }
      if (!session) {
        redirectToLogin("sign-up");
        return;
      }
      // Session.org_id is the WorkOS org id, not the slug — we need the
      // slug for the checkout endpoint. The slug lives in the URL on
      // tenant subdomains (<slug>.moleculesai.app), so we read it from
      // the helper. Session without a tenant slug means the user is on
      // the canvas apex and needs to pick an org first.
      const slug = getTenantSlug();
      if (!slug) {
        setError("Open a specific org on its tenant subdomain to upgrade.");
        return;
      }
      const result = await startCheckout(plan.id as Exclude<PlanId, "free">, slug);
      window.location.href = result.url;
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingPlan(null);
    }
  };

  return (
    <div className="mx-auto max-w-6xl px-6">
      {error && (
        <div
          role="alert"
          className="mx-auto mb-6 max-w-3xl rounded border border-red-900 bg-red-950/40 p-4 text-sm text-red-200"
        >
          {error}
        </div>
      )}
      <div className="grid gap-6 md:grid-cols-3">
        {plans.map((plan) => (
          <PlanCard
            key={plan.id}
            plan={plan}
            loading={loadingPlan === plan.id}
            onSelect={() => handleClick(plan)}
          />
        ))}
      </div>
    </div>
  );
}

function PlanCard({
  plan,
  loading,
  onSelect,
}: {
  plan: Plan;
  loading: boolean;
  onSelect: () => void;
}) {
  const ring = plan.highlighted
    ? "border-accent ring-2 ring-blue-600/30"
    : "border-line";
  return (
    <article
      className={`flex flex-col rounded-lg border ${ring} bg-surface-sunken/40 p-6`}
      aria-labelledby={`plan-${plan.id}-name`}
    >
      {plan.highlighted && (
        <span className="mb-3 inline-block rounded-full bg-accent-strong/20 px-3 py-1 text-xs font-medium text-accent">
          Most popular
        </span>
      )}
      <h2 id={`plan-${plan.id}-name`} className="text-xl font-semibold text-ink">
        {plan.name}
      </h2>
      <p className="mt-1 text-sm text-ink-mid">{plan.tagline}</p>
      <p className="mt-4 text-3xl font-bold text-ink">{plan.price}</p>
      <ul className="mt-6 flex-1 space-y-2 text-sm text-ink-mid">
        {plan.features.map((f) => (
          <li key={f} className="flex items-start">
            <span className="mr-2 text-accent" aria-hidden>
              ✓
            </span>
            {f}
          </li>
        ))}
      </ul>
      <button
        type="button"
        onClick={onSelect}
        disabled={loading}
        className={`mt-6 rounded-lg px-4 py-3 text-sm font-medium ${
          plan.highlighted
            ? "bg-accent-strong text-white hover:bg-accent disabled:bg-blue-900"
            : "border border-line bg-surface-sunken text-ink hover:bg-surface-card disabled:opacity-50"
        }`}
      >
        {loading ? "Opening checkout…" : plan.ctaLabel}
      </button>
    </article>
  );
}

