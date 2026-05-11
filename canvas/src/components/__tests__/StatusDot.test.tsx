// @vitest-environment jsdom
/**
 * Tests for StatusDot — the small coloured indicator rendered inside
 * workspace cards to convey runtime status (online/offline/degraded/etc.).
 *
 * Coverage:
 *   - Renders for every known status in STATUS_CONFIG
 *   - Unknown status falls back to bg-zinc-500
 *   - size prop (sm/md) applies the correct Tailwind dimension class
 *   - aria-hidden="true" and role="img" for accessibility
 *   - provisioning status carries motion-safe:animate-pulse for the pulsing effect
 *   - glow class applied when STATUS_CONFIG declares one
 */
import { afterEach, describe, expect, it } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import React from "react";

import { StatusDot } from "../StatusDot";

afterEach(cleanup);

describe("StatusDot — snapshot", () => {
  it("renders with online status", () => {
    render(<StatusDot status="online" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-emerald-400");
    expect(dot.className).toContain("shadow-emerald-400/50");
    expect(dot.getAttribute("aria-hidden")).toBe("true");
  });

  it("renders with offline status", () => {
    render(<StatusDot status="offline" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-zinc-500");
    // offline has no glow
    expect(dot.className).not.toContain("shadow-");
  });

  it("renders with degraded status", () => {
    render(<StatusDot status="degraded" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-amber-400");
    expect(dot.className).toContain("shadow-amber-400/50");
  });

  it("renders with failed status", () => {
    render(<StatusDot status="failed" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-red-400");
    expect(dot.className).toContain("shadow-red-400/50");
  });

  it("renders with paused status", () => {
    render(<StatusDot status="paused" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-indigo-400");
  });

  it("renders with not_configured status", () => {
    render(<StatusDot status="not_configured" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-amber-300");
    expect(dot.className).toContain("shadow-amber-300/50");
  });

  it("renders with provisioning status and pulsing animation", () => {
    render(<StatusDot status="provisioning" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-sky-400");
    expect(dot.className).toContain("motion-safe:animate-pulse");
    expect(dot.className).toContain("shadow-sky-400/50");
  });

  it("falls back to bg-zinc-500 for unknown status", () => {
    render(<StatusDot status="alien_artifact" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("bg-zinc-500");
  });
});

describe("StatusDot — size prop", () => {
  it("applies w-2 h-2 (sm, default)", () => {
    render(<StatusDot status="online" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("w-2");
    expect(dot.className).toContain("h-2");
  });

  it("applies w-2.5 h-2.5 (md)", () => {
    render(<StatusDot status="online" size="md" />);
    const dot = screen.getByRole("img", { hidden: true });
    expect(dot.className).toContain("w-2.5");
    expect(dot.className).toContain("h-2.5");
  });
});

describe("StatusDot — accessibility", () => {
  it("is aria-hidden so it doesn't pollute the accessibility tree", () => {
    render(<StatusDot status="online" />);
    expect(screen.getByRole("img", { hidden: true }).getAttribute("aria-hidden")).toBe("true");
  });
});
