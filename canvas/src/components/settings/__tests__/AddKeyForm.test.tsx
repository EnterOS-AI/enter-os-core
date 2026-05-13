// @vitest-environment jsdom
/**
 * Tests for AddKeyForm — inline form for adding a new API key.
 *
 * Covers:
 *   - Header + key name + value fields rendered
 *   - Key name auto-uppercased on input
 *   - Validation: UPPER_SNAKE_CASE required, duplicate name blocked
 *   - Provider hint shown for known providers (GitHub, Anthropic, OpenRouter)
 *   - Provider hint hidden for custom key names
 *   - Debounced value validation
 *   - Save button disabled when form invalid / saving
 *   - createSecret called on save with correct args
 *   - onCancel called on Cancel click
 *   - Save error shown on failure
 *   - TestConnectionButton shown when value is format-valid and provider supports it
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AddKeyForm } from "../AddKeyForm";

// ── Mocks ─────────────────────────────────────────────────────────────────────

const { mockValidateSecretValue, mockIsValidKeyName, mockInferGroup } = vi.hoisted(() => ({
  mockValidateSecretValue: vi.fn((value: string) => {
    // Return error for "bad-value" to test ValidationHint display
    if (value === "bad-value") return "Invalid format";
    return null;
  }),
  mockIsValidKeyName: vi.fn((name: string) => /^[A-Z][A-Z0-9_]*$/.test(name)),
  mockInferGroup: vi.fn((name: string) => {
    const u = name.toUpperCase();
    if (u.includes("GITHUB")) return "github" as const;
    if (u.includes("ANTHROPIC")) return "anthropic" as const;
    if (u.includes("OPENROUTER")) return "openrouter" as const;
    return "custom" as const;
  }),
}));

const mockCreateSecret = vi.fn();

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: Object.assign(
    vi.fn((selector?: (s: { createSecret: typeof mockCreateSecret }) => unknown) =>
      selector ? selector({ createSecret: mockCreateSecret }) : { createSecret: mockCreateSecret }
    ),
    { getState: () => ({ createSecret: mockCreateSecret }) },
  ),
}));

vi.mock("@/lib/validation/secret-formats", () => ({
  validateSecretValue: mockValidateSecretValue,
  isValidKeyName: mockIsValidKeyName,
  inferGroup: mockInferGroup,
}));

vi.mock("@/lib/services", () => ({
  SERVICES: {
    github: { label: "GitHub", icon: "github", keyNames: [], docsUrl: "https://github.com", testSupported: true },
    anthropic: { label: "Anthropic", icon: "anthropic", keyNames: [], docsUrl: "https://anthropic.com", testSupported: true },
    openrouter: { label: "OpenRouter", icon: "openrouter", keyNames: [], docsUrl: "https://openrouter.ai", testSupported: true },
    custom: { label: "Other", icon: "key", keyNames: [], docsUrl: "", testSupported: false },
  },
  KEY_NAME_SUGGESTIONS: [],
}));

vi.mock("@/components/ui/KeyValueField", () => ({
  KeyValueField: ({ value, onChange, disabled }: { value: string; onChange: (v: string) => void; disabled?: boolean }) => (
    <textarea
      data-testid="key-value-field"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      aria-label="Key value"
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

beforeEach(() => {
  mockCreateSecret.mockReset().mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ── Helpers ──────────────────────────────────────────────────────────────────

async function typeKeyName(name: string) {
  const input = screen.getByLabelText("Key name");
  fireEvent.change(input, { target: { value: name } });
  await act(async () => { await Promise.resolve(); });
}

async function typeValue(val: string) {
  const textarea = screen.getByTestId("key-value-field");
  fireEvent.change(textarea, { target: { value: val } });
  await act(async () => { await Promise.resolve(); });
}

// ─── Initial render ─────────────────────────────────────────────────────────

describe("AddKeyForm — initial render", () => {
  it("renders header 'Add New Key'", () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    expect(screen.getByText("Add New Key")).toBeTruthy();
  });

  it("has key name and value inputs", () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    expect(screen.getByLabelText("Key name")).toBeTruthy();
    expect(screen.getByTestId("key-value-field")).toBeTruthy();
  });

  it("Save and Cancel buttons present", () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    expect(screen.getByRole("button", { name: /save key/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeTruthy();
  });

  it("Save button disabled initially", () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    expect((screen.getByRole("button", { name: /save key/i }) as HTMLButtonElement).disabled).toBe(true);
  });
});

// ─── Key name validation ────────────────────────────────────────────────────

describe("AddKeyForm — key name validation", () => {
  it("auto-uppercases key name input", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    const input = screen.getByLabelText("Key name") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "github_token" } });
    expect(input.value).toBe("GITHUB_TOKEN");
  });

  it("shows error for key name starting with digit (invalid UPPER_SNAKE_CASE)", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    // The key name input auto-uppercases, so "123_token" → "123_TOKEN"
    // which fails /^[A-Z][A-Z0-9_]*$/ (must start with uppercase letter)
    const input = screen.getByLabelText("Key name");
    fireEvent.change(input, { target: { value: "123_token" } });
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/upper_snake_case/i)).toBeTruthy();
  });

  it("shows error for key name starting with number", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("123_TOKEN");
    expect(screen.getByText(/upper_snake_case/i)).toBeTruthy();
  });

  it("shows duplicate error when key name already exists", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={["ANTHROPIC_API_KEY"]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/already exists/i)).toBeTruthy();
  });

  it("no error for valid new key name", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("MY_SECRET_KEY");
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

// ─── Provider hint ──────────────────────────────────────────────────────────

describe("AddKeyForm — provider hint", () => {
  it("shows provider hint for ANTHROPIC_API_KEY (known provider)", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByTestId("provider-hint")).toBeTruthy();
    expect(screen.getByText("Anthropic")).toBeTruthy();
  });

  it("shows provider hint for GITHUB_TOKEN", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("GITHUB_TOKEN");
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByTestId("provider-hint")).toBeTruthy();
    expect(screen.getByText("GitHub")).toBeTruthy();
  });

  it("shows provider hint for OPENROUTER_API_KEY", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("OPENROUTER_API_KEY");
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByTestId("provider-hint")).toBeTruthy();
    expect(screen.getByText("OpenRouter")).toBeTruthy();
  });

  it("hides provider hint for unknown custom key name", async () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("MY_CUSTOM_TOKEN");
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByTestId("provider-hint")).toBeNull();
  });
});

// ─── Value validation (debounced) ───────────────────────────────────────────

describe("AddKeyForm — value validation (debounced)", () => {
  it("ValidationHint shown after debounce for invalid value", async () => {
    vi.useFakeTimers();
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    const textarea = screen.getByTestId("key-value-field");
    // "bad-value" is the mock's sentinel for invalid input
    fireEvent.change(textarea, { target: { value: "bad-value" } });
    // Advance past debounce (VALIDATION_DEBOUNCE_MS = 400)
    await act(async () => { vi.advanceTimersByTime(400); });
    expect(screen.getByRole("alert")).toBeTruthy();
    vi.useRealTimers();
  });
});

// ─── Save ───────────────────────────────────────────────────────────────────

describe("AddKeyForm — save", () => {
  it("Save button disabled when key name or value missing", () => {
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    const saveBtn = screen.getByRole("button", { name: /save key/i });
    expect((saveBtn as HTMLButtonElement).disabled).toBe(true);
  });

  it("Save button enabled when valid key name + value", async () => {
    vi.useFakeTimers();
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("GITHUB_FAKE_VALUE_FOR_TEST");
    await act(async () => { vi.advanceTimersByTime(400); });
    const saveBtn = screen.getByRole("button", { name: /save key/i });
    expect((saveBtn as HTMLButtonElement).disabled).toBe(false);
    vi.useRealTimers();
  });

  it("calls createSecret(workspaceId, keyName, value) on save", async () => {
    vi.useFakeTimers();
    render(<AddKeyForm workspaceId="ws-test" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("GITHUB_FAKE_VALUE_FOR_TEST");
    await act(async () => { vi.advanceTimersByTime(400); });
    fireEvent.click(screen.getByRole("button", { name: /save key/i }));
    await act(async () => { vi.advanceTimersByTime(0); });
    expect(mockCreateSecret).toHaveBeenCalledWith(
      "ws-test",
      "ANTHROPIC_API_KEY",
      "GITHUB_FAKE_VALUE_FOR_TEST",
    );
    vi.useRealTimers();
  });

  it("Save button shows 'Saving…' during save", async () => {
    vi.useFakeTimers();
    mockCreateSecret.mockImplementation(() => new Promise(() => {}));
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("GITHUB_FAKE_VALUE_FOR_TEST");
    await act(async () => { vi.advanceTimersByTime(400); });
    fireEvent.click(screen.getByRole("button", { name: /save key/i }));
    await act(async () => { vi.advanceTimersByTime(0); });
    expect(screen.getByRole("button", { name: /saving/i })).toBeTruthy();
    vi.useRealTimers();
  });

  it("shows error on save failure", async () => {
    mockCreateSecret.mockRejectedValue(new Error("network error"));
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("GITHUB_FAKE_VALUE_FOR_TEST");
    fireEvent.click(screen.getByRole("button", { name: /save key/i }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/network error/i)).toBeTruthy();
  });
});

// ─── Cancel ─────────────────────────────────────────────────────────────────

describe("AddKeyForm — cancel", () => {
  it("onCancel called when Cancel button clicked", () => {
    const onCancel = vi.fn();
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalled();
  });

  it("Cancel button disabled during save", async () => {
    vi.useFakeTimers();
    mockCreateSecret.mockImplementation(() => new Promise(() => {}));
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("GITHUB_FAKE_VALUE_FOR_TEST");
    await act(async () => { vi.advanceTimersByTime(400); });
    fireEvent.click(screen.getByRole("button", { name: /save key/i }));
    await act(async () => { vi.advanceTimersByTime(0); });
    expect((screen.getByRole("button", { name: /cancel/i }) as HTMLButtonElement).disabled).toBe(true);
    vi.useRealTimers();
  });
});

// ─── TestConnectionButton ────────────────────────────────────────────────────

describe("AddKeyForm — TestConnectionButton", () => {
  it("TestConnectionButton shown for known provider with valid-format value", async () => {
    vi.useFakeTimers();
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    // Use a value that passes the regex (sk-ant- prefix + 90+ chars)
    const validValue = "GHP_FAKEPLACEHOLDER_NOTREAL_ABCDEFGHIJKLMNOPQRSTUVWXYZ12345678901234567890";
    await typeValue(validValue);
    await act(async () => { vi.advanceTimersByTime(400); });
    expect(screen.getByTestId("test-connection-btn")).toBeTruthy();
    vi.useRealTimers();
  });

  it("TestConnectionButton NOT shown when value is invalid format", async () => {
    vi.useFakeTimers();
    render(<AddKeyForm workspaceId="ws-1" existingNames={[]} onCancel={vi.fn()} />);
    await typeKeyName("ANTHROPIC_API_KEY");
    await typeValue("bad-value");
    await act(async () => { vi.advanceTimersByTime(400); });
    expect(screen.queryByTestId("test-connection-btn")).toBeNull();
    vi.useRealTimers();
  });
});
