// WCAG-AA contrast gate for the canvas design-token SSOT (core#2742).
//
// The light @theme `good`/`bad`/`ink-soft` once shipped values that FAILED
// WCAG AA 4.5:1 on their own 10%-tint badges (axe measured text-good #0c8a52
// on bg-good/10 = 3.87, bad = 4.46) — molecule-app's stricter a11y e2e caught
// it (issue #48) only after it adopted these tokens. This unit-level gate
// PARSES globals.css and computes the contrast for exactly those at-risk pairs
// so the SSOT itself can never regress to inaccessible values again — and it
// runs in the already-gated canvas vitest suite (no Playwright infra needed).
//
// Pairs gated (light mode; dark already passes AA):
//   • text-good  on bg-good/10  (status badge: 10%-tint of good over white)
//   • text-bad   on bg-bad/10
//   • text-ink-soft on surface AND on surface-elevated (muted body/labels)

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const css = readFileSync(fileURLToPath(new URL("../globals.css", import.meta.url)), "utf8");

// The light warm-paper palette: first `@theme {` block defining --color-surface.
function lightTokens(): Record<string, string> {
  let from = 0;
  for (;;) {
    const start = css.indexOf("@theme {", from);
    if (start === -1) throw new Error("light @theme block not found");
    const open = css.indexOf("{", start);
    const body = css.slice(open, css.indexOf("\n}", open));
    if (body.includes("--color-surface")) {
      const out: Record<string, string> = {};
      for (const m of body.matchAll(/--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;/g)) out[m[1]] = m[2];
      return out;
    }
    from = open + 1;
  }
}

type RGB = [number, number, number];
const hex2rgb = (h: string): RGB => {
  const s = h.replace("#", "");
  return [0, 2, 4].map((i) => parseInt(s.slice(i, i + 2), 16)) as RGB;
};
const lin = (c: number) => {
  const x = c / 255;
  return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4;
};
const lum = ([r, g, b]: RGB) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
const contrast = (fg: RGB, bg: RGB) => {
  const a = lum(fg), b = lum(bg), hi = Math.max(a, b), lo = Math.min(a, b);
  return (hi + 0.05) / (lo + 0.05);
};
// `bg-<token>/10` = the token color at 10% alpha composited over `over`.
const tint = (fg: RGB, alpha: number, over: RGB): RGB =>
  fg.map((c, i) => Math.round(c * alpha + over[i] * (1 - alpha))) as RGB;

const AA = 4.5;

describe("canvas design-token SSOT — WCAG AA contrast (core#2742)", () => {
  const t = lightTokens();
  const white = hex2rgb(t["surface-elevated"]); // badges sit on elevated white
  const surface = hex2rgb(t["surface"]);

  it("text-good on bg-good/10 clears AA (was 3.87 with #0c8a52)", () => {
    const good = hex2rgb(t["good"]);
    const ratio = contrast(good, tint(good, 0.1, white));
    expect(ratio, `text-good on bg-good/10 = ${ratio.toFixed(2)}`).toBeGreaterThanOrEqual(AA);
  });

  it("text-bad on bg-bad/10 clears AA (was 4.46 with #c2403c)", () => {
    const bad = hex2rgb(t["bad"]);
    const ratio = contrast(bad, tint(bad, 0.1, white));
    expect(ratio, `text-bad on bg-bad/10 = ${ratio.toFixed(2)}`).toBeGreaterThanOrEqual(AA);
  });

  it("text-ink-soft clears AA on surface and surface-elevated", () => {
    const ink = hex2rgb(t["ink-soft"]);
    const onSurface = contrast(ink, surface);
    const onElev = contrast(ink, white);
    expect(onSurface, `ink-soft on surface = ${onSurface.toFixed(2)}`).toBeGreaterThanOrEqual(AA);
    expect(onElev, `ink-soft on surface-elevated = ${onElev.toFixed(2)}`).toBeGreaterThanOrEqual(AA);
  });
});
