"use client";

// /orgs — the post-signup landing page.
//
// The control plane's Callback handler (authorized via WorkOS) redirects
// every new session to APP_URL/orgs after login/signup succeeds. Before
// this route existed that redirect 404'd and new users were stranded.
// Now:
//   - Signed-out browsers are bounced back to /cp/auth/login
//   - Zero-org users see a slug-picker → POST /cp/orgs → refresh
//   - `awaiting_payment` orgs get a "Complete payment" CTA → /pricing
//   - `running` orgs show a link to the tenant URL
//   - `provisioning` / `failed` surface the state so the user knows
//     why their tenant isn't available yet
//
// Everything here is intentionally server-light: one GET /cp/orgs,
// zero WebSocket, no canvas store hydration — the whole point is a
// quick bounce between signup and either Checkout or the tenant UI.

import { useEffect, useState } from "react";
import { fetchSession, redirectToLogin, type Session } from "@/lib/auth";
import { PLATFORM_URL } from "@/lib/api";
import { formatCredits, pillTone, bannerKind } from "@/lib/credits";
import { TermsGate } from "@/components/TermsGate";

type OrgStatus = "awaiting_payment" | "provisioning" | "running" | "failed" | string;

interface Org {
  id: string;
  slug: string;
  name: string;
  plan: string;
  status: OrgStatus;
  created_at: string;
  updated_at: string;
  // Credit system fields. Present whenever the control plane's models
  // serializer runs — tests + older snapshot JSONs may not have them,
  // so treat as optional in TS and fall back to 0 at render time.
  credits_balance?: number;
  plan_monthly_credits?: number;
  overage_used_credits?: number;
  overage_cap_credits?: number;
}

export default function OrgsPage() {
  const [session, setSession] = useState<Session | null | "loading">("loading");
  const [orgs, setOrgs] = useState<Org[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [justCheckedOut, setJustCheckedOut] = useState(false);

  useEffect(() => {
    // URLSearchParams is safe on the first render because this component
    // is "use client" — window exists. Clear the flag from the URL so
    // reloading the page doesn't keep showing the banner indefinitely.
    if (typeof window !== "undefined") {
      const params = new URLSearchParams(window.location.search);
      if (params.get("checkout") === "success") {
        setJustCheckedOut(true);
        window.history.replaceState({}, "", window.location.pathname);
      }
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    let pollTimer: ReturnType<typeof setTimeout> | null = null;

    const fetchOrgs = async () => {
      try {
        const sess = await fetchSession();
        if (cancelled) return;
        if (!sess) {
          redirectToLogin();
          return;
        }
        setSession(sess);
        const res = await fetch(`${PLATFORM_URL}/cp/orgs`, {
          credentials: "include",
          signal: AbortSignal.timeout(15_000),
        });
        if (!res.ok) {
          throw new Error(`GET /cp/orgs: ${res.status}`);
        }
        const body = (await res.json()) as { orgs?: Org[] } | Org[];
        const list = Array.isArray(body) ? body : body.orgs ?? [];
        if (cancelled) return;
        setOrgs(list);

        // Poll while anything is still moving so the user sees the
        // status flip live after a Stripe Checkout. 5s is frequent
        // enough to feel responsive, slow enough to not DoS the CP.
        const stillMoving = list.some(
          (o) => o.status === "provisioning" || o.status === "awaiting_payment"
        );
        if (stillMoving) {
          pollTimer = setTimeout(fetchOrgs, 5_000);
        }
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
        }
      }
    };

    fetchOrgs();
    return () => {
      cancelled = true;
      if (pollTimer) clearTimeout(pollTimer);
    };
  }, []);

  if (session === "loading" || (orgs === null && error === null)) {
    return <Shell><p className="text-ink-mid">Loading…</p></Shell>;
  }
  if (error) {
    return (
      <Shell>
        <p role="alert" className="text-bad">Error: {error}</p>
        <button
          onClick={() => window.location.reload()}
          className="mt-4 rounded bg-surface-card px-4 py-2 text-sm text-ink hover:bg-surface-card"
        >
          Retry
        </button>
      </Shell>
    );
  }
  if (!orgs || orgs.length === 0) {
    return <EmptyState banner={justCheckedOut ? <CheckoutBanner /> : null} />;
  }
  return (
    <Shell>
      {justCheckedOut && <CheckoutBanner />}
      <ul className="space-y-3">
        {orgs.map((o) => (
          <OrgRow key={o.id} org={o} />
        ))}
      </ul>
      <div className="mt-8 border-t border-line pt-6">
        <CreateOrgForm
          onCreated={(slug) => {
            // Refresh the list so the new org appears + its CTA fires.
            window.location.reload();
            void slug;
          }}
        />
      </div>
    </Shell>
  );
}

function CheckoutBanner() {
  return (
    <div role="status" aria-live="polite" className="mb-6 rounded-lg border border-emerald-700 bg-emerald-950 p-4">
      <p className="text-sm text-emerald-200">
        <span aria-hidden="true">✓</span> Payment confirmed. Your workspace is spinning up now — this page
        refreshes automatically when it&apos;s ready.
      </p>
    </div>
  );
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <main className="min-h-screen bg-surface text-ink">
      <TermsGate>
        <div className="mx-auto max-w-2xl px-6 pt-20 pb-12">
          <h1 className="text-3xl font-bold text-ink">Your organizations</h1>
          <p className="mt-2 text-ink-mid">
            Each org is an isolated Molecule workspace.
          </p>
          <DataResidencyNotice />
          <div className="mt-8">{children}</div>
        </div>
      </TermsGate>
    </main>
  );
}

// DataResidencyNotice surfaces where workspace data lives so EU-based
// signups can make an informed choice (GDPR Art. 13 disclosure
// requirement). Plain text, no icon — the goal is clarity, not
// decoration. A future EU region selector can replace this with a
// region dropdown.
function DataResidencyNotice() {
  return (
    <p className="mt-3 rounded border border-line bg-surface-sunken/60 px-3 py-2 text-xs text-ink-mid">
      Workspaces run in AWS us-east-2 (Ohio, United States). EU region support is on the roadmap — reach out to
      {" "}
      <a href="mailto:support@moleculesai.app" className="underline">
        support@moleculesai.app
      </a>
      {" "}if you need data residency in another region today.
    </p>
  );
}

function OrgRow({ org }: { org: Org }) {
  return (
    <li className="rounded-lg border border-line bg-surface-sunken p-4">
      <div className="flex items-center justify-between">
        <div>
          <div className="font-medium text-ink">{org.name}</div>
          <div className="text-sm text-ink-mid">
            {org.slug} · <StatusLabel status={org.status} /> · {org.plan || "free"}
          </div>
          <div className="mt-2 flex items-center gap-2">
            <CreditsPill org={org} />
            <LowCreditsBanner org={org} />
          </div>
        </div>
        <OrgCTA org={org} />
      </div>
    </li>
  );
}

// CreditsPill renders the balance with a tone that matches the banner
// severity. Format + color logic lives in @/lib/credits so it can be
// tested without mounting React.
function CreditsPill({ org }: { org: Org }) {
  const balance = org.credits_balance ?? 0;
  return (
    <span className={`rounded border px-2 py-0.5 text-xs ${pillTone(org)}`} title="Credit balance">
      {formatCredits(balance)} credits
    </span>
  );
}

// LowCreditsBanner is a one-liner that only renders when the balance
// is low AND the org is running. bannerKind() picks which message to
// show; render just dispatches on it.
function LowCreditsBanner({ org }: { org: Org }) {
  if (org.status !== "running") return null;
  const kind = bannerKind(org);
  if (kind === "none") return null;
  if (kind === "overage") {
    const used = (org.overage_used_credits ?? 0).toLocaleString();
    return (
      <span className="text-xs text-warm">
        overage active · {used} used
      </span>
    );
  }
  if (kind === "out-of-credits") {
    return (
      <a href={`/pricing?org=${encodeURIComponent(org.slug)}`} className="text-xs text-bad underline">
        out of credits — upgrade to keep running
      </a>
    );
  }
  // trial-tail
  return (
    <a href={`/pricing?org=${encodeURIComponent(org.slug)}`} className="text-xs text-warm underline">
      trial almost out
    </a>
  );
}

function StatusLabel({ status }: { status: OrgStatus }) {
  const cls =
    status === "running"
      ? "text-good"
      : status === "awaiting_payment"
      ? "text-warm"
      : status === "failed"
      ? "text-bad"
      : "text-sky-400";
  const label =
    status === "awaiting_payment"
      ? "awaiting payment"
      : status;
  return <span className={cls}>{label}</span>;
}

function OrgCTA({ org }: { org: Org }) {
  if (org.status === "running") {
    const host = typeof window !== "undefined" ? window.location.hostname : "moleculesai.app";
    const appDomain = host.endsWith(".moleculesai.app")
      ? host.split(".").slice(-2).join(".")
      : "moleculesai.app";
    const href = `https://${org.slug}.${appDomain}`;
    return (
      <a
        href={href}
        className="rounded bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-500"
      >
        Open
      </a>
    );
  }
  if (org.status === "awaiting_payment") {
    return (
      <a
        href={`/pricing?org=${encodeURIComponent(org.slug)}`}
        className="rounded bg-amber-600 px-4 py-2 text-sm font-medium text-white hover:bg-amber-500"
      >
        Complete payment
      </a>
    );
  }
  if (org.status === "failed") {
    return (
      <a
        href="mailto:support@moleculesai.app"
        className="rounded bg-surface-card px-4 py-2 text-sm font-medium text-ink hover:bg-surface-card"
      >
        Contact support
      </a>
    );
  }
  // provisioning / unknown — non-interactive
  return <span className="text-sm text-ink-soft">{org.status}…</span>;
}

function EmptyState({ banner }: { banner?: React.ReactNode }) {
  return (
    <Shell>
      {banner}
      <p className="text-ink-mid">
        You don't have any organizations yet. Create one to get started — your
        workspace spins up automatically once billing is set up.
      </p>
      <div className="mt-6">
        <CreateOrgForm
          onCreated={() => {
            window.location.reload();
          }}
        />
      </div>
    </Shell>
  );
}

function CreateOrgForm({ onCreated }: { onCreated: (slug: string) => void }) {
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setSubmitting(true);
    setErr(null);
    try {
      const res = await fetch(`${PLATFORM_URL}/cp/orgs`, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ slug, name }),
        signal: AbortSignal.timeout(15_000),
      });
      if (!res.ok) {
        const body = await res.text();
        console.error(`[orgs] create ${res.status}: ${body}`);
        throw new Error(`Failed to create organization (${res.status})`);
      }
      onCreated(slug);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <div>
        <label htmlFor="org-slug" className="block text-sm text-ink-mid">Slug (URL)</label>
        <input
          id="org-slug"
          value={slug}
          onChange={(e) => setSlug(e.target.value.toLowerCase())}
          pattern="^[a-z][a-z0-9-]{2,31}$"
          placeholder="acme"
          required
          aria-describedby="org-slug-hint"
          className="mt-1 w-full rounded border border-line bg-surface-card px-3 py-2 text-sm text-ink"
        />
        <p id="org-slug-hint" className="mt-1 text-xs text-ink-soft">
          Lowercase letters, numbers, and hyphens only. Cannot be changed later.
        </p>
      </div>
      <div>
        <label htmlFor="org-name" className="block text-sm text-ink-mid">Display name</label>
        <input
          id="org-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Acme Corp"
          required
          className="mt-1 w-full rounded border border-line bg-surface-card px-3 py-2 text-sm text-ink"
        />
      </div>
      {err && <p role="alert" className="text-sm text-bad">{err}</p>}
      <button
        type="submit"
        disabled={submitting}
        className="rounded bg-accent-strong px-4 py-2 text-sm font-medium text-white hover:bg-accent disabled:opacity-50"
      >
        {submitting ? "Creating…" : "Create organization"}
      </button>
    </form>
  );
}
