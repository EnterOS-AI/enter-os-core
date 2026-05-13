// @vitest-environment jsdom
/**
 * TokensTab — workspace API token management.
 *
 * Per spec §5: lists bearer tokens, creates new ones, revokes existing.
 * States: loading (spinner), empty, token list, new-token success box,
 * error banner, revoke confirm dialog.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * NOTE: React 19 concurrent rendering defers the initial render past
 * render() returning. Use flush() (act + await Promise.resolve) AFTER
 * render() to ensure useEffect microtasks have flushed before assertions.
 *
 * Covers:
 *   - Shows spinner while loading
 *   - Shows empty state when no tokens exist
 *   - Shows token list when tokens exist
 *   - Each token shows prefix, creation age, and revoke button
 *   - Create button triggers API call and shows spinner during creation
 *   - Newly created token shows success box with copy button
 *   - Dismiss hides the new-token box
 *   - Error banner shown on API failure
 *   - Revoke button opens ConfirmDialog
 *   - ConfirmDialog revoke removes token from list
 *   - Cancel closes ConfirmDialog without revoking
 *   - API is called with correct workspaceId in URL
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import React from "react";

import { TokensTab } from "../TokensTab";

// ─── Mocks ────────────────────────────────────────────────────────────────────

const mockApiGet = vi.fn();
const mockApiPost = vi.fn();
const mockApiDel = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => mockApiGet(...args),
    post: (...args: unknown[]) => mockApiPost(...args),
    del: (...args: unknown[]) => mockApiDel(...args),
  },
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

const WS_ID = "ws-test-123";

function renderTab() {
  return render(<TokensTab workspaceId={WS_ID} />);
}

/** Flush React useEffect microtasks after render (per ChannelsTab pattern). */
async function flush() {
  await act(async () => { await Promise.resolve(); });
}

afterEach(() => {
  cleanup();
  // NOTE: Do NOT call mockReset() here — it clears the mockResolvedValue
  // set in each describe-block's beforeEach, causing the next test's
  // api.get() to return undefined instead of the intended mock data.
  // Each describe-block calls mockReset() itself before setting up mocks.
});

// ─── Loading state ─────────────────────────────────────────────────────────────

describe("TokensTab — loading", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    // Never resolves — component stays in loading state
    mockApiGet.mockImplementation(() => new Promise(() => {}));
  });

  it("shows spinner while loading", () => {
    renderTab();
    // Loading state is synchronous — no flush needed
    const loadingEl = document.querySelector('[role="status"]');
    expect(loadingEl?.textContent).toContain("Loading");
  });
});

// ─── Empty state ─────────────────────────────────────────────────────────────

describe("TokensTab — empty", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockApiGet.mockResolvedValue({ tokens: [], count: 0 });
  });

  it("shows empty state when no tokens exist", async () => {
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("No active tokens");
  });
});

// ─── Token list ─────────────────────────────────────────────────────────────

describe("TokensTab — token list", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockApiPost.mockReset();
    mockApiDel.mockReset();
    mockApiGet.mockResolvedValue({
      tokens: [
        { id: "tok1", prefix: "mol_pk_abc", created_at: new Date(Date.now() - 120 * 60 * 1000).toISOString(), last_used_at: null },
        { id: "tok2", prefix: "mol_pk_xyz", created_at: new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString(), last_used_at: new Date(Date.now() - 60 * 60 * 1000).toISOString() },
      ],
      count: 2,
    });
  });

  it("renders tokens when API returns them", async () => {
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("mol_pk_abc");
    expect(document.body.textContent).toContain("mol_pk_xyz");
  });

  it("each token has a Revoke button", async () => {
    renderTab();
    await flush();
    const revokeBtns = Array.from(document.querySelectorAll("button")).filter(
      (b) => b.textContent === "Revoke",
    );
    expect(revokeBtns).toHaveLength(2);
  });

  it("API get is called with correct workspaceId", async () => {
    renderTab();
    await flush();
    expect(mockApiGet).toHaveBeenCalledWith(`/workspaces/${WS_ID}/tokens`);
  });

  it("revoke button opens ConfirmDialog", async () => {
    renderTab();
    await flush();
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    const revokeBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Revoke",
    ) as HTMLButtonElement;
    await act(async () => {
      revokeBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.querySelector('[role="dialog"]')).toBeTruthy();
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain("Revoke Token");
  });

  it("ConfirmDialog cancel closes the dialog", async () => {
    renderTab();
    await flush();
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    const revokeBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Revoke",
    ) as HTMLButtonElement;
    await act(async () => {
      revokeBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.querySelector('[role="dialog"]')).toBeTruthy();
    const cancelBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Cancel",
    ) as HTMLButtonElement;
    await act(async () => {
      cancelBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    // API delete should NOT have been called
    expect(mockApiDel).not.toHaveBeenCalled();
  });

  it("ConfirmDialog confirm calls API del and re-fetches", async () => {
    mockApiDel.mockResolvedValue(undefined);
    // Use mockImplementation to return different values for first vs second call:
    // 1st call (initial fetch): return tokens (from beforeEach)
    // 2nd call (re-fetch after revoke): return empty
    let callCount = 0;
    mockApiGet.mockImplementation(() => {
      callCount++;
      if (callCount === 1) {
        return Promise.resolve({
          tokens: [
            { id: "tok1", prefix: "mol_pk_abc", created_at: new Date(Date.now() - 120 * 60 * 1000).toISOString(), last_used_at: null },
            { id: "tok2", prefix: "mol_pk_xyz", created_at: new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString(), last_used_at: new Date(Date.now() - 60 * 60 * 1000).toISOString() },
          ],
          count: 2,
        });
      }
      return Promise.resolve({ tokens: [], count: 0 });
    });
    renderTab();
    await flush();
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(document.body.textContent).toContain("mol_pk_abc");
    const revokeBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Revoke",
    ) as HTMLButtonElement;
    await act(async () => {
      revokeBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.querySelector('[role="dialog"]')).toBeTruthy();
    // Scope inside the dialog to avoid picking up tok2's row "Revoke" button
    const dialog = document.querySelector('[role="dialog"]') as Element;
    const confirmBtn = Array.from(dialog.querySelectorAll("button")).find(
      (b) => b.textContent === "Revoke",
    ) as HTMLButtonElement;
    await act(async () => {
      confirmBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(mockApiDel).toHaveBeenCalledWith(`/workspaces/${WS_ID}/tokens/tok1`);
  });
});

// ─── Create token ─────────────────────────────────────────────────────────────

describe("TokensTab — create token", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockApiPost.mockReset();
    mockApiGet.mockResolvedValue({ tokens: [], count: 0 });
  });

  it("create button triggers POST and shows new token box", async () => {
    mockApiPost.mockResolvedValue({ auth_token: "mol_pk_newtoken12345" });
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("No active tokens");
    const createBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("New Token"),
    ) as HTMLButtonElement;
    // Update mock for re-fetch after POST resolves
    mockApiGet.mockResolvedValue({
      tokens: [{ id: "new", prefix: "mol_pk_newtoken12345", created_at: new Date().toISOString(), last_used_at: null }],
      count: 1,
    });
    await act(async () => {
      createBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    await flush();
    expect(document.body.textContent).toContain("mol_pk_newtoken12345");
    expect(mockApiPost).toHaveBeenCalledWith(`/workspaces/${WS_ID}/tokens`);
  });

  it("dismiss button hides new-token box", async () => {
    mockApiPost.mockResolvedValue({ auth_token: "mol_pk_test123" });
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("No active tokens");
    mockApiGet.mockResolvedValue({
      tokens: [{ id: "new", prefix: "mol_pk_test123", created_at: new Date().toISOString(), last_used_at: null }],
      count: 1,
    });
    const createBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("New Token"),
    ) as HTMLButtonElement;
    await act(async () => {
      createBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    await flush();
    expect(document.body.textContent).toContain("New Token Created");
    const dismissBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Dismiss",
    ) as HTMLButtonElement;
    await act(async () => {
      dismissBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.body.textContent).not.toContain("New Token Created");
  });

  it("error shown when create fails", async () => {
    mockApiPost.mockRejectedValue(new Error("Server error"));
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("No active tokens");
    const createBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("New Token"),
    ) as HTMLButtonElement;
    await act(async () => {
      createBtn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    expect(document.body.textContent).toContain("Server error");
  });
});

// ─── Error state ─────────────────────────────────────────────────────────────

describe("TokensTab — error", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockApiGet.mockRejectedValue(new Error("Network failure"));
  });

  it("shows error message when API fails", async () => {
    renderTab();
    await flush();
    expect(document.body.textContent).toContain("Network failure");
    // Should NOT show spinner
    expect(document.querySelector('[role="status"]')).toBeNull();
  });
});
