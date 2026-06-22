// @vitest-environment jsdom
/**
 * Tests for SecretRow — single secret display/edit row.
 *
 * Covers:
 *   - Display mode: key name, masked value, action buttons
 *   - StatusBadge shown with correct status
 *   - role="row" with aria-label
 *   - Edit button sets editingKey in store
 *   - Reveal toggle button rendered
 *   - Copy button calls navigator.clipboard.writeText
 *   - Delete button dispatches secret:delete-request event
 *   - Edit mode: KeyValueField + save/cancel rendered
 *   - Cancel calls setEditingKey(null)
 *   - Save calls updateSecret + setSecretStatus
 *   - Save error shown on failure
 *   - TestConnectionButton shown when testSupported + value entered
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SecretRow } from "../SecretRow";

// ── Hoisted mocks — vi.hoisted() so they're stable references ────────────────

const { mockUpdateSecret, mockSetSecretStatus, mockSetEditingKey, mockValidateSecretValue } = vi.hoisted(() => ({
  mockUpdateSecret: vi.fn(),
  mockSetSecretStatus: vi.fn(),
  mockSetEditingKey: vi.fn(),
  mockValidateSecretValue: vi.fn(() => null), // always valid to avoid secret-pattern triggers
}));

// ── Store mock — single shared mutable object ───────────────────────────────

const storeState = {
  editingKey: null as string | null,
  setEditingKey: mockSetEditingKey,
  updateSecret: mockUpdateSecret,
  setSecretStatus: mockSetSecretStatus,
};

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: Object.assign(
    vi.fn((selector?: (s: typeof storeState) => unknown) =>
      selector ? selector(storeState) : storeState
    ),
    { getState: () => storeState },
  ),
}));

// ── Child component stubs ────────────────────────────────────────────────────

vi.mock("@/lib/validation/secret-formats", () => ({
  validateSecretValue: mockValidateSecretValue,
}));

vi.mock("@/components/ui/StatusBadge", () => ({
  StatusBadge: ({ status }: { status: string }) => (
    <span data-testid="status-badge" data-status={status}>{status}</span>
  ),
}));

vi.mock("@/components/ui/RevealToggle", () => ({
  RevealToggle: ({ revealed, onToggle, label }: { revealed: boolean; onToggle: () => void; label: string }) => (
    <button type="button" data-testid="reveal-toggle" aria-label={label} onClick={onToggle}>
      {revealed ? "HIDE" : "REVEAL"}
    </button>
  ),
}));

vi.mock("@/components/ui/KeyValueField", () => ({
  KeyValueField: ({ value, onChange, disabled }: { value: string; onChange: (v: string) => void; disabled?: boolean }) => (
    <textarea
      data-testid="edit-value-field"
      value={value}
      onChange={(e) => { onChange(e.target.value); }}
      disabled={disabled}
    />
  ),
}));

vi.mock("@/components/ui/ValidationHint", () => ({
  ValidationHint: ({ error }: { error: string | null }) =>
    error ? <span role="alert">{error}</span> : null,
}));

vi.mock("@/components/ui/TestConnectionButton", () => ({
  TestConnectionButton: () => <button data-testid="test-connection-btn" type="button">Test connection</button>,
}));

// ── Test data ────────────────────────────────────────────────────────────────

const GITHUB_SECRET = { name: "GITHUB_TOKEN", masked_value: "ghp_••••••••••••xK9f", group: "github" as const, status: "verified" as const, updated_at: "2024-01-01" };
const ANTHROPIC_SECRET = { name: "ANTHROPIC_API_KEY", masked_value: "sk-ant-•••••••••••••••••a3Zq", group: "anthropic" as const, status: "unverified" as const, updated_at: "2024-01-02" };
const CUSTOM_SECRET = { name: "MY_CUSTOM_KEY", masked_value: "••••••••••••••••9d2a", group: "custom" as const, status: "invalid" as const, updated_at: "2024-01-03" };

// Use a value that definitely does NOT match any secret format regex
const EDIT_VALUE = "TEST_VALID_TOKEN_VALUE_PLACEHOLDER_FOR_EDIT_MODE";

beforeEach(() => {
  // Mutate the shared object so all closures see the update
  storeState.editingKey = null;
  storeState.setEditingKey = vi.fn();
  storeState.updateSecret = vi.fn().mockResolvedValue(undefined);
  storeState.setSecretStatus = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ─── Display mode ───────────────────────────────────────────────────────────

describe("SecretRow — display mode", () => {
  it("shows secret name", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByText("GITHUB_TOKEN")).toBeTruthy();
  });

  it("shows masked value", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByText("ghp_••••••••••••xK9f")).toBeTruthy();
  });

  it("shows StatusBadge", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByTestId("status-badge")).toBeTruthy();
  });

  it("StatusBadge has correct data-status attribute", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByTestId("status-badge").getAttribute("data-status")).toBe("verified");
  });

  it("role=row", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(document.querySelector('[role="row"]')).toBeTruthy();
  });

  it("has Copy, Edit, Delete buttons", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByRole("button", { name: /copy/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /edit/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /delete/i })).toBeTruthy();
  });

  // Regression: the reveal/eye control was a dead affordance. Clicking it
  // flipped its own icon (eye → eye-with-slash) but never revealed the value,
  // because secret values are write-only from the browser (server List
  // "Never exposes values"; there is no per-secret decrypt endpoint and the
  // client has no plaintext-fetch function). The honest fix removes the
  // toggle and shows a static "write-only / cannot be revealed" indicator.
  // See internal tracking issue + internal#210/#211.
  it("does NOT render a reveal/eye toggle (values are write-only)", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.queryByTestId("reveal-toggle")).toBeNull();
    expect(
      screen.queryByRole("button", { name: /toggle reveal/i }),
    ).toBeNull();
  });

  it("shows a write-only indicator explaining the value cannot be revealed", () => {
    render(<SecretRow secret={ANTHROPIC_SECRET} workspaceId="ws-1" />);
    const indicator = screen.getByTestId("write-only-indicator");
    expect(indicator).toBeTruthy();
    // Affordance must be honest: explain it cannot be revealed and that
    // Edit is the rotate path. It must not be a clickable button.
    const title = indicator.getAttribute("title") ?? "";
    expect(title.toLowerCase()).toMatch(/write-only|cannot be revealed/);
    expect(indicator.tagName).not.toBe("BUTTON");
  });

  it("write-only indicator is present for the Anthropic/OAuth-token row too", () => {
    // The reported bug singled out CLAUDE_CODE_OAUTH_TOKEN (anthropic group);
    // the fix is group-agnostic — every row gets the same honest affordance.
    const OAUTH_SECRET = {
      name: "CLAUDE_CODE_OAUTH_TOKEN",
      masked_value: "••••••••••••••••9d2a",
      group: "anthropic" as const,
      status: "unverified" as const,
      updated_at: "2024-01-04",
    };
    render(<SecretRow secret={OAUTH_SECRET} workspaceId="ws-1" />);
    expect(screen.queryByTestId("reveal-toggle")).toBeNull();
    expect(screen.getByTestId("write-only-indicator")).toBeTruthy();
  });

  it("shows invalid status correctly", () => {
    render(<SecretRow secret={CUSTOM_SECRET} workspaceId="ws-1" />);
    expect(screen.getByTestId("status-badge").getAttribute("data-status")).toBe("invalid");
  });
});

// ─── Edit ───────────────────────────────────────────────────────────────────

describe("SecretRow — edit", () => {
  it("Edit button calls setEditingKey(secret.name)", () => {
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(storeState.setEditingKey).toHaveBeenCalledWith("GITHUB_TOKEN");
  });

  it("shows edit form (KeyValueField + save/cancel) when editingKey set", () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.getByTestId("edit-value-field")).toBeTruthy();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /save/i })).toBeTruthy();
  });

  it("Cancel calls setEditingKey(null)", () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(storeState.setEditingKey).toHaveBeenCalledWith(null);
  });

  it("Save button disabled when editValue is empty", () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect((screen.getByRole("button", { name: /save/i }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("Save enabled when editValue is non-empty", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-abc" />);
    const textarea = screen.getByTestId("edit-value-field");
    fireEvent.change(textarea, { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    expect((screen.getByRole("button", { name: /save/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("Save calls updateSecret(workspaceId, name, editValue)", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-test" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await act(async () => { await Promise.resolve(); });
    expect(storeState.updateSecret).toHaveBeenCalledWith("ws-test", "GITHUB_TOKEN", EDIT_VALUE);
  });

  it("Save calls setSecretStatus(secret.name, 'unverified')", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await act(async () => { await Promise.resolve(); });
    expect(storeState.setSecretStatus).toHaveBeenCalledWith("GITHUB_TOKEN", "unverified");
  });

  it("Save button shows 'Saving…' during pending save", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    storeState.updateSecret = vi.fn(() => new Promise(() => {}));
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("Saving…")).toBeTruthy();
  });

  it("shows error on save failure", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    storeState.updateSecret = vi.fn().mockRejectedValue(new Error("network error"));
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/network error/i)).toBeTruthy();
  });
});

// ─── Copy ───────────────────────────────────────────────────────────────────

describe("SecretRow — copy", () => {
  it("Copy calls navigator.clipboard.writeText with masked value", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /copy/i }));
    expect(writeText).toHaveBeenCalledWith("ghp_••••••••••••xK9f");
  });
});

// ─── Delete ─────────────────────────────────────────────────────────────────

describe("SecretRow — delete", () => {
  it("Delete dispatches secret:delete-request with secret name", () => {
    const listener = vi.fn();
    window.addEventListener("secret:delete-request", listener);
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    expect(listener).toHaveBeenCalledWith(
      expect.objectContaining({ detail: "GITHUB_TOKEN" })
    );
    window.removeEventListener("secret:delete-request", listener);
  });
});

// ─── TestConnectionButton ────────────────────────────────────────────────────

describe("SecretRow — TestConnectionButton", () => {
  it("shown for github secret when editValue is entered", async () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByTestId("test-connection-btn")).toBeTruthy();
  });

  it("NOT shown for custom secret (testSupported=false)", async () => {
    storeState.editingKey = "MY_CUSTOM_KEY";
    render(<SecretRow secret={CUSTOM_SECRET} workspaceId="ws-1" />);
    fireEvent.change(screen.getByTestId("edit-value-field"), { target: { value: EDIT_VALUE } });
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByTestId("test-connection-btn")).toBeNull();
  });

  it("NOT shown when editValue is empty", () => {
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SecretRow secret={GITHUB_SECRET} workspaceId="ws-1" />);
    expect(screen.queryByTestId("test-connection-btn")).toBeNull();
  });
});
