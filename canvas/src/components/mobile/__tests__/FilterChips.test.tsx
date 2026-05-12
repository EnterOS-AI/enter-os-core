// @vitest-environment jsdom
/**
 * FilterChips — mobile agent filter toolbar.
 *
 * Per WCAG 2.1 AA / ARIA radio group pattern:
 *   - Container has role="toolbar" + aria-label
 *   - Each button has role="radio" + aria-checked
 *   - Icon spans have aria-hidden="true"
 *   - Only one radio can be checked at a time (single-select filter)
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { FilterChips, type AgentFilter } from "../components";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

const defaultCounts = { all: 12, online: 8, issue: 2, paused: 2 };

// ─── Render ───────────────────────────────────────────────────────────────────

describe("FilterChips — render", () => {
  it("renders 4 filter buttons", () => {
    render(<FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const buttons = document.querySelectorAll('[role="radio"]');
    expect(buttons.length).toBe(4);
  });

  it("container has role=toolbar and aria-label", () => {
    render(<FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const toolbar = document.querySelector('[role="toolbar"]');
    expect(toolbar).toBeTruthy();
    expect(toolbar?.getAttribute("aria-label")).toBe("Filter agents");
  });

  it("each button has role=radio", () => {
    render(<FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const buttons = document.querySelectorAll('[role="radio"]');
    buttons.forEach((btn) => {
      expect(btn.getAttribute("role")).toBe("radio");
    });
  });

  it("active filter has aria-checked=true, others false", () => {
    render(<FilterChips value="issue" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const buttons = document.querySelectorAll('[role="radio"]');
    buttons.forEach((btn) => {
      const label = btn.textContent ?? "";
      if (label.startsWith("Issues")) {
        expect(btn.getAttribute("aria-checked")).toBe("true");
      } else {
        expect(btn.getAttribute("aria-checked")).toBe("false");
      }
    });
  });

  it("count spans have aria-hidden=true", () => {
    render(<FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const hidden = document.querySelectorAll('[aria-hidden="true"]');
    // Each chip has one count span marked aria-hidden
    expect(hidden.length).toBeGreaterThanOrEqual(4);
  });
});

// ─── Interaction ─────────────────────────────────────────────────────────────

describe("FilterChips — interaction", () => {
  it("calls onChange with correct filter id when clicked", () => {
    const onChange = vi.fn();
    render(<FilterChips value="all" onChange={onChange} dark={false} counts={defaultCounts} />);
    const buttons = document.querySelectorAll('[role="radio"]');
    const onlineBtn = Array.from(buttons).find((b) => b.textContent?.startsWith("Online")) as Element;
    fireEvent.click(onlineBtn);
    expect(onChange).toHaveBeenCalledWith("online");
  });

  it("calls onChange when the already-active filter is clicked (component does not guard)", () => {
    const onChange = vi.fn();
    render(<FilterChips value="all" onChange={onChange} dark={false} counts={defaultCounts} />);
    const buttons = document.querySelectorAll('[role="radio"]');
    const allBtn = Array.from(buttons).find((b) => b.textContent?.startsWith("All")) as Element;
    fireEvent.click(allBtn);
    // Component calls onChange even for the already-active filter;
    // the guard belongs at the consumer level (MobileHome) if needed.
    expect(onChange).toHaveBeenCalledWith("all");
  });

  it("updating value prop changes aria-checked", () => {
    const { rerender } = render(
      <FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />,
    );
    const allBtn = document.querySelector('[id="filter-all"]') as Element;
    expect(allBtn.getAttribute("aria-checked")).toBe("true");

    rerender(<FilterChips value="paused" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    expect(allBtn.getAttribute("aria-checked")).toBe("false");
    const pausedBtn = document.querySelector('[id="filter-paused"]') as Element;
    expect(pausedBtn.getAttribute("aria-checked")).toBe("true");
  });

  it("all filter labels are present", () => {
    render(<FilterChips value="all" onChange={vi.fn()} dark={false} counts={defaultCounts} />);
    const texts = Array.from(document.querySelectorAll('[role="radio"]')).map((b) =>
      b.textContent?.trim(),
    );
    expect(texts.some((t) => t?.startsWith("All"))).toBe(true);
    expect(texts.some((t) => t?.startsWith("Online"))).toBe(true);
    expect(texts.some((t) => t?.startsWith("Issues"))).toBe(true);
    expect(texts.some((t) => t?.startsWith("Paused"))).toBe(true);
  });
});
