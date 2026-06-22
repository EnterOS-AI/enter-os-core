// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// ---------------------------------------------------------------------------
// We test ErrorBoundary both in pure-unit style (class instantiation) and
// with React DOM rendering to verify it catches child render throws.
// ---------------------------------------------------------------------------

// Mock fetch globally so transitive imports of api.ts don't blow up
globalThis.fetch = vi.fn(() =>
  Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response)
);

import React from "react";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { ErrorBoundary } from "../ErrorBoundary";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createInstance(props: { children: React.ReactNode } = { children: null }) {
  const instance = new ErrorBoundary(props);
  return instance;
}

function Thrower({ shouldThrow }: { shouldThrow: boolean }) {
  if (shouldThrow) {
    throw new Error("boom");
  }
  return <div data-testid="child">healthy child</div>;
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("ErrorBoundary", () => {
  let consoleErrorSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    consoleErrorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    consoleErrorSpy.mockRestore();
    cleanup();
  });

  // ---- static getDerivedStateFromError -----------------------------------

  it("getDerivedStateFromError returns error state", () => {
    const err = new Error("boom");
    const state = ErrorBoundary.getDerivedStateFromError(err);
    expect(state).toEqual({ hasError: true, error: err });
  });

  // ---- componentDidCatch ------------------------------------------------

  it("componentDidCatch logs to console.error", () => {
    const instance = createInstance();
    const err = new Error("render failure");
    const info: React.ErrorInfo = { componentStack: "<App>\n  <Child>" } as React.ErrorInfo;

    instance.componentDidCatch(err, info);

    expect(consoleErrorSpy).toHaveBeenCalledWith(
      "ErrorBoundary caught an error:",
      err,
      "<App>\n  <Child>"
    );
  });

  // ---- initial state (no error) -----------------------------------------

  it("initial state has no error", () => {
    const instance = createInstance();
    expect(instance.state.hasError).toBe(false);
    expect(instance.state.error).toBeNull();
  });

  // ---- render with no error returns children ----------------------------

  it("render returns children when there is no error", () => {
    const child = React.createElement("div", null, "Hello");
    const instance = createInstance({ children: child });
    // state should be no-error, so render() returns children
    const result = instance.render();
    expect(result).toBe(child);
  });

  // ---- render with error returns fallback UI ----------------------------

  it("render returns fallback UI when hasError is true", () => {
    const instance = createInstance({ children: React.createElement("div") });
    // Simulate error state
    instance.state = { hasError: true, error: new Error("test crash") };

    const result = instance.render();

    // result should be a React element (the fallback), not the children
    expect(result).not.toBeNull();
    expect(typeof result).toBe("object");
    // The fallback is a div, not the original children
    const element = result as React.ReactElement<{ className?: string }>;
    expect(element.props?.className).toContain("fixed");
    expect(element.props?.className).toContain("inset-0");
  });

  // ---- fallback UI contains error message --------------------------------

  it("fallback UI includes the error message text", () => {
    const instance = createInstance({ children: React.createElement("div") });
    instance.state = { hasError: true, error: new Error("kaboom!") };

    const result = instance.render() as React.ReactElement;
    // Deep-search the rendered tree for the error message
    const json = JSON.stringify(result);
    expect(json).toContain("kaboom!");
    expect(json).toContain("Something went wrong");
    expect(json).toContain("Reload");
    expect(json).toContain("Report");
  });

  // ---- fallback UI renders safely with null error -----------------------

  it("fallback UI handles null error gracefully", () => {
    const instance = createInstance({ children: React.createElement("div") });
    instance.state = { hasError: true, error: null };

    const result = instance.render() as React.ReactElement;
    const json = JSON.stringify(result);
    expect(json).toContain("Unknown error");
    expect(json).toContain("Something went wrong");
  });

  // ---- DOM integration: catches child render throws ---------------------

  it("renders a fallback when a child throws during render", () => {
    render(
      <ErrorBoundary>
        <Thrower shouldThrow={true} />
      </ErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toBeDefined();
    expect(screen.queryByText("Something went wrong")).not.toBeNull();
    expect(screen.queryByText("boom")).not.toBeNull();
  });

  it("reload button calls window.location.reload", () => {
    const reload = vi.fn();
    Object.defineProperty(window, "location", {
      value: { reload },
      writable: true,
    });

    render(
      <ErrorBoundary>
        <Thrower shouldThrow={true} />
      </ErrorBoundary>,
    );

    fireEvent.click(screen.getByRole("button", { name: /reload/i }));
    expect(reload).toHaveBeenCalled();
  });
});
