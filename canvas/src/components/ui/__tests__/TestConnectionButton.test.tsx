// @vitest-environment jsdom
/**
 * TestConnectionButton — async connection tester for secret keys.
 *
 * States: idle → testing → success/failure → auto-reset to idle.
 *
 * Coverage:
 *   - Idle state: renders "Test connection" label
 *   - Disabled when secretValue is empty
 *   - Enabled when secretValue is present
 *   - Disabled while testing
 *   - Success path: calls validateSecret, shows "Connected ✓", resets after 3s
 *   - Failure path: calls validateSecret, shows "Test failed", shows error detail
 *   - Catch path: network error shows "Connection timed out"
 *   - Error detail only shown on failure state
 *   - onResult callback called with correct value
 *   - Cleanup: timer cancelled on unmount
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { TestConnectionButton } from "../TestConnectionButton";

const mockValidateSecret = vi.fn();

vi.mock("@/lib/api/secrets", () => ({
  validateSecret: (...args: unknown[]) => mockValidateSecret(...args),
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  },
}));

// Re-import the mocked ApiError so test cases construct the same class the
// component's `instanceof` check sees.
import { ApiError } from "@/lib/api/secrets";

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("TestConnectionButton — render", () => {
  it("renders 'Test connection' in idle state", () => {
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    expect(document.body.textContent).toContain("Test connection");
  });

  it("is disabled when secretValue is empty", () => {
    render(
      <TestConnectionButton provider="github" secretValue="" />,
    );
    const btn = document.querySelector('button[type="button"]');
    expect(btn?.getAttribute("disabled")).not.toBeNull();
  });

  it("is enabled when secretValue is present", () => {
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    const btn = document.querySelector('button[type="button"]');
    expect(btn?.getAttribute("disabled")).toBeNull();
  });
});

describe("TestConnectionButton — success path", () => {
  it("shows 'Testing…' while validating", async () => {
    mockValidateSecret.mockImplementation(
      () => new Promise(() => {}), // never resolves — stays in testing state
    );
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    const btn = document.querySelector('button[type="button"]')!;
    await act(async () => {
      fireEvent.click(btn);
    });

    expect(document.body.textContent).toContain("Testing");
    expect(btn.getAttribute("disabled")).not.toBeNull(); // disabled while testing
  });

  it("shows 'Connected ✓' after successful validation", async () => {
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    const btn = document.querySelector('button[type="button"]')!;
    fireEvent.click(btn);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("Connected");
  });

  it("resets to idle after 3 seconds on success", async () => {
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);

    // Resolve the mock and flush React state synchronously via act
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    // Advance past the 3000ms RESET_DELAYS.success
    await act(async () => {
      vi.advanceTimersByTime(3001);
    });
    expect(document.body.textContent).toContain("Test connection");
  });

  it("calls onResult(true) on success", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockResolvedValue({ valid: true });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" onResult={onResult} />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(onResult).toHaveBeenCalledWith(true);
  });
});

describe("TestConnectionButton — failure path", () => {
  it("shows 'Test failed' after invalid key", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false, error: "Invalid token" });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_invalid" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("Test failed");
  });

  it("shows error detail message", async () => {
    mockValidateSecret.mockResolvedValue({
      valid: false,
      error: "Token missing required scopes",
    });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_invalid" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("Token missing required scopes");
  });

  it("resets to idle after 5 seconds on failure", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_invalid" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    await act(async () => {
      vi.advanceTimersByTime(5001);
    });
    expect(document.body.textContent).toContain("Test connection");
  });

  it("shows default error when error is absent", async () => {
    mockValidateSecret.mockResolvedValue({ valid: false });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_invalid" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("Could not verify key");
  });

  it("calls onResult(false) on failure", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockResolvedValue({ valid: false });
    render(
      <TestConnectionButton provider="github" secretValue="ghp_invalid" onResult={onResult} />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(onResult).toHaveBeenCalledWith(false);
  });
});

describe("TestConnectionButton — catch path", () => {
  it("does NOT claim a timeout when the validate endpoint 404s (regression: internal#492)", async () => {
    // The validate route is unimplemented on the server and returns a fast
    // 404. Before the fix this rendered the misleading hardcoded string
    // "Connection timed out. Service may be down." It must instead state
    // honestly that validation isn't available and the key was not tested.
    mockValidateSecret.mockRejectedValue(new ApiError(404, "Not Found"));
    render(
      <TestConnectionButton provider="anthropic" secretValue="sk-ant-xxx" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).not.toContain("Connection timed out");
    expect(document.body.textContent).not.toContain("Service may be down");
    expect(document.body.textContent).toContain("not available");
    expect(document.body.textContent).toContain("not tested");
  });

  it("reports a non-404 server error with its status, not a timeout", async () => {
    mockValidateSecret.mockRejectedValue(new ApiError(500, "Internal Server Error"));
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("500");
    expect(document.body.textContent).not.toContain("Connection timed out");
  });

  it("shows a connectivity message on a genuine network error", async () => {
    mockValidateSecret.mockRejectedValue(new Error("network down"));
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(document.body.textContent).toContain("Could not reach the validation service");
  });

  it("calls onResult(false) on network error", async () => {
    const onResult = vi.fn();
    mockValidateSecret.mockRejectedValue(new Error("timeout"));
    render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" onResult={onResult} />,
    );
    fireEvent.click(document.querySelector('button[type="button"]')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(onResult).toHaveBeenCalledWith(false);
  });
});

describe("TestConnectionButton — cleanup", () => {
  it("clears timer on unmount", async () => {
    const clearTimeoutSpy = vi.spyOn(globalThis, "clearTimeout");
    mockValidateSecret.mockImplementation(
      () => new Promise(() => {}), // never resolves
    );
    const { unmount } = render(
      <TestConnectionButton provider="github" secretValue="ghp_xxx" />,
    );
    await act(async () => {
      fireEvent.click(document.querySelector('button[type="button"]')!);
    });
    unmount();
    expect(clearTimeoutSpy).toHaveBeenCalled();
  });
});
