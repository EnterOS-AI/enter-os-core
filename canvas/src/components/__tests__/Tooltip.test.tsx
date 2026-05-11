// @vitest-environment jsdom
/**
 * Tests for Tooltip component.
 *
 * Covers: portal rendering, 400ms hover delay, keyboard focus reveal,
 * Esc dismiss, no render when text is empty.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { Tooltip } from "../Tooltip";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("Tooltip — render", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  it("renders children without showing tooltip on mount", () => {
    render(
      <Tooltip text="Hello world">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    expect(screen.getByRole("button", { name: "Hover me" })).toBeTruthy();
    // Tooltip portal is not yet in the DOM (no timer fires on mount)
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("does not render the tooltip portal when text is empty string", () => {
    render(
      <Tooltip text="">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    // Move mouse over trigger
    fireEvent.mouseEnter(screen.getByRole("button"));
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("mounts the tooltip into a portal attached to document.body", () => {
    render(
      <Tooltip text="Portal tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    // Simulate mouse enter → 400ms delay → tooltip renders
    fireEvent.mouseEnter(screen.getByRole("button"));
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(document.body.querySelector('[role="tooltip"]')).toBeTruthy();
  });
});

describe("Tooltip — hover delay", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("does NOT show tooltip before the 400ms delay expires", () => {
    render(
      <Tooltip text="Delayed tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    fireEvent.mouseEnter(screen.getByRole("button"));
    act(() => {
      vi.advanceTimersByTime(300);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("shows tooltip after 400ms hover delay", () => {
    render(
      <Tooltip text="Delayed tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    fireEvent.mouseEnter(screen.getByRole("button"));
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();
  });

  it("hides tooltip immediately on mouse leave (clears pending timer)", () => {
    render(
      <Tooltip text="Cleared tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    fireEvent.mouseEnter(btn);
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    fireEvent.mouseLeave(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    // Still not shown because mouseLeave cancelled the timer
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("does not show on a second mouseEnter after mouseLeave", () => {
    render(
      <Tooltip text="Re-show tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    fireEvent.mouseEnter(btn);
    fireEvent.mouseLeave(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    // Re-enter
    fireEvent.mouseEnter(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();
  });
});

describe("Tooltip — keyboard focus reveal", () => {
  it("shows tooltip on focus without needing the hover timer", () => {
    vi.useFakeTimers();
    render(
      <Tooltip text="Keyboard tip">
        <button type="button">Focus me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    // No timer needed — onFocus shows immediately
    act(() => {
      btn.focus();
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();
    vi.useRealTimers();
  });

  it("hides tooltip on blur", () => {
    vi.useFakeTimers();
    render(
      <Tooltip text="Blur tip">
        <button type="button">Focus me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    act(() => {
      btn.focus();
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();

    act(() => {
      btn.blur();
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
    vi.useRealTimers();
  });
});

describe("Tooltip — Esc dismiss (WCAG 1.4.13)", () => {
  it("dismisses tooltip on Escape without blurring the trigger", () => {
    vi.useFakeTimers();
    render(
      <Tooltip text="Esc dismiss tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    fireEvent.mouseEnter(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();
    expect(document.activeElement).toBe(btn);

    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
    // Trigger is still focused (Esc dismisses tooltip but does not blur)
    expect(document.activeElement).toBe(btn);
    vi.useRealTimers();
  });

  it("does nothing on non-Escape keys while tooltip is open", () => {
    vi.useFakeTimers();
    render(
      <Tooltip text="Non-Escape key">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    fireEvent.mouseEnter(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(screen.queryByRole("tooltip")).toBeTruthy();

    act(() => {
      fireEvent.keyDown(window, { key: "Enter" });
    });
    // Tooltip still visible
    expect(screen.queryByRole("tooltip")).toBeTruthy();
    vi.useRealTimers();
  });
});

describe("Tooltip — aria-describedby", () => {
  it("associates tooltip with the trigger via aria-describedby", () => {
    render(
      <Tooltip text="Associated tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    // The aria-describedby is on the wrapper div, not the button child
    const btn = screen.getByRole("button");
    const wrapper = btn.parentElement as HTMLElement;
    const describedBy = wrapper.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    // The describedby id matches the tooltip id
    expect(document.getElementById(describedBy!)).toBeTruthy();
  });
});
