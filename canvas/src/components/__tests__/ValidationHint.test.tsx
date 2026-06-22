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
    const { container } = render(<ValidationHint error="Invalid email address" />);
    const el = container.querySelector('[role="alert"]');
    expect(el).toBeTruthy();
    expect(el?.textContent).toContain("Invalid email address");
  });

  it("includes the warning icon in error state", () => {
    render(<ValidationHint error="Too short" />);
    // The warning icon is a separate span with aria-hidden
    const container = document.body.querySelector('[role="alert"]');
    expect(container?.innerHTML).toContain("⚠");
  });

  it("uses the error class on the paragraph element", () => {
    render(<ValidationHint error="Bad input" />);
    const el = document.body.querySelector(".validation-hint--error");
    expect(el).toBeTruthy();
  });

  it("renders error even when showValid is true", () => {
    const { container } = render(<ValidationHint error="Oops" showValid={true} />);
    const alertEl = container.querySelector('[role="alert"]');
    expect(alertEl).toBeTruthy();
    // No ✓ checkmark in error state
    expect(container.querySelector('[role="status"]')).toBeNull();
  });
});

describe("ValidationHint — valid state", () => {
  it("renders valid message when error is null and showValid is true", () => {
    const { container } = render(<ValidationHint error={null} showValid={true} />);
    expect(container.textContent).toContain("Valid format");
  });

  it("includes the checkmark icon in valid state", () => {
    render(<ValidationHint error={null} showValid={true} />);
    // The valid hint contains a span with ✓ followed by "Valid format"
    const container = document.body.querySelector(".validation-hint--valid");
    expect(container?.innerHTML).toContain("✓");
  });

  it("uses the valid class on the paragraph element", () => {
    const { container } = render(<ValidationHint error={null} showValid={true} />);
    const el = container.querySelector(".validation-hint--valid");
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
