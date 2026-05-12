// @vitest-environment jsdom
/**
 * Tests for KeyValueField component.
 *
 * Covers: initial password type, onChange callback (including whitespace trim
 * on type), aria-label forwarding, disabled state, and auto-hide timer setup.
 */
import React from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { KeyValueField } from "../KeyValueField";

describe("KeyValueField — rendering", () => {
  afterEach(cleanup);

  it("renders input with type=password by default (secret hidden)", () => {
    render(<KeyValueField value="" onChange={vi.fn()} />);
    const input = screen.getByLabelText("Secret value");
    expect(input.getAttribute("type")).toBe("password");
  });

  it("passes custom aria-label to the input element", () => {
    render(<KeyValueField value="" onChange={vi.fn()} aria-label="API secret key" />);
    expect(screen.getByLabelText("API secret key")).toBeTruthy();
  });

  it("disables the input when disabled=true", () => {
    render(<KeyValueField value="secret" onChange={vi.fn()} disabled />);
    expect(screen.getByLabelText("Secret value").disabled).toBe(true);
  });

  it("renders with the current value", () => {
    render(<KeyValueField value="sk-test-key-123" onChange={vi.fn()} />);
    expect(screen.getByLabelText("Secret value").value).toBe("sk-test-key-123");
  });

  it("renders with the placeholder text", () => {
    render(<KeyValueField value="" onChange={vi.fn()} placeholder="Enter API key" />);
    expect(screen.getByLabelText("Secret value").getAttribute("placeholder")).toBe("Enter API key");
  });

  it("renders the RevealToggle child button", () => {
    render(<KeyValueField value="secret" onChange={vi.fn()} />);
    // KeyValueField renders exactly one button (the RevealToggle)
    expect(screen.getByRole("button")).toBeTruthy();
  });
});

describe("KeyValueField — onChange", () => {
  afterEach(cleanup);

  it("calls onChange with the new value when user types", () => {
    const onChange = vi.fn();
    render(<KeyValueField value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Secret value"), { target: { value: "new-value" } });
    expect(onChange).toHaveBeenCalledWith("new-value");
  });

  it("trims leading whitespace when user types with leading space", () => {
    const onChange = vi.fn();
    render(<KeyValueField value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Secret value"), { target: { value: "  trimmed" } });
    expect(onChange).toHaveBeenCalledWith("trimmed");
  });

  it("trims trailing whitespace when user types with trailing space", () => {
    const onChange = vi.fn();
    render(<KeyValueField value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Secret value"), { target: { value: "trimmed  " } });
    expect(onChange).toHaveBeenCalledWith("trimmed");
  });

  it("trims both sides when user types whitespace-surrounded value", () => {
    const onChange = vi.fn();
    render(<KeyValueField value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Secret value"), { target: { value: "  both sides  " } });
    expect(onChange).toHaveBeenCalledWith("both sides");
  });

  it("does not modify value with no whitespace", () => {
    const onChange = vi.fn();
    render(<KeyValueField value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Secret value"), { target: { value: "clean-value" } });
    expect(onChange).toHaveBeenCalledWith("clean-value");
  });
});

describe("KeyValueField — auto-hide timer setup", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("sets up a 30s setTimeout when the component mounts with a non-empty value", () => {
    const setTimeoutSpy = vi.spyOn(global, "setTimeout");
    render(<KeyValueField value="secret" onChange={vi.fn()} />);
    // No timer should be set initially (revealed=false by default)
    const callsBeforeInteraction = setTimeoutSpy.mock.calls.length;

    // Simulate reveal (click the only button)
    act(() => { fireEvent.click(screen.getByRole("button")); });

    // After reveal, a 30s timer should be set
    const timerCalls = setTimeoutSpy.mock.calls.filter(
      ([, delay]) => delay === 30_000,
    );
    expect(timerCalls.length).toBeGreaterThanOrEqual(1);
  });

  it("clears existing timer when a new toggle happens before auto-hide fires", () => {
    const clearTimeoutSpy = vi.spyOn(global, "clearTimeout");
    const timerObj = {}; // fake timer ID
    vi.spyOn(global, "setTimeout").mockImplementation((fn: () => void, delay: number) => {
      return timerObj;
    });
    render(<KeyValueField value="secret" onChange={vi.fn()} />);

    // First toggle — reveal
    act(() => { fireEvent.click(screen.getByRole("button")); });

    // Second toggle — hide (should clear the timer from first toggle)
    act(() => { fireEvent.click(screen.getByRole("button")); });

    // clearTimeout was called with the timer object
    expect(clearTimeoutSpy).toHaveBeenCalledWith(timerObj);
  });

  it("clears timer on unmount", () => {
    const clearTimeoutSpy = vi.spyOn(global, "clearTimeout");
    const { unmount } = render(<KeyValueField value="secret" onChange={vi.fn()} />);

    // Toggle reveal to start the timer
    act(() => { fireEvent.click(screen.getByRole("button")); });

    unmount();
    expect(clearTimeoutSpy).toHaveBeenCalled();
  });
});
