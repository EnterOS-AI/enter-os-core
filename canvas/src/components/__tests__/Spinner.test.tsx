// @vitest-environment jsdom
/**
 * Tests for Spinner component.
 *
 * Covers: sm/md/lg size classes, aria-hidden, motion-safe animate-spin class.
 *
 * NOTE: SVG elements use SVGAnimatedString for className (not a plain string),
 * so we use getAttribute("class") instead of className for assertions.
 */
import React from "react";
import { render, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Spinner } from "../Spinner";

afterEach(cleanup);

function getSvgClass(r: ReturnType<typeof render>): string {
  const svg = r.container.querySelector("svg");
  if (!svg) throw new Error("No SVG found");
  return svg.getAttribute("class") ?? "";
}

describe("Spinner — size variants", () => {
  it("renders with sm size class", () => {
    const r = render(<Spinner size="sm" />);
    expect(getSvgClass(r)).toContain("w-3");
    expect(getSvgClass(r)).toContain("h-3");
  });

  it("renders with md size class (default)", () => {
    const r = render(<Spinner size="md" />);
    expect(getSvgClass(r)).toContain("w-4");
    expect(getSvgClass(r)).toContain("h-4");
  });

  it("renders with lg size class", () => {
    const r = render(<Spinner size="lg" />);
    expect(getSvgClass(r)).toContain("w-5");
    expect(getSvgClass(r)).toContain("h-5");
  });

  it("defaults to md size when no size prop given", () => {
    const r = render(<Spinner />);
    expect(getSvgClass(r)).toContain("w-4");
    expect(getSvgClass(r)).toContain("h-4");
  });

  it("has aria-hidden=true so screen readers skip it", () => {
    const r = render(<Spinner />);
    const svg = r.container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("includes the motion-safe:animate-spin class for CSS animation", () => {
    expect(getSvgClass(render(<Spinner />))).toContain("motion-safe:animate-spin");
  });

  it("renders exactly one SVG element", () => {
    const { container } = render(<Spinner />);
    expect(container.querySelectorAll("svg").length).toBe(1);
  });
});