import type { MetadataRoute } from "next";

// Marketing-launch SEO (mc#1486). Next.js App-Router robots convention:
// this file is served as `/robots.txt` at build time and is the single
// source of truth for crawler allow/disallow.
//
// Contract:
//   - Public marketing routes (/, /pricing, /blog/*) are crawlable.
//   - Authed/app routes (/orgs, /api/*) are noindex'd. They render
//     useful content only after a session round-trip, so a crawler hit
//     just wastes our crawl budget and exposes endpoint shapes.
//   - Tenant subdomains (<slug>.moleculesai.app) share this build but
//     are blocked at the host level by the canvas middleware sending
//     an `X-Robots-Tag: noindex` header — robots.txt is per-host and
//     this file's `host` field claims the apex as canonical.
//
// Note: `sitemap` is published via the sibling `sitemap.ts` route; we
// reference it explicitly here so crawlers don't have to guess.
const SITE_URL =
  process.env.NEXT_PUBLIC_SITE_URL ?? "https://app.moleculesai.app";

export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      {
        userAgent: "*",
        allow: ["/", "/pricing", "/blog"],
        // Authed app surface + API + transient checkout returns. The
        // /orgs route boots the org-selector behind AuthGate; even
        // though SSR returns markup, that markup is a login wall when
        // hit by an unauthenticated crawler, so indexing it dilutes
        // brand searches with a "Please sign in" snippet.
        disallow: [
          "/orgs",
          "/orgs/",
          "/api/",
          "/cp/",
          "/checkout/",
        ],
      },
    ],
    sitemap: `${SITE_URL}/sitemap.xml`,
    host: SITE_URL,
  };
}
