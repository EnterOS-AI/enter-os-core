// @vitest-environment jsdom
/**
 * Mobile primitives — StatusDot, TierChip, Chip, SectionLabel.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { Chip, SectionLabel, StatusDot, TierChip } from "../primitives";

afterEach(() => {
  cleanup();
});

// ─── StatusDot ──────────────────────────────────────────────────────────────

describe("StatusDot", () => {
  it("renders a span with correct size", () => {
    const { container } = render(<StatusDot size={12} />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span).toBeTruthy();
    expect(span.style.width).toBe("12px");
    expect(span.style.height).toBe("12px");
  });

  it("has border-radius 999 (circle)", () => {
    const { container } = render(<StatusDot size={8} />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.borderRadius).toBe("999px");
  });

  it("has flexShrink: 0 to prevent collapsing in flex rows", () => {
    const { container } = render(<StatusDot size={6} />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.flexShrink).toBe("0");
  });

  it("has halo boxShadow by default (halo=true)", () => {
    const { container } = render(<StatusDot size={8} />);
    const span = container.querySelector("span") as HTMLSpanElement;
    // Math.max(2, 8*0.45) = Math.max(2, 3.6) = 3.6 → "3.6px"
    expect(span.style.boxShadow).toContain("px");
  });

  it("has no boxShadow when halo=false", () => {
    const { container } = render(<StatusDot size={8} halo={false} />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.boxShadow).toBe("none");
  });

  it("renders with default props (size=8, halo=true, status=online)", () => {
    const { container } = render(<StatusDot />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.width).toBe("8px");
    expect(span.style.height).toBe("8px");
    expect(span.style.boxShadow).not.toBe("none");
  });
});

// ─── TierChip ───────────────────────────────────────────────────────────────

describe("TierChip", () => {
  it("renders the tier text inside a span", () => {
    const { container } = render(<TierChip tier="T1" />);
    expect(container.textContent).toContain("T1");
  });

  it("renders T1, T2, T3, T4 with correct text", () => {
    for (const tier of ["T1", "T2", "T3", "T4"] as const) {
      const { container } = render(<TierChip tier={tier} />);
      expect(container.textContent).toBe(tier);
    }
  });

  it("sm size renders smaller dimensions than lg", () => {
    const { container: sm } = render(<TierChip tier="T2" size="sm" />);
    const { container: lg } = render(<TierChip tier="T2" size="lg" />);
    const smSpan = sm.querySelector("span") as HTMLSpanElement;
    const lgSpan = lg.querySelector("span") as HTMLSpanElement;
    expect(smSpan.style.width).toBe("26px");
    expect(smSpan.style.height).toBe("19px");
    expect(lgSpan.style.width).toBe("32px");
    expect(lgSpan.style.height).toBe("22px");
  });

  it("uses flexShrink: 0 to prevent collapsing", () => {
    const { container } = render(<TierChip tier="T3" />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.flexShrink).toBe("0");
  });

  it("renders with default props (tier=T2, size=sm)", () => {
    const { container } = render(<TierChip />);
    expect(container.textContent).toBe("T2");
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.width).toBe("26px");
  });
});

// ─── Chip ───────────────────────────────────────────────────────────────────

describe("Chip", () => {
  it("renders the value text", () => {
    const { container } = render(<Chip value="12 skills" />);
    expect(container.textContent).toContain("12 skills");
  });

  it("renders label + value when label is provided", () => {
    const { container } = render(<Chip label="SKILLS" value="3" />);
    const text = container.textContent ?? "";
    expect(text).toContain("SKILLS");
    expect(text).toContain("3");
  });

  it("has border-radius 999 (pill shape)", () => {
    const { container } = render(<Chip value="test" />);
    const span = container.querySelector("span") as HTMLSpanElement;
    expect(span.style.borderRadius).toBe("999px");
  });

  it("soft mode applies accent background", () => {
    const { container: normal } = render(<Chip value="a" />);
    const { container: soft } = render(<Chip value="a" soft={true} accent="#2f9e6a" />);
    const normalSpan = normal.querySelector("span") as HTMLSpanElement;
    const softSpan = soft.querySelector("span") as HTMLSpanElement;
    // soft uses accent+1a hex, normal uses dark/light hardcoded
    expect(normalSpan.style.background).toBeTruthy();
    expect(softSpan.style.background).toBeTruthy();
    expect(normalSpan.style.background).not.toBe(softSpan.style.background);
  });
});

// ─── SectionLabel ───────────────────────────────────────────────────────────

describe("SectionLabel", () => {
  it("renders children text", () => {
    const { container } = render(<SectionLabel>Runtime config</SectionLabel>);
    expect(container.textContent).toContain("Runtime config");
  });

  it("renders right slot content when provided", () => {
    const { container } = render(
      <SectionLabel right={<button>Edit</button>}>Runtime config</SectionLabel>,
    );
    expect(container.textContent).toContain("Edit");
    expect(container.querySelector("button")).toBeTruthy();
  });

  it("renders without right slot", () => {
    const { container } = render(<SectionLabel>Runtime config</SectionLabel>);
    expect(container.querySelector("button")).toBeNull();
  });

  it("uses uppercase text transform", () => {
    const { container } = render(<SectionLabel>Runtime config</SectionLabel>);
    const div = container.querySelector("div") as HTMLDivElement;
    expect(div.style.textTransform).toBe("uppercase");
  });
});
