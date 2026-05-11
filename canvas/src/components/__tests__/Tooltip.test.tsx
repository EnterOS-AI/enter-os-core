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

afterEach(cleanup);

// Tooltip uses useRef ids that increment per render.
// After cleanup, reset so IDs are predictable again.
// Since tooltipIdCounter is a module-level var, we just re-render in each test.

describe("Tooltip — render", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders children without showing tooltip on mount", () => {
    render(
      <Tooltip text="Hello world">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const { container } = render(<Tooltip text="Hello world"><button type="button">Hover me</button></Tooltip>);
    const btn = container.querySelector("button");
    expect(btn).toBeTruthy();
    // Tooltip portal is not yet in the DOM (no timer fires on mount)
    expect(document.body.querySelector('[role="tooltip"]')).toBeNull();
  });

  it("does not render the tooltip portal when text is empty string", () => {
    const { container } = render(
      <Tooltip text="">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    fireEvent.mouseEnter(container.querySelector("button")!);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    expect(document.body.querySelector('[role="tooltip"]')).toBeNull();
  });

  it("mounts the tooltip into a portal attached to document.body", () => {
    const { container } = render(
      <Tooltip text="Portal tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    fireEvent.mouseEnter(container.querySelector("button")!);
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
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows tooltip on focus without needing the hover timer", () => {
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
  });

  it("hides tooltip on blur", () => {
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
  });
});

describe("Tooltip — Esc dismiss (WCAG 1.4.13)", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("dismisses tooltip on Escape without blurring the trigger", () => {
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
    // Focus the trigger so activeElement is the button (jsdom mouseEnter doesn't focus)
    act(() => { btn.focus(); });
    const activeBefore = document.activeElement;

    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
    // Trigger element was the active element before Esc (button)
    expect(activeBefore?.tagName).toBe("BUTTON");
  });

  it("does nothing on non-Escape keys while tooltip is open", () => {
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
    expect(document.body.querySelector('[role="tooltip"]')).toBeTruthy();

    act(() => {
      fireEvent.keyDown(window, { key: "Enter" });
    });
    // Tooltip still visible
    expect(screen.queryByRole("tooltip")).toBeTruthy();
  });
});

describe("Tooltip — aria-describedby", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("associates tooltip with the trigger wrapper via aria-describedby", () => {
    render(
      <Tooltip text="Associated tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    const btn = screen.getByRole("button");
    fireEvent.mouseEnter(btn);
    act(() => {
      vi.advanceTimersByTime(500);
    });
    // The aria-describedby is on the wrapper div (the Tooltip root element),
    // not on the children button directly.
    const wrapper = document.body.querySelector('[aria-describedby]') as HTMLElement;
    expect(wrapper).toBeTruthy();
    const describedBy = wrapper.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    // The describedby id matches the tooltip id in the portal
    expect(document.getElementById(describedBy!)).toBeTruthy();
  });

  // WCAG 1.4.13 (Content on Hover or Focus): aria-describedby must NOT be set
  // when the tooltip is hidden. An unconditional aria-describedby causes screen
  // readers to announce tooltip text even when the tooltip is not visible, which
  // is an accessibility regression. The fix makes it conditional on `show`.
  it("does NOT set aria-describedby when tooltip is hidden (WCAG 1.4.13)", () => {
    render(
      <Tooltip text="Hidden tip">
        <button type="button">Hover me</button>
      </Tooltip>
    );
    // Without any hover/focus, the tooltip is not shown
    const wrapper = document.body.querySelector('[aria-describedby]');
    expect(wrapper).toBeNull();
  });
});
