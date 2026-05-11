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
 *
 * NOTE: role="img" with aria-hidden="true" is invisible to getByRole in jsdom
 * (Testing Library only finds accessible elements by default). Use
 * container.querySelector with getAttribute instead.
 */
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import React from "react";

import { StatusDot } from "../StatusDot";

function getDot(status: string, size?: "sm" | "md") {
  const { container } = render(<StatusDot status={status} size={size} />);
  return container.querySelector("[role=img]") as HTMLElement;
}

function getAttr(el: HTMLElement | null, name: string) {
  return el?.getAttribute(name) ?? "";
}

describe("StatusDot — snapshot", () => {
  it("renders with online status", () => {
    const { container } = render(<StatusDot status="online" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-emerald-400")).toBe(true);
    expect(dot.classList.contains("shadow-emerald-400/50")).toBe(true);
    expect(dot.getAttribute("aria-hidden")).toBe("true");
  });

  it("renders with offline status", () => {
    const { container } = render(<StatusDot status="offline" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-zinc-500")).toBe(true);
    expect(dot.classList.contains("shadow-")).toBe(false);
  });

  it("renders with degraded status", () => {
    const { container } = render(<StatusDot status="degraded" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-amber-400")).toBe(true);
    expect(dot.classList.contains("shadow-amber-400/50")).toBe(true);
  });

  it("renders with failed status", () => {
    const { container } = render(<StatusDot status="failed" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-red-400")).toBe(true);
    expect(dot.classList.contains("shadow-red-400/50")).toBe(true);
  });

  it("renders with paused status", () => {
    const { container } = render(<StatusDot status="paused" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-indigo-400")).toBe(true);
  });

  it("renders with not_configured status", () => {
    const { container } = render(<StatusDot status="not_configured" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-amber-300")).toBe(true);
    expect(dot.classList.contains("shadow-amber-300/50")).toBe(true);
  });

  it("renders with provisioning status and pulsing animation", () => {
    const { container } = render(<StatusDot status="provisioning" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-sky-400")).toBe(true);
    expect(dot.classList.contains("motion-safe:animate-pulse")).toBe(true);
    expect(dot.classList.contains("shadow-sky-400/50")).toBe(true);
  });

  it("falls back to bg-zinc-500 for unknown status", () => {
    const { container } = render(<StatusDot status="alien_artifact" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("bg-zinc-500")).toBe(true);
  });
});

describe("StatusDot — size prop", () => {
  it("applies w-2 h-2 (sm, default)", () => {
    const { container } = render(<StatusDot status="online" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("w-2")).toBe(true);
    expect(dot.classList.contains("h-2")).toBe(true);
  });

  it("applies w-2.5 h-2.5 (md)", () => {
    const { container } = render(<StatusDot status="online" size="md" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.classList.contains("w-2.5")).toBe(true);
    expect(dot.classList.contains("h-2.5")).toBe(true);
  });
});

describe("StatusDot — accessibility", () => {
  it("is aria-hidden so it doesn't pollute the accessibility tree", () => {
    const { container } = render(<StatusDot status="online" />);
    const dot = container.querySelector('[role="img"]') as HTMLElement;
    expect(dot.getAttribute("aria-hidden")).toBe("true");
  });
});
