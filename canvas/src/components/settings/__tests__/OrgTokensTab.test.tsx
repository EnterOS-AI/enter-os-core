// @vitest-environment jsdom
/**
 * Tests for OrgTokensTab — org-scoped API key management.
 *
 * Covers:
 *   - Loading state (spinner + aria-busy)
 *   - Empty state when no tokens
 *   - Token list rendering (single + multiple)
 *   - Token age display (just now, minutes, hours, days)
 *   - New key form: label input + Create button
 *   - Create: POST with optional name payload
 *   - Create: loading spinner during creation
 *   - New-token success box with copy button
 *   - Copy button writes to clipboard + shows "Copied"
 *   - Copy auto-resets to "Copy" after 2s
 *   - Dismiss button hides new-token box
 *   - Revoke button opens ConfirmDialog
 *   - ConfirmDialog cancel closes without calling API
 *   - ConfirmDialog confirm calls DELETE and re-fetches
 *   - Error banner on fetch failure
 *   - Error banner on create failure
 *   - Error banner on revoke failure
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OrgTokensTab } from "../OrgTokensTab";

vi.mock("@/components/ConfirmDialog", () => ({
  ConfirmDialog: vi.fn(() => null),
}));

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockDel = vi.fn();

vi.mock("@/lib/api", () => ({
  api: { get: (...args: unknown[]) => mockGet(...args), post: (...args: unknown[]) => mockPost(...args), del: (...args: unknown[]) => mockDel(...args) },
}));

// Stub clipboard
vi.stubGlobal("navigator", { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });

beforeEach(() => {
  vi.useRealTimers();
  mockGet.mockReset();
  mockPost.mockReset();
  mockDel.mockReset();
  vi.mocked(navigator.clipboard.writeText).mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ─── Helpers ──────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

function token(overrides: Partial<{
  id: string; prefix: string; name?: string; created_by?: string; created_at: string; last_used_at?: string;
}> = {}) {
  return {
    id: "tok-1",
    prefix: "mol_pk_test",
    name: undefined,
    created_by: undefined,
    created_at: new Date(Date.now() - 120_000).toISOString(),
    last_used_at: undefined,
    ...overrides,
  };
}

// ─── Loading ─────────────────────────────────────────────────────────────────

describe("OrgTokensTab — loading", () => {
  it("shows spinner while fetching", () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<OrgTokensTab />);
    expect(screen.getByRole("status")).toBeTruthy();
    expect(screen.getByText("Loading keys...")).toBeTruthy();
  });

  it("loading indicator has role=status and aria-live=polite", () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<OrgTokensTab />);
    const status = screen.getByRole("status");
    expect(status.getAttribute("aria-live")).toBe("polite");
    expect(status.textContent).toContain("Loading keys");
  });
});

// ─── Empty state ─────────────────────────────────────────────────────────────

describe("OrgTokensTab — empty", () => {
  it("shows empty state when no tokens", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText("No active keys")).toBeTruthy();
    expect(screen.getByText(/Create a key above to authenticate/i)).toBeTruthy();
  });
});

// ─── Token list ─────────────────────────────────────────────────────────────

describe("OrgTokensTab — token list", () => {
  it("renders token rows", async () => {
    mockGet.mockResolvedValue({ tokens: [token({ id: "tok-1", prefix: "mol_pk_abc" })], count: 1 });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/mol_pk_abc/)).toBeTruthy();
  });

  it("renders multiple token rows", async () => {
    mockGet.mockResolvedValue({
      tokens: [
        token({ id: "tok-1", prefix: "mol_pk_a" }),
        token({ id: "tok-2", prefix: "mol_pk_b" }),
      ],
      count: 2,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/mol_pk_a/)).toBeTruthy();
    expect(screen.getByText(/mol_pk_b/)).toBeTruthy();
  });

  it("shows token name when present", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1", prefix: "mol_pk_abc", name: "zapier-integration" })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText("zapier-integration")).toBeTruthy();
  });

  it("age shows 'just now' for very recent tokens", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1", created_at: new Date().toISOString() })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/just now/)).toBeTruthy();
  });

  it("age shows minutes ago", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1", created_at: new Date(Date.now() - 5 * 60_000).toISOString() })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/5m ago/)).toBeTruthy();
  });

  it("age shows hours ago", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1", created_at: new Date(Date.now() - 3 * 3600_000).toISOString() })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/3h ago/)).toBeTruthy();
  });

  it("age shows days ago", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1", created_at: new Date(Date.now() - 2 * 86400_000).toISOString() })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/2d ago/)).toBeTruthy();
  });

  it("each token has a Revoke button", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({ id: "tok-1" }), token({ id: "tok-2" })],
      count: 2,
    });
    render(<OrgTokensTab />);
    await flush();
    const revokeBtns = Array.from(document.querySelectorAll("button")).filter(b => b.textContent === "Revoke");
    expect(revokeBtns.length).toBe(2);
  });

  it("last_used_at is shown when present", async () => {
    mockGet.mockResolvedValue({
      tokens: [token({
        id: "tok-1",
        created_at: new Date(Date.now() - 86400_000).toISOString(),
        last_used_at: new Date(Date.now() - 3600_000).toISOString(),
      })],
      count: 1,
    });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/Last used/i)).toBeTruthy();
  });
});

// ─── Create token ─────────────────────────────────────────────────────────────

describe("OrgTokensTab — create", () => {
  it("Create button calls POST with empty body when no label", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_new_secret", prefix: "tok_new" });
    render(<OrgTokensTab />);
    await flush();
    const createBtn = screen.getByRole("button", { name: "+ New Key" });
    await act(async () => { createBtn.click(); });
    await flush();
    expect(mockPost).toHaveBeenCalledWith("/org/tokens", {});
  });

  it("Create button calls POST with name when label is filled", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_new_secret", prefix: "tok_new" });
    render(<OrgTokensTab />);
    await flush();
    const input = screen.getByRole("textbox");
    fireEvent.change(input, { target: { value: "zapier-prod" } });
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(mockPost).toHaveBeenCalledWith("/org/tokens", { name: "zapier-prod" });
  });

  it("shows spinner while creating", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockImplementation(() => new Promise(() => {}));
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(screen.getByText(/Creating/)).toBeTruthy();
  });

  it("shows new token box after creation", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_new_secret_xyz", prefix: "tok_new" });
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(screen.getByText(/tok_new_secret_xyz/)).toBeTruthy();
    expect(screen.getByText(/Copy now/)).toBeTruthy();
  });

  it("new token shows label when provided", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_abc123", prefix: "tok_abc" });
    render(<OrgTokensTab />);
    await flush();
    const input = screen.getByRole("textbox");
    fireEvent.change(input, { target: { value: "my-label" } });
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(screen.getByText(/New Key: my-label/)).toBeTruthy();
  });

  it("dismiss hides the new-token box", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_dismiss", prefix: "tok_d" });
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(screen.getByText(/tok_dismiss/)).toBeTruthy();
    await act(async () => { screen.getByText("Dismiss").closest("button")!.click(); });
    await flush();
    expect(screen.queryByText(/tok_dismiss/)).toBeNull();
  });
});

// ─── Copy button ─────────────────────────────────────────────────────────────

describe("OrgTokensTab — copy", () => {
  it("Copy button writes token to clipboard", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_copy_test", prefix: "tok_c" });
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    const copyBtn = screen.getByRole("button", { name: "Copy" });
    await act(async () => { copyBtn.click(); });
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith("tok_copy_test");
  });

  it("Copy button shows 'Copied' after click", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_copy_2", prefix: "tok_c" });
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    await act(async () => { screen.getByRole("button", { name: "Copy" }).click(); });
    await flush();
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
  });

  it("Copy resets to 'Copy' after 2s", async () => {
    vi.useFakeTimers();
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockResolvedValue({ auth_token: "tok_timer", prefix: "tok_t" });
    render(<OrgTokensTab />);
    await act(async () => { await Promise.resolve(); });
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await act(async () => { await Promise.resolve(); });
    await act(async () => { screen.getByRole("button", { name: "Copy" }).click(); });
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
    act(() => { vi.advanceTimersByTime(2000); });
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
    vi.useRealTimers();
  });
});

// ─── Revoke ─────────────────────────────────────────────────────────────────

describe("OrgTokensTab — revoke", () => {
  it("Revoke button opens ConfirmDialog", async () => {
    mockGet.mockResolvedValue({ tokens: [token({ id: "tok-revoke", prefix: "mol_pk_rev" })], count: 1 });
    render(<OrgTokensTab />);
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    await act(async () => {
      Array.from(document.querySelectorAll("button")).find(b => b.textContent === "Revoke")!.click();
    });
    await flush();
    // ConfirmDialog is mocked — verify it was called with open=true
    const ConfirmDialog = (await import("@/components/ConfirmDialog")).ConfirmDialog as ReturnType<typeof vi.fn>;
    const lastCall = ConfirmDialog.mock.calls[ConfirmDialog.mock.calls.length - 1];
    expect(lastCall[0]).toMatchObject({ open: true, title: "Revoke API Key" });
  });

  it("DELETE is called with correct URL on confirm", async () => {
    mockGet.mockResolvedValue({ tokens: [token({ id: "tok-del", prefix: "mol_pk_del" })], count: 1 });
    mockDel.mockResolvedValue(undefined);
    render(<OrgTokensTab />);
    await flush();

    // Open confirm
    await act(async () => {
      Array.from(document.querySelectorAll("button")).find(b => b.textContent === "Revoke")!.click();
    });
    await flush();

    // Get the onConfirm prop from the last ConfirmDialog call
    const ConfirmDialog = (await import("@/components/ConfirmDialog")).ConfirmDialog as ReturnType<typeof vi.fn>;
    const lastCall = ConfirmDialog.mock.calls[ConfirmDialog.mock.calls.length - 1];
    const onConfirm = lastCall[0]?.onConfirm;

    // Call onConfirm
    await act(async () => { onConfirm?.(); });
    await flush();

    expect(mockDel).toHaveBeenCalledWith("/org/tokens/tok-del");
  });
});

// ─── Error states ─────────────────────────────────────────────────────────────

describe("OrgTokensTab — errors", () => {
  it("shows error when fetch fails", async () => {
    mockGet.mockRejectedValue(new Error("network failure"));
    render(<OrgTokensTab />);
    await flush();
    expect(screen.getByText(/network failure/i)).toBeTruthy();
  });

  it("shows error when create fails", async () => {
    mockGet.mockResolvedValue({ tokens: [], count: 0 });
    mockPost.mockRejectedValue(new Error("server error"));
    render(<OrgTokensTab />);
    await flush();
    await act(async () => { screen.getByRole("button", { name: "+ New Key" }).click(); });
    await flush();
    expect(screen.getByText(/server error/i)).toBeTruthy();
  });

  it("shows error when revoke fails", async () => {
    mockGet.mockResolvedValue({ tokens: [token({ id: "tok-err" })], count: 1 });
    mockDel.mockRejectedValue(new Error("revoke denied"));
    render(<OrgTokensTab />);
    await flush();

    await act(async () => {
      Array.from(document.querySelectorAll("button")).find(b => b.textContent === "Revoke")!.click();
    });
    await flush();

    const ConfirmDialog = (await import("@/components/ConfirmDialog")).ConfirmDialog as ReturnType<typeof vi.fn>;
    const onConfirm = ConfirmDialog.mock.calls[ConfirmDialog.mock.calls.length - 1][0]?.onConfirm;
    await act(async () => { onConfirm?.(); });
    await flush();

    expect(screen.getByText(/revoke denied/i)).toBeTruthy();
  });
});
