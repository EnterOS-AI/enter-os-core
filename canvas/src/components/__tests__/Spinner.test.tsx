// @vitest-environment jsdom
/**
 * Tests for Spinner component.
 *
 * Covers: sm/md/lg size classes, aria-hidden, motion-safe animate-spin class.
 */
import React from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Spinner } from "../Spinner";

describe("Spinner — size variants", () => {
  it("renders with sm size class", () => {
    const { container } = render(<Spinner size="sm" />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    expect(svg?.className).toContain("w-3");
    expect(svg?.className).toContain("h-3");
  });

  it("renders with md size class (default)", () => {
    const { container } = render(<Spinner size="md" />);
    const svg = container.querySelector("svg");
    expect(svg?.className).toContain("w-4");
    expect(svg?.className).toContain("h-4");
  });

  it("renders with lg size class", () => {
    const { container } = render(<Spinner size="lg" />);
    const svg = container.querySelector("svg");
    expect(svg?.className).toContain("w-5");
    expect(svg?.className).toContain("h-5");
  });

  it("defaults to md size when no size prop given", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.className).toContain("w-4");
    expect(svg?.className).toContain("h-4");
  });

  it("has aria-hidden=true so screen readers skip it", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("includes the motion-safe:animate-spin class for CSS animation", () => {
    const { container } = render(<Spinner />);
    const svg = container.querySelector("svg");
    expect(svg?.className).toContain("motion-safe:animate-spin");
  });

  it("renders exactly one SVG element", () => {
    const { container } = render(<Spinner />);
    expect(container.querySelectorAll("svg").length).toBe(1);
  });
});
