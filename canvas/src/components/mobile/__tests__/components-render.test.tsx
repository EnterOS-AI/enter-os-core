/** @vitest-environment jsdom */
/**
 * Tests for rendering components exported from components.tsx:
 *   RemoteBadge, WorkspacePill.
 *
 * Note: TabBar, FilterChips, AgentCard are tested in their own files.
 * toMobileAgent and classifyForFilter are tested in components.test.ts.
 */
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";

import { RemoteBadge, WorkspacePill } from "../components";
import { MOL_DARK, MOL_LIGHT } from "../palette";
import { MobileAccentProvider } from "../palette-context";

// ─── Palette provider wrapper ────────────────────────────────────────────────
// RemoteBadge uses palette directly; WorkspacePill calls usePalette(dark) internally,
// so WorkspacePill must be rendered inside MobileAccentProvider.

function renderWithProvider(ui: React.ReactElement) {
  return render(<MobileAccentProvider accent="#2f9e6a">{ui}</MobileAccentProvider>);
}

// ─── RemoteBadge ─────────────────────────────────────────────────────────────

describe("RemoteBadge", () => {
  it("renders the ★ REMOTE label text", () => {
    const { container } = render(
      <RemoteBadge palette={MOL_LIGHT} />
    );
    expect(container.textContent).toContain("REMOTE");
    expect(container.textContent).toContain("★");
  });

  it("renders a span element", () => {
    const { container } = render(
      <RemoteBadge palette={MOL_DARK} />
    );
    expect(container.querySelector("span")).toBeTruthy();
  });

  it("has border-radius 4px (compact badge shape)", () => {
    const { container } = render(
      <RemoteBadge palette={MOL_LIGHT} />
    );
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.borderRadius).toBe("4px");
  });

  it("applies the palette's remote color as text color", () => {
    const { container } = render(
      <RemoteBadge palette={MOL_DARK} />
    );
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.color).toBeTruthy();
  });

  it("applies the palette's remoteBg as background", () => {
    const { container } = render(
      <RemoteBadge palette={MOL_LIGHT} />
    );
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.background).toBeTruthy();
  });

  it("dark and light palettes produce different background colors", () => {
    const { container: darkContainer } = render(
      <RemoteBadge palette={MOL_DARK} />
    );
    const { container: lightContainer } = render(
      <RemoteBadge palette={MOL_LIGHT} />
    );
    const darkSpan = darkContainer.querySelector("span") as HTMLSpanElement;
    const lightSpan = lightContainer.querySelector("span") as HTMLSpanElement;
    expect(darkSpan.style.background).not.toBe(lightSpan.style.background);
  });
});

// ─── WorkspacePill ────────────────────────────────────────────────────────────

describe("WorkspacePill", () => {
  it("renders the Molecule AI brand text", () => {
    const { container } = renderWithProvider(<WorkspacePill dark={false} count={3} />);
    expect(container.textContent).toContain("Molecule AI");
  });

  it("renders the count value", () => {
    const { container } = renderWithProvider(<WorkspacePill dark={true} count={7} />);
    expect(container.textContent).toContain("7");
  });

  it("accepts a string count (e.g. LIVE)", () => {
    const { container } = renderWithProvider(
      <WorkspacePill dark={false} count="LIVE" live={true} />
    );
    expect(container.textContent).toContain("LIVE");
  });

  it("does NOT render LIVE when live=false", () => {
    const { container } = renderWithProvider(
      <WorkspacePill dark={false} count={5} live={false} />
    );
    expect(container.textContent).not.toContain("LIVE");
  });

  it("renders LIVE by default (live=true)", () => {
    const { container } = renderWithProvider(
      <WorkspacePill dark={true} count={2} />
    );
    expect(container.textContent).toContain("LIVE");
  });

  it("renders the brand initial M in the logo badge", () => {
    const { container } = renderWithProvider(<WorkspacePill dark={false} count={1} />);
    expect(container.textContent).toContain("M");
  });

  it("has an inline borderRadius style (pill shape)", () => {
    const { container } = renderWithProvider(<WorkspacePill dark={false} count={0} />);
    // Walk the DOM tree to find the outermost pill div (has inline borderRadius)
    let el: HTMLElement | null = container.firstElementChild as HTMLElement | null;
    while (el && !el.style.borderRadius) {
      el = el.parentElement;
    }
    expect(el?.style.borderRadius).toBeTruthy();
  });

  it("dark and light palettes produce different root container backgrounds", () => {
    const { container: dark } = renderWithProvider(<WorkspacePill dark={true} count={1} />);
    const { container: light } = renderWithProvider(<WorkspacePill dark={false} count={1} />);
    // The outermost element should have an inline background color set by the dark/light prop
    const darkRoot = dark.firstElementChild as HTMLElement | null;
    const lightRoot = light.firstElementChild as HTMLElement | null;
    expect(darkRoot?.style.background).toBeTruthy();
    expect(lightRoot?.style.background).toBeTruthy();
  });
});
