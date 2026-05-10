// @vitest-environment jsdom
/**
 * Tests for TestConnectionButton component.
 *
 * Covers: all 4 states (idle/testing/success/failure), button disabled
 * during testing, disabled when secretValue empty, error detail display,
 * auto-reset to idle after 3s (success) and 5s (failure), onResult callback.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TestConnectionButton } from "../ui/TestConnectionButton";
import type { SecretGroup } from "@/types/secrets";

// ─── Mock validateSecret ──────────────────────────────────────────────────────

const mockValidateSecret = vi.fn();
vi.mock("@/lib/api/secrets", () => ({
  validateSecret: mockValidateSecret,
}));

// SecretGroup is a string literal type: 'github' | 'anthropic' | 'openrouter' | 'custom'
const toGroup = (id: string): SecretGroup => id as SecretGroup;

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("TestConnectionButton — render", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    mockValidateSecret.mockReset();
  });

  it("renders 'Test connection' button in idle state", () => {
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);
    expect(screen.getByRole("button", { name: "Test connection" })).toBeTruthy();
  });

  it("disables button when secretValue is empty", () => {
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="" />);
    expect(screen.getByRole("button").getAttribute("disabled")).toBeTruthy();
  });

  it("enables button when secretValue is non-empty", () => {
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-test" />);
    expect(screen.getByRole("button").getAttribute("disabled")).toBeFalsy();
  });
});

describe("TestConnectionButton — state machine", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    mockValidateSecret.mockReset();
  });

  it("shows 'Testing…' while validateSecret is pending", async () => {
    mockValidateSecret.mockImplementation(() => new Promise(() => {})); // never resolves
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);

    fireEvent.click(screen.getByRole("button"));

    // Button should show testing label and be disabled
    expect(screen.getByRole("button", { name: "Testing…" }).getAttribute("disabled")).toBeTruthy();
  });

  it("shows 'Connected ✓' on success", async () => {
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });

    expect(screen.getByRole("button", { name: "Connected ✓" })).toBeTruthy();
  });

  it("shows 'Test failed' on validation failure", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false, error: "Invalid key format" });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="bad-key" />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });

    expect(screen.getByRole("button", { name: "Test failed" })).toBeTruthy();
  });

  it("shows error detail when validation returns invalid with message", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false, error: "Permission denied" });
    render(<TestConnectionButton provider={toGroup("github")} secretValue="ghp_xxx" />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Permission denied")).toBeTruthy();
  });

  it("shows generic error message on unexpected exception", async () => {
    mockValidateSecret.mockRejectedValue(new Error("timeout"));
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush */ });

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/timeout/i)).toBeTruthy();
  });
});

describe("TestConnectionButton — auto-reset", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    mockValidateSecret.mockReset();
  });

  it("resets to idle after 3 seconds on success", async () => {
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });
    expect(screen.getByRole("button", { name: "Connected ✓" })).toBeTruthy();

    act(() => { vi.advanceTimersByTime(3000); });
    await act(async () => { /* flush */ });

    expect(screen.getByRole("button", { name: "Test connection" })).toBeTruthy();
  });

  it("resets to idle after 5 seconds on failure", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false, error: "Bad key" });
    render(<TestConnectionButton provider={toGroup("github")} secretValue="bad" />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });
    expect(screen.getByRole("button", { name: "Test failed" })).toBeTruthy();

    act(() => { vi.advanceTimersByTime(5000); });
    await act(async () => { /* flush */ });

    expect(screen.getByRole("button", { name: "Test connection" })).toBeTruthy();
  });

  it("does not reset before 3 seconds on success", async () => {
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });
    expect(screen.getByRole("button", { name: "Connected ✓" })).toBeTruthy();

    act(() => { vi.advanceTimersByTime(2900); });
    await act(async () => { /* flush */ });

    // Still showing success
    expect(screen.getByRole("button", { name: "Connected ✓" })).toBeTruthy();
  });
});

describe("TestConnectionButton — onResult callback", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    mockValidateSecret.mockReset();
  });

  it("calls onResult(true) on success", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." onResult={onResult} />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });

    expect(onResult).toHaveBeenCalledWith(true);
  });

  it("calls onResult(false) on failure", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockResolvedValue({ valid: false });
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="bad" onResult={onResult} />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush microtasks */ });

    expect(onResult).toHaveBeenCalledWith(false);
  });

  it("calls onResult(false) when exception is thrown", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockRejectedValue(new Error("network error"));
    render(<TestConnectionButton provider={toGroup("anthropic")} secretValue="sk-..." onResult={onResult} />);

    fireEvent.click(screen.getByRole("button"));
    await act(async () => { /* flush */ });

    expect(onResult).toHaveBeenCalledWith(false);
  });
});
