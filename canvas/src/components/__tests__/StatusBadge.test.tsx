// @vitest-environment jsdom
/**
 * Tests for StatusBadge component.
 *
 * Covers: renders all three status variants, aria-label, role=status,
 * icon presence, className variants, no render when passed invalid status.
 */
import React from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusBadge } from "../ui/StatusBadge";

describe("StatusBadge — render", () => {
  it("renders verified status with ✓ icon", () => {
    render(<StatusBadge status="verified" />);
    const badge = screen.getByRole("status");
    expect(badge.textContent).toBe("✓");
    expect(badge.getAttribute("aria-label")).toBe("Connection status: verified");
  });

  it("renders invalid status with ✗ icon", () => {
    render(<StatusBadge status="invalid" />);
    const badge = screen.getByRole("status");
    expect(badge.textContent).toBe("✗");
    expect(badge.getAttribute("aria-label")).toBe("Connection status: invalid");
  });

  it("renders unverified status with ○ icon", () => {
    render(<StatusBadge status="unverified" />);
    const badge = screen.getByRole("status");
    expect(badge.textContent).toBe("○");
    expect(badge.getAttribute("aria-label")).toBe("Connection status: unverified");
  });

  it("has role=status on the badge element", () => {
    render(<StatusBadge status="verified" />);
    expect(screen.getByRole("status")).toBeTruthy();
  });

  it("includes the config className on the rendered element", () => {
    render(<StatusBadge status="verified" />);
    const badge = screen.getByRole("status");
    expect(badge.className).toContain("status-badge--valid");
  });

  it("includes status-badge--invalid class for invalid status", () => {
    render(<StatusBadge status="invalid" />);
    const badge = screen.getByRole("status");
    expect(badge.className).toContain("status-badge--invalid");
  });

  it("includes status-badge--unverified class for unverified status", () => {
    render(<StatusBadge status="unverified" />);
    const badge = screen.getByRole("status");
    expect(badge.className).toContain("status-badge--unverified");
  });
});
