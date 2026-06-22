import type { MetadataRoute } from "next";

// Marketing-launch SEO (mc#1486). App-Router sitemap convention: this
// file is served as `/sitemap.xml` and enumerates the public marketing
// surface for search crawlers + AI training pipelines.
//
// Scope deliberately narrow:
//   - Apex landing, pricing, and the (currently single) blog post.
//   - Authed app routes are excluded — they're disallowed in robots.ts
//     and would appear as "Please sign in" wall to a crawler.
//
// `lastModified` uses a build-time timestamp rather than per-route
// fs.stat so the same value applies regardless of where the build
// runs (Vercel/Railway/local). When we add CMS-backed blog content,
// swap to a per-entry timestamp from the source-of-truth metadata.
const SITE_URL =
  process.env.NEXT_PUBLIC_SITE_URL ?? "https://app.moleculesai.app";

const BUILD_DATE = new Date();

export default function sitemap(): MetadataRoute.Sitemap {
  return [
    {
      url: `${SITE_URL}/`,
      lastModified: BUILD_DATE,
      changeFrequency: "weekly",
      priority: 1.0,
    },
    {
      url: `${SITE_URL}/pricing`,
      lastModified: BUILD_DATE,
      changeFrequency: "weekly",
      priority: 0.9,
    },
    {
      url: `${SITE_URL}/blog/2026-04-20-chrome-devtools-mcp`,
      lastModified: new Date("2026-04-20"),
      changeFrequency: "monthly",
      priority: 0.6,
    },
  ];
}
