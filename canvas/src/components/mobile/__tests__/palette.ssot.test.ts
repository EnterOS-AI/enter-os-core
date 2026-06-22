// Design-token SSOT drift gate (core#mobile-design-parity).
//
// The mobile palette's CORE tokens must stay equal to the canonical canvas
// @theme tokens in src/app/globals.css — the single source of truth for the
// warm-paper + near-black-dark surfaces and the PURPLE brand accent. This
// test PARSES globals.css (not a second hardcoded copy) and asserts the
// mobile MOL_LIGHT / MOL_DARK core values match, so the two can't silently
// re-diverge (the mobile UI once shipped a green accent + lighter surfaces).
//
// Status/tier badge colors are intentionally mobile-specific and NOT gated
// here — only the core design language (surfaces, text, borders, accent, and
// good→green) is the cross-surface SSOT.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { MOL_DARK, MOL_LIGHT } from "../palette";

const css = readFileSync(
  fileURLToPath(new URL("../../../app/globals.css", import.meta.url)),
  "utf8",
);

// Extract the `--color-<name>: <value>;` map from the FIRST block opened by
// `selector {` that actually defines --color-surface (globals.css has
// several blocks per selector — e.g. an @theme bg block + a
// [data-theme="dark"] .shadow-embossed rule — that don't hold the palette).
function tokensInBlock(selector: string): Record<string, string> {
  let from = 0;
  for (;;) {
    const start = css.indexOf(selector + " {", from);
    if (start === -1) throw new Error(`palette block not found for: ${selector}`);
    const open = css.indexOf("{", start);
    const close = css.indexOf("\n}", open);
    const body = css.slice(open, close);
    if (body.includes("--color-surface")) {
      const out: Record<string, string> = {};
      for (const m of body.matchAll(/--color-([a-z0-9-]+):\s*([^;]+);/g)) {
        out[m[1]] = m[2].trim();
      }
      return out;
    }
    from = open + 1;
  }
}

// The warm-paper light palette lives in the first `@theme {` block that
// defines --color-surface; dark values under `[data-theme="dark"] {`.
const light = tokensInBlock("@theme");
const dark = tokensInBlock('[data-theme="dark"]');

describe("mobile palette ↔ canvas @theme SSOT", () => {
  it("MOL_LIGHT core tokens equal the canvas light @theme", () => {
    expect(MOL_LIGHT.bg).toBe(light["surface"]);
    expect(MOL_LIGHT.surface).toBe(light["surface-elevated"]);
    expect(MOL_LIGHT.surface2).toBe(light["surface-card"]);
    expect(MOL_LIGHT.border).toBe(light["line"]);
    expect(MOL_LIGHT.divider).toBe(light["line-soft"]);
    expect(MOL_LIGHT.text).toBe(light["ink"]);
    expect(MOL_LIGHT.text2).toBe(light["ink-mid"]);
    expect(MOL_LIGHT.text3).toBe(light["ink-soft"]);
    expect(MOL_LIGHT.accent).toBe(light["accent"]);
    expect(MOL_LIGHT.green).toBe(light["good"]);
    expect(MOL_LIGHT.online).toBe(light["good"]);
  });

  it("MOL_DARK core tokens equal the canvas dark [data-theme] block", () => {
    expect(MOL_DARK.bg).toBe(dark["surface"]);
    expect(MOL_DARK.surface).toBe(dark["surface-elevated"]);
    expect(MOL_DARK.surface2).toBe(dark["surface-card"]);
    expect(MOL_DARK.border).toBe(dark["line"]);
    expect(MOL_DARK.divider).toBe(dark["line-soft"]);
    expect(MOL_DARK.text).toBe(dark["ink"]);
    expect(MOL_DARK.text2).toBe(dark["ink-mid"]);
    expect(MOL_DARK.text3).toBe(dark["ink-soft"]);
    expect(MOL_DARK.accent).toBe(dark["accent"]);
    expect(MOL_DARK.green).toBe(dark["good"]);
    expect(MOL_DARK.online).toBe(dark["good"]);
  });

  it("the brand accent is the canvas purple, not the legacy mobile green", () => {
    expect(MOL_LIGHT.accent.toLowerCase()).toBe("#7c3aed");
    expect(MOL_DARK.accent.toLowerCase()).toBe("#a78bfa");
    expect(MOL_LIGHT.accent).not.toBe("#2f9e6a");
  });
});
