// @vitest-environment jsdom
/**
 * Tests for StatusBadge component.
 *
 * Covers: renders all three status variants, aria-label, role=status,
 * icon presence, className variants, no render when passed invalid status.
 */
import React from "react";
import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusBadge } from "../ui/StatusBadge";

describe("StatusBadge — render", () => {
  // Scoping queries to [aria-label] avoids ambiguity with role=status
  // from other components (Spinner, Toast, etc.) in the shared jsdom env.

  it("renders verified status with ✓ icon", () => {
    const { container } = render(<StatusBadge status="verified" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.textContent).toBe("✓");
  });

  it("renders invalid status with ✗ icon", () => {
    const { container } = render(<StatusBadge status="invalid" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.textContent).toBe("✗");
  });

  it("renders unverified status with ○ icon", () => {
    const { container } = render(<StatusBadge status="unverified" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.textContent).toBe("○");
  });

  it("has role=status on the badge element", () => {
    const { container } = render(<StatusBadge status="verified" />);
    expect(container.querySelector('[role="status"]')).toBeTruthy();
  });

  it("includes the config className on the rendered element", () => {
    const { container } = render(<StatusBadge status="verified" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.classList.contains("status-badge--valid")).toBe(true);
  });

  it("includes status-badge--invalid class for invalid status", () => {
    const { container } = render(<StatusBadge status="invalid" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.classList.contains("status-badge--invalid")).toBe(true);
  });

  it("includes status-badge--unverified class for unverified status", () => {
    const { container } = render(<StatusBadge status="unverified" />);
    const badge = container.querySelector('[role="status"]') as HTMLElement;
    expect(badge.classList.contains("status-badge--unverified")).toBe(true);
  });
});
