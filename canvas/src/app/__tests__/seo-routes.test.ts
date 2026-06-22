import { describe, it, expect, vi } from "vitest";

// Marketing-launch SEO (mc#1486). These tests pin the public crawler
// contract: anything that flips public marketing routes to disallow,
// drops the sitemap from robots.txt, or removes the OG image
// reference from root metadata should fail loudly here.

// next/font and the rest of the layout's runtime tree are not
// vitest-compatible (next/font expects the Next.js compiler swc
// transform). We import layout.tsx only for its exported `metadata`
// constant — mock the font module to a constructor-returning stub.
vi.mock("next/font/google", () => ({
  Hanken_Grotesk: () => ({ variable: "--font-hanken" }),
  JetBrains_Mono: () => ({ variable: "--font-jetbrains" }),
}));

import robots from "../robots";
import sitemap from "../sitemap";
import { metadata } from "../layout";

describe("robots.ts", () => {
  it("allows public marketing routes and blocks authed/app routes", () => {
    const r = robots();
    expect(r.rules).toBeDefined();
    const rule = Array.isArray(r.rules) ? r.rules[0] : r.rules!;
    expect(rule.userAgent).toBe("*");
    const allow = Array.isArray(rule.allow) ? rule.allow : [rule.allow];
    expect(allow).toEqual(expect.arrayContaining(["/", "/pricing", "/blog"]));
    const disallow = Array.isArray(rule.disallow)
      ? rule.disallow
      : [rule.disallow];
    expect(disallow).toEqual(
      expect.arrayContaining(["/api/", "/orgs", "/cp/"]),
    );
  });

  it("declares the sitemap URL", () => {
    const r = robots();
    expect(r.sitemap).toMatch(/\/sitemap\.xml$/);
  });

  it("declares a canonical host", () => {
    const r = robots();
    expect(r.host).toMatch(/^https:\/\//);
  });
});

describe("sitemap.ts", () => {
  it("includes apex, pricing, and the live blog post", () => {
    const entries = sitemap();
    const urls = entries.map((e) => e.url);
    expect(urls.some((u) => u.endsWith("/"))).toBe(true);
    expect(urls.some((u) => u.endsWith("/pricing"))).toBe(true);
    expect(
      urls.some((u) => u.includes("/blog/2026-04-20-chrome-devtools-mcp")),
    ).toBe(true);
  });

  it("does NOT include authed/app routes", () => {
    const entries = sitemap();
    const urls = entries.map((e) => e.url);
    expect(urls.some((u) => u.includes("/orgs"))).toBe(false);
    expect(urls.some((u) => u.includes("/api/"))).toBe(false);
  });

  it("sets a non-zero priority and a valid changeFrequency on every entry", () => {
    const valid = new Set([
      "always",
      "hourly",
      "daily",
      "weekly",
      "monthly",
      "yearly",
      "never",
    ]);
    for (const e of sitemap()) {
      expect(e.priority).toBeGreaterThan(0);
      expect(valid.has(String(e.changeFrequency))).toBe(true);
    }
  });
});

describe("root layout metadata", () => {
  it("sets a templated title + non-empty description", () => {
    const t = metadata.title as { default: string; template: string };
    expect(t.default).toMatch(/Molecule AI/);
    expect(t.template).toMatch(/%s/);
    expect((metadata.description ?? "").length).toBeGreaterThan(50);
  });

  it("declares OG + Twitter text fields (image comes from opengraph-image.tsx)", () => {
    const og = metadata.openGraph;
    expect(og).toBeDefined();
    expect((og as { title: string }).title).toMatch(/Molecule AI/);
    expect((og as { description: string }).description.length).toBeGreaterThan(
      50,
    );
    const tw = metadata.twitter;
    expect(tw).toBeDefined();
    // Next.js typings narrow twitter.card to a union — assert via cast.
    expect((tw as { card: string }).card).toBe("summary_large_image");
  });

  it("sets a canonical alternate", () => {
    expect(metadata.alternates?.canonical).toBe("/");
  });

  it("enables indexing at the metadata level (robots.ts owns per-route)", () => {
    const r = metadata.robots as { index: boolean; follow: boolean };
    expect(r.index).toBe(true);
    expect(r.follow).toBe(true);
  });
});
