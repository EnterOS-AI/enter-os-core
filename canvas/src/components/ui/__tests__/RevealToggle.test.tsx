// @vitest-environment jsdom
/**
 * Tests for RevealToggle component.
 *
 * Covers: eye-icon (hidden) vs eye-off-icon (revealed), onToggle callback,
 * aria-label (default + custom), title attribute.
 */
import { afterEach, describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { RevealToggle } from "../RevealToggle";

afterEach(cleanup);

describe("RevealToggle", () => {
  it("renders as a button", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button")).toBeTruthy();
  });

  it("uses default aria-label when not provided", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("aria-label")).toBe("Toggle reveal secret");
  });

  it("uses custom aria-label when provided", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} label="Show password" />);
    expect(screen.getByRole("button").getAttribute("aria-label")).toBe("Show password");
  });

  it('title is "Hide value" when revealed', () => {
    render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("title")).toBe("Hide value");
  });

  it('title is "Show value" when hidden', () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    expect(screen.getByRole("button").getAttribute("title")).toBe("Show value");
  });

  it("calls onToggle when clicked (revealed=true → should hide)", () => {
    const onToggle = vi.fn();
    render(<RevealToggle revealed={true} onToggle={onToggle} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it("calls onToggle when clicked (revealed=false → should show)", () => {
    const onToggle = vi.fn();
    render(<RevealToggle revealed={false} onToggle={onToggle} />);
    fireEvent.click(screen.getByRole("button"));
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it("renders the eye-open SVG (hide icon) when revealed=false", () => {
    render(<RevealToggle revealed={false} onToggle={vi.fn()} />);
    const btn = screen.getByRole("button");
    // The eye SVG contains a circle element; eye-off has a strikethrough line
    expect(btn.querySelector("circle")).toBeTruthy();
    expect(btn.querySelectorAll("line")).toHaveLength(0);
  });

  it("renders the eye-off SVG (show icon) when revealed=true", () => {
    render(<RevealToggle revealed={true} onToggle={vi.fn()} />);
    const btn = screen.getByRole("button");
    // EyeOffIcon has a line (strikethrough) through the eye
    expect(btn.querySelectorAll("line")).toHaveLength(1);
  });
});
