/**
 * Canvas-side billing helper. Talks to the control plane's /cp/billing/*
 * routes — these exist on the `molecule-cp` app in prod and are mirrored
 * via fly-replay from tenant subdomains. Dev requires a locally-running
 * control plane on the same port as PLATFORM_URL or these calls 404.
 */
import { PLATFORM_URL } from "./api";

export type PlanId = "free" | "starter" | "pro";

/**
 * Plan is the static metadata a pricing card needs to render. Kept in
 * the frontend (not fetched from the API) because changing prices or
 * feature lists requires a deploy anyway — and most of the strings are
 * marketing copy that belongs with the rest of the UI.
 */
export interface Plan {
  id: PlanId;
  name: string;
  tagline: string;
  /** Human-readable price, e.g. "$0" or "$29/month". Stored as a string
   *  so we don't accidentally leak per-tier pricing math to the client. */
  price: string;
  features: string[];
  /** CTA button label — varies per plan because free-tier is "Get started"
   *  and paid tiers are "Upgrade to Pro" etc. */
  ctaLabel: string;
  /** Visual flag for the "most popular" highlight on the middle card. */
  highlighted?: boolean;
}

// plans is the canonical order shown on the pricing page: free → starter
// → pro. Change the order here + the rendered columns follow. Keeping
// this as a module-level const so tests can assert against a known list.
//
// Flat-rate positioning (Issue #1833): "starter" and "pro" are flat-rate
// per-org, not per-seat. This is a deliberate wedge against Cursor/Windsurf
// ($40/seat) — at 5 engineers the Team tier is 28% cheaper.
export const plans: Plan[] = [
  {
    id: "free",
    name: "Free",
    tagline: "For tinkering + personal projects",
    price: "$0",
    features: [
      "3 workspaces",
      "Claude Code, Codex, Hermes, OpenClaw runtimes",
      "Shared Redis + bounded storage",
      "Community support",
    ],
    ctaLabel: "Get started",
  },
  {
    id: "starter",
    name: "Team",
    tagline: "Flat-rate for teams — one price, no per-seat fees",
    price: "$29/month",
    features: [
      "10 workspaces",
      "All runtimes + plugins",
      "Private Upstash Redis namespace",
      "Email support (48h)",
      "5M LLM tokens / month included",
      "No per-seat pricing",
    ],
    ctaLabel: "Upgrade to Team",
    highlighted: true,
  },
  {
    id: "pro",
    name: "Growth",
    tagline: "Flat-rate for production multi-agent orgs",
    price: "$99/month",
    features: [
      "Unlimited workspaces",
      "Dedicated Fly Machine per tenant",
      "Cross-workspace A2A audit log",
      "Priority support (24h)",
      "25M LLM tokens / month included",
      "No per-seat pricing",
      "Usage-based overage billing",
    ],
    ctaLabel: "Upgrade to Growth",
  },
];

export interface CheckoutResponse {
  url: string;
  id?: string;
}

/**
 * startCheckout asks the control plane to open a Stripe Checkout session
 * for the given org + plan, then returns the Stripe URL the caller
 * should window.location.href to. success_url and cancel_url default
 * to the current page with ?checkout=success / ?checkout=cancel query
 * params so the pricing page can display a confirmation banner.
 *
 * Throws on non-2xx (caller surfaces the error — the page renders a
 * toast). Does NOT automatically redirect the browser; the caller
 * decides when to navigate.
 */
export async function startCheckout(
  plan: Exclude<PlanId, "free">,
  orgSlug: string,
): Promise<CheckoutResponse> {
  // On success, send the user to /orgs so they can watch their newly-
  // paid org move from awaiting_payment → provisioning → running.
  // Landing back on /pricing (the old default) left people staring at
  // plan cards with no indication anything happened.
  // On cancel, keep them on the current page so they can retry.
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const cancelBase =
    typeof window !== "undefined"
      ? window.location.origin + window.location.pathname
      : "";
  const res = await fetch(`${PLATFORM_URL}/cp/billing/checkout`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      org_slug: orgSlug,
      plan,
      success_url: `${origin}/orgs?checkout=success`,
      cancel_url: `${cancelBase}?checkout=cancel`,
    }),
  });
  if (!res.ok) {
    // Never embed res.text() in the thrown error — the response body
    // may contain Stripe API error detail (e.g. invalid key, card decline
    // message, raw Stripe envelope) that should not reach the client.
    const detail = await res.text();
    console.error(`[billing] checkout ${res.status}: ${detail}`);
    throw new Error(`checkout failed (${res.status})`);
  }
  return res.json();
}

/**
 * openBillingPortal bounces the user to Stripe's hosted customer portal
 * so they can update their card, cancel, or download invoices. Same
 * error-handling contract as startCheckout.
 */
export async function openBillingPortal(orgSlug: string): Promise<string> {
  const returnUrl =
    typeof window !== "undefined" ? window.location.href : "";
  const res = await fetch(`${PLATFORM_URL}/cp/billing/portal`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ org_slug: orgSlug, return_url: returnUrl }),
  });
  if (!res.ok) {
    // Never embed res.text() in the thrown error — the response body
    // may contain Stripe API error detail (e.g. invalid key, card decline
    // message, raw Stripe envelope) that should not reach the client.
    const detail = await res.text();
    console.error(`[billing] portal ${res.status}: ${detail}`);
    throw new Error(`portal failed (${res.status})`);
  }
  const data = (await res.json()) as { url: string };
  return data.url;
}
