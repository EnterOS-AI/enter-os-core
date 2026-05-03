// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { Toaster, showToast } from "../Toaster";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("Toaster keyboard a11y", () => {
  it("Esc dismisses the most recent toast", () => {
    render(<Toaster />);
    act(() => {
      showToast("first", "info");
      showToast("second", "info");
    });
    expect(screen.getByText("first")).toBeTruthy();
    expect(screen.getByText("second")).toBeTruthy();

    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(screen.queryByText("second")).toBeNull();
    expect(screen.getByText("first")).toBeTruthy();
  });

  it("Esc dismisses persistent error toasts", () => {
    render(<Toaster />);
    act(() => {
      showToast("boom", "error");
    });
    expect(screen.getByText("boom")).toBeTruthy();

    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(screen.queryByText("boom")).toBeNull();
  });

  it("Esc with no toasts is a no-op", () => {
    render(<Toaster />);
    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    // no throw, nothing rendered
    expect(screen.queryAllByRole("button", { name: "Dismiss notification" })).toHaveLength(0);
  });

  it("dismiss button has accessible label and is keyboard reachable", () => {
    render(<Toaster />);
    act(() => {
      showToast("hi", "info");
    });
    const btn = screen.getByRole("button", { name: "Dismiss notification" });
    expect(btn).toBeTruthy();
    // Native <button> defaults to keyboard-focusable; explicit assertion guards
    // against a future regression where someone adds tabindex=-1.
    expect(btn.getAttribute("tabindex")).not.toBe("-1");
  });

  it("dismiss button click removes that specific toast", () => {
    render(<Toaster />);
    act(() => {
      showToast("a", "info");
      showToast("b", "info");
    });
    const buttons = screen.getAllByRole("button", { name: "Dismiss notification" });
    expect(buttons).toHaveLength(2);

    // Click the first dismiss → "a" goes away, "b" stays
    act(() => {
      fireEvent.click(buttons[0]);
    });
    expect(screen.queryByText("a")).toBeNull();
    expect(screen.getByText("b")).toBeTruthy();
  });
});
