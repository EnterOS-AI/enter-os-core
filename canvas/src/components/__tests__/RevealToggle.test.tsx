// @vitest-environment jsdom
/**
 * Tests for RevealToggle component.
 *
 * Covers: renders eye icon when hidden, eye-off when revealed,
 * aria-label, title text, onToggle callback.
 */
import React from "react";
import { render, screen, fireEvent } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { RevealToggle } from "../ui/RevealToggle";

describe("RevealToggle — render", () => {
  it("renders a button element", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button")).toBeTruthy();
  });

  it("uses the provided aria-label", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} label="Show password" />);
    expect(screen.getByRole("button").getAttribute("aria-label")).toBe("Show password");
  });

  it("uses default aria-label when label prop is omitted", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("aria-label")).toBe("Toggle visibility");
  });

  it("has title 'Show value' when revealed=false", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("title")).toBe("Show value");
  });

  it("has title 'Hide value' when revealed=true", () => {
    render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("title")).toBe("Hide value");
  });
});

describe("RevealToggle — interaction", () => {
  it("calls onToggle when clicked", () => {
    const onToggle = vi.fn();
    render(<RevealToggle revealed={false} onToggle={onToggle} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it("renders EyeIcon (eye SVG) when revealed=false", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    // Eye icon has a circle path for the eye
    expect(container.innerHTML).toContain("M1 12s4-8 11-8");
  });

  it("renders EyeOffIcon (eye-off SVG) when revealed=true", () => {
    const { container } = render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    // Eye-off has a diagonal line
    expect(container.innerHTML).toContain("x1");
    expect(container.innerHTML).toContain("y2");
  });
});
