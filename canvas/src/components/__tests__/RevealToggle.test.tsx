// @vitest-environment jsdom
/**
 * Tests for RevealToggle component.
 *
 * Covers: renders eye icon when hidden, eye-off when revealed,
 * aria-label, title text, onToggle callback.
 */
import React from "react";
import { render, fireEvent, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { RevealToggle } from "../ui/RevealToggle";

describe("RevealToggle — render", () => {
  // Scope all queries to container to avoid button ambiguity from other
  // components in the shared jsdom environment.
  it("renders a button element", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(container.querySelector("button")).toBeTruthy();
  });

  it("uses the provided aria-label", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} label="Show password" />);
    const btn = container.querySelector("button") as HTMLButtonElement;
    expect(btn.getAttribute("aria-label")).toBe("Show password");
  });

  it("uses default aria-label when label prop is omitted", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    const btn = container.querySelector("button") as HTMLButtonElement;
    expect(btn.getAttribute("aria-label")).toBe("Toggle reveal secret");
  });

  it("has title 'Show value' when revealed=false", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    const btn = container.querySelector("button") as HTMLButtonElement;
    expect(btn.getAttribute("title")).toBe("Show value");
  });

  it("has title 'Hide value' when revealed=true", () => {
    const { container } = render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    const btn = container.querySelector("button") as HTMLButtonElement;
    expect(btn.getAttribute("title")).toBe("Hide value");
  });
});

describe("RevealToggle — interaction", () => {
  it("calls onToggle when clicked", () => {
    const onToggle = vi.fn();
    const { container } = render(<RevealToggle revealed={false} onToggle={onToggle} />);
    const btn = container.querySelector("button") as HTMLButtonElement;
    fireEvent.click(btn);
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it("renders EyeIcon (eye SVG) when revealed=false", () => {
    const { container } = render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    expect(container.innerHTML).toContain("M1 12s4-8 11-8");
  });

  it("renders EyeOffIcon (eye-off SVG) when revealed=true", () => {
    const { container } = render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    expect(container.innerHTML).toContain("x1");
    expect(container.innerHTML).toContain("y2");
  });
});
