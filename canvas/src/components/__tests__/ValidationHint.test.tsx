// @vitest-environment jsdom
/**
 * Tests for ValidationHint component.
 *
 * Covers: error state, valid state, neutral/hidden state,
 * aria-live for error, icon rendering.
 */
import React from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ValidationHint } from "../ui/ValidationHint";

describe("ValidationHint — error state", () => {
  it("renders error message when error is a non-null string", () => {
    render(<ValidationHint error="Invalid email address" />);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Invalid email address")).toBeTruthy();
  });

  it("includes the warning icon in error state", () => {
    render(<ValidationHint error="Too short" />);
    expect(screen.getByText(/⚠/)).toBeTruthy();
  });

  it("uses the error class on the paragraph element", () => {
    render(<ValidationHint error="Bad input" />);
    const el = screen.getByRole("alert");
    expect(el.className).toContain("validation-hint--error");
  });

  it("renders error even when showValid is true", () => {
    render(<ValidationHint error="Oops" showValid={true} />);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByText(/✓/)).toBeNull();
  });
});

describe("ValidationHint — valid state", () => {
  it("renders valid message when error is null and showValid is true", () => {
    render(<ValidationHint error={null} showValid={true} />);
    expect(screen.getByText("Valid format")).toBeTruthy();
  });

  it("includes the checkmark icon in valid state", () => {
    render(<ValidationHint error={null} showValid={true} />);
    expect(screen.getByText(/✓ Valid format/)).toBeTruthy();
  });

  it("uses the valid class on the paragraph element", () => {
    render(<ValidationHint error={null} showValid={true} />);
    const el = document.body.querySelector(".validation-hint--valid");
    expect(el).toBeTruthy();
  });

  it("renders nothing when error is null and showValid is false (default)", () => {
    const { container } = render(<ValidationHint error={null} />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when error is empty string", () => {
    const { container } = render(<ValidationHint error="" />);
    expect(container.textContent).toBe("");
  });
});

describe("ValidationHint — neutral / not-yet-validated", () => {
  it("renders nothing when error is null and showValid defaults to false", () => {
    const { container } = render(<ValidationHint error={null} />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when error is undefined", () => {
    // @ts-expect-error — testing runtime behavior with undefined
    const { container } = render(<ValidationHint error={undefined} />);
    expect(container.textContent).toBe("");
  });
});
