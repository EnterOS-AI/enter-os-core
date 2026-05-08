import { PricingTable } from "@/components/PricingTable";

/**
 * /pricing — static marketing + plan-selector route.
 *
 * Served from the same canvas deploy as the tenant UI and the apex
 * landing page. Intentionally a server component so the initial HTML
 * renders with full content for SEO; PricingTable is a client
 * component that handles the CTA click + checkout POST.
 *
 * Uses the same dark theme as the canvas so the visual transition
 * from landing → pricing → in-app experience stays cohesive.
 */
export const metadata = {
  title: "Pricing — Molecule AI",
  description:
    "Flat-rate team and org pricing — no per-seat fees. Free to start, $29/month for teams, $99/month for production orgs. Full runtime stack included on every paid tier.",
};

export default function PricingPage() {
  return (
    <main className="min-h-screen bg-surface text-ink">
      <div className="mx-auto max-w-5xl px-6 pt-20 pb-8 text-center">
        <h1 className="text-5xl font-bold tracking-tight text-ink md:text-6xl">
          Pricing
        </h1>
        <p className="mx-auto mt-4 max-w-2xl text-lg text-ink-mid">
          One flat price per org — not per seat. Every paid tier includes the
          full runtime stack. You upgrade for scale, support, and dedicated
          infrastructure.
        </p>
        <p className="mx-auto mt-2 max-w-xl text-sm text-ink-mid">
          5-person team? You pay $29/month — not $200. No seat math, ever.
        </p>
      </div>

      <PricingTable />

      <section className="mx-auto mt-20 max-w-3xl px-6 text-center">
        <h2 className="text-2xl font-semibold text-ink">Questions?</h2>
        <p className="mt-2 text-ink-mid">
          We publish the{" "}
          <a
            href="https://git.moleculesai.app/molecule-ai/molecule-monorepo"
            className="text-accent underline hover:text-accent"
          >
            full source on GitHub
          </a>
          {" "}— if something's ambiguous, file an issue or{" "}
          <a
            href="mailto:support@moleculesai.app"
            className="text-accent underline hover:text-accent"
          >
            email support
          </a>
          .
        </p>
        <p className="mt-6 text-sm text-ink-soft">
          Prices shown in USD. Flat-rate per org — no per-seat fees on any paid tier.
          Enterprise / self-hosted licensing available — contact us.
        </p>
      </section>

      <footer className="mx-auto mt-20 max-w-5xl border-t border-line px-6 py-6 text-center text-sm text-ink-soft">
        <p>
          © {new Date().getFullYear()} Molecule AI, Inc. ·{" "}
          <a href="/legal/terms" className="hover:text-ink-mid">
            Terms
          </a>
          {" "}·{" "}
          <a href="/legal/privacy" className="hover:text-ink-mid">
            Privacy
          </a>
          {" "}·{" "}
          <a href="/legal/dpa" className="hover:text-ink-mid">
            DPA
          </a>
        </p>
      </footer>
    </main>
  );
}
