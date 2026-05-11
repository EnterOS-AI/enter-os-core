// @vitest-environment jsdom
/**
 * Tests for Spinner component.
 *
 * Covers: sm/md/lg size classes, aria-hidden, motion-safe animate-spin class.
 */
import React from "react";
import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Spinner } from "../Spinner";

describe("Spinner — size variants", () => {
  // Use getAttribute("class") instead of .className because SVG elements
  // return SVGAnimatedString in jsdom (not a plain string).
  it("renders with sm size class", () => {
    const { container } = render(<Spinner size="sm" />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    // SVG elements use SVGAnimatedString for className — use classList instead
    expect(svg!.classList.contains("w-3")).toBe(true);
    expect(svg!.classList.contains("h-3")).toBe(true);
  });

  it("renders with md size class (default)", () => {
    const { container } = render(<Spinner size="md" />);
    const svg = container.querySelector("svg");
    expect(svg?.classList.contains("w-4")).toBe(true);
    expect(svg?.classList.contains("h-4")).toBe(true);
  });

  it("renders with lg size class", () => {
    const { container } = render(<Spinner size="lg" />);
    const svg = container.querySelector("svg");
    expect(svg?.classList.contains("w-5")).toBe(true);
    expect(svg?.classList.contains("h-5")).toBe(true);
  });

  it("defaults to md size when no size prop given", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.classList.contains("w-4")).toBe(true);
    expect(svg?.classList.contains("h-4")).toBe(true);
  });

  it("has aria-hidden=true so screen readers skip it", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("includes the motion-safe:animate-spin class for CSS animation", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.classList.contains("motion-safe:animate-spin")).toBe(true);
  });

  it("renders exactly one SVG element", () => {
    const { container } = render(<Spinner />);
    expect(container.querySelectorAll("svg").length).toBe(1);
  });
});