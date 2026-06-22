// @vitest-environment jsdom
/**
 * Tests for ValidationHint component.
 *
 * Covers: null/neutral render, error state (red ⚠ + message), valid state
 * (green ✓ + "Valid format"), ARIA role="alert" on error.
 */
import { afterEach, describe, it, expect } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { ValidationHint } from "../ValidationHint";

afterEach(cleanup);

describe("ValidationHint", () => {
  it("renders nothing when error is null and showValid is false", () => {
    const { container } = render(<ValidationHint error={null} showValid={false} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing when error is null and showValid is undefined", () => {
    const { container } = render(<ValidationHint error={null} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders error state with ⚠ icon and message", () => {
    render(<ValidationHint error="Key name must be UPPER_SNAKE_CASE" />);
    const el = screen.getByRole("alert");
    expect(el.textContent).toContain("⚠");
    expect(el.textContent).toContain("Key name must be UPPER_SNAKE_CASE");
  });

  it("renders valid state with ✓ and 'Valid format'", () => {
    render(<ValidationHint error={null} showValid />);
    const el = screen.getByText("Valid format");
    expect(el.textContent).toContain("✓");
  });

  it("prefers error over valid when both are set (error is not null)", () => {
    // ValidationHint checks error first; showValid is only rendered when error is falsy.
    render(<ValidationHint error="Some error" showValid />);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByText("Valid format")).toBeNull();
  });

  it("error alert has role='alert' for screen readers", () => {
    render(<ValidationHint error="Invalid format" />);
    expect(screen.getByRole("alert")).toBeTruthy();
  });
});
