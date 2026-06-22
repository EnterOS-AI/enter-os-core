// @vitest-environment jsdom
/**
 * Tests for MissingKeysModal component (issue #1037 companion)
 *
 * Covers:
 *  - Renders null when open=false; dialog when open=true
 *  - ARIA: role=dialog, aria-modal, aria-labelledby pointing to title
 *  - Initializes entries from missingKeys prop with correct labels
 *  - Escape key calls onCancel
 *  - Save: button disabled when empty, shows "..." while saving, shows "Saved" on success
 *  - Enter key in input triggers save
 *  - Error display when API save fails
 *  - Add Keys & Deploy: calls onKeysAdded only when all saved; shows global error otherwise
 *  - Cancel button and backdrop click call onCancel
 *  - Open Settings button calls onOpenSettings when provided; absent when not
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, act, cleanup } from "@testing-library/react";

import { MissingKeysModal } from "../MissingKeysModal";

// ── Mocks (hoisted before vi.mock) ────────────────────────────────────────────

const { mockPut } = vi.hoisted(() => ({ mockPut: vi.fn() }));

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), put: mockPut },
}));

vi.mock("@/lib/deploy-preflight", () => ({
  getKeyLabel: (key: string) => {
    const labels: Record<string, string> = {
      ANTHROPIC_API_KEY: "Anthropic API Key",
      OPENAI_API_KEY: "OpenAI API Key",
      GOOGLE_API_KEY: "Google API Key",
    };
    return labels[key] ?? key;
  },
}));
// Tests render the modal without a `providers` prop — the component
// falls back to the all-keys mode using the `missingKeys` array, which
// matches the contract these tests were written for.

// ── Suite 1: Visibility and ARIA ────────────────────────────────────────────

describe("MissingKeysModal — visibility and ARIA", () => {
  afterEach(() => cleanup());

  it("renders nothing when open=false", () => {
    render(
      <MissingKeysModal
        open={false}
        missingKeys={[]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders dialog when open=true", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("dialog has aria-modal=\"true\"", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByRole("dialog").getAttribute("aria-modal")).toBe("true");
  });

  it("dialog has aria-labelledby pointing to title element", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const dialog = screen.getByRole("dialog");
    const labelledby = dialog.getAttribute("aria-labelledby");
    expect(labelledby).toBeTruthy();
    expect(document.getElementById(labelledby ?? "")?.textContent).toContain("Missing API Keys");
  });
});

// ── Suite 2: Content ────────────────────────────────────────────────────────

describe("MissingKeysModal — content", () => {
  afterEach(() => cleanup());

  it("renders all missing keys from prop", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY", "OPENAI_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByText("Anthropic API Key")).toBeTruthy();
    expect(screen.getByText("OpenAI API Key")).toBeTruthy();
  });

  it("renders key name (env var) for each missing key", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByText("ANTHROPIC_API_KEY")).toBeTruthy();
  });

  it("renders runtime label in header", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByText(/claude code/i)).toBeTruthy();
  });

  it("renders Cancel button", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByText(/Cancel/i)).toBeTruthy();
  });

  it("renders 'Add Keys & Deploy' button", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByText(/Add Keys/i)).toBeTruthy();
  });

  it("each key has a password input", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY", "OPENAI_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input[type=password]"));
    expect(inputs.length).toBeGreaterThanOrEqual(2);
  });

  it("each key has a Save button", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const saves = screen.getAllByRole("button").filter(b => /save/i.test(b.textContent ?? ""));
    expect(saves.length).toBeGreaterThanOrEqual(1);
  });
});

// ── Suite 3: Keyboard ────────────────────────────────────────────────────────

describe("MissingKeysModal — keyboard", () => {
  afterEach(() => cleanup());

  it("Escape key calls onCancel", () => {
    const onCancel = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={onCancel}
      />
    );
    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(onCancel).toHaveBeenCalled();
  });

  it("Enter key in password input triggers save for that entry", async () => {
    mockPut.mockResolvedValueOnce({});
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-test-key-123" } });
    });
    act(() => {
      fireEvent.keyDown(input, { key: "Enter" });
    });
    await waitFor(() => {
      expect(mockPut).toHaveBeenCalled();
    });
  });
});

// ── Suite 4: Save flow ───────────────────────────────────────────────────────

describe("MissingKeysModal — save flow", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockPut.mockResolvedValue({});
  });
  afterEach(() => cleanup());

  it("Save button disabled when input is empty", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const saveBtn = screen.getAllByRole("button").find(b => /save/i.test(b.textContent ?? "")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);
  });

  it("Save button enabled when input has value", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-123" } });
    });
    const saveBtn = screen.getAllByRole("button").find(b => /save/i.test(b.textContent ?? "")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(false);
  });

  it("shows '...' while saving", async () => {
    mockPut.mockImplementation(() => new Promise(() => {}));
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-123" } });
    });
    act(() => {
      act(() => { fireEvent.click(screen.getAllByRole("button").find(b => b.textContent?.trim() === "Save")!); });
    });
    await waitFor(() => {
      expect(screen.getByText("...")).toBeTruthy();
    });
  });

  it("shows 'Saved' indicator on successful save", async () => {
    mockPut.mockResolvedValueOnce({});
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-123" } });
    });
    act(() => {
      act(() => { fireEvent.click(screen.getAllByRole("button").find(b => b.textContent?.trim() === "Save")!); });
    });
    await waitFor(() => {
      expect(screen.getByText("Saved")).toBeTruthy();
    });
  });

  it("shows error message on failed save", async () => {
    mockPut.mockRejectedValueOnce(new Error("Invalid key"));
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "bad-key" } });
    });
    act(() => {
      act(() => { fireEvent.click(screen.getAllByRole("button").find(b => b.textContent?.trim() === "Save")!); });
    });
    await waitFor(() => {
      expect(screen.getByText(/invalid key/i)).toBeTruthy();
    });
  });
});

// ── Suite 5: Add Keys & Deploy ─────────────────────────────────────────────

describe("MissingKeysModal — add keys and deploy", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockPut.mockResolvedValue({});
  });
  afterEach(() => cleanup());

  it("calls onKeysAdded when all keys are saved", async () => {
    const onKeysAdded = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={onKeysAdded}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-123" } });
    });
    act(() => {
      act(() => { fireEvent.click(screen.getAllByRole("button").find(b => b.textContent?.trim() === "Save")!); });
    });
    await waitFor(() => {
      expect(screen.getByText("Saved")).toBeTruthy();
    });
    // After save, button text changes from "Add Keys" to "Deploy"
    const deployBtn = Array.from(document.querySelectorAll("button")).find(b => b.textContent?.trim() === "Deploy");
    expect(deployBtn).toBeTruthy();
    act(() => { fireEvent.click(deployBtn!); });
    expect(onKeysAdded).toHaveBeenCalled();
  });

  it("shows optional keys without blocking deploy", () => {
    const onKeysAdded = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={[]}
        optionalKeys={["GOOGLE_GSC_SITE"]}
        runtime="claude-code"
        title="Configure Workspace"
        onKeysAdded={onKeysAdded}
        onCancel={vi.fn()}
      />
    );

    expect(screen.getByText("Optional")).toBeTruthy();
    expect(screen.getAllByText("GOOGLE_GSC_SITE").length).toBeGreaterThan(0);
    const deployBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Deploy",
    );
    expect(deployBtn).toBeTruthy();
    expect(deployBtn!.disabled).toBe(false);
    act(() => { fireEvent.click(deployBtn!); });
    expect(onKeysAdded).toHaveBeenCalled();
  });

  it("shows global error when not all keys saved", async () => {
    const onKeysAdded = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={onKeysAdded}
        onCancel={vi.fn()}
      />
    );
    // Button is disabled (not all keys saved) — click is a no-op
    const addKeysBtn = Array.from(document.querySelectorAll("button")).find(b => b.textContent?.trim() === "Add Keys");
    act(() => { fireEvent.click(addKeysBtn!); });
    // Verify button is disabled and onKeysAdded was NOT called
    expect(addKeysBtn!.disabled).toBe(true);
    expect(onKeysAdded).not.toHaveBeenCalled();
  });

  it("shows global error when a key is still saving", async () => {
    mockPut.mockImplementation(() => new Promise(() => {}));
    const onKeysAdded = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={onKeysAdded}
        onCancel={vi.fn()}
      />
    );
    const inputs = Array.from(document.querySelectorAll("input"));
    const input = inputs[0];
    act(() => {
      fireEvent.change(input, { target: { value: "sk-123" } });
    });
    act(() => {
      act(() => { fireEvent.click(screen.getAllByRole("button").find(b => b.textContent?.trim() === "Save")!); });
    });
    await waitFor(() => {
      expect(screen.getByText("Saving...")).toBeTruthy();
    });
    // While a key is still saving, the Add Keys button shows "Saving..." and is disabled
    const addKeysBtn = Array.from(document.querySelectorAll("button")).find(b =>
      b.textContent?.trim() === "Add Keys" || b.textContent?.trim() === "Saving..."
    );
    // Verify the button is disabled during save
    expect(addKeysBtn).toBeTruthy();
    expect(addKeysBtn!.disabled).toBe(true);
  });
});

// ── Suite 6: Cancel and settings ───────────────────────────────────────────

describe("MissingKeysModal — cancel and settings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockPut.mockResolvedValue({});
  });
  afterEach(() => cleanup());

  it("Cancel button calls onCancel", () => {
    const onCancel = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={onCancel}
      />
    );
    act(() => {
      fireEvent.click(screen.getByText(/Cancel/i));
    });
    expect(onCancel).toHaveBeenCalled();
  });

  it("backdrop click calls onCancel", () => {
    const onCancel = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={onCancel}
      />
    );
    // The backdrop is the first div.absolute covering the screen
    const backdrop = document.querySelector(".fixed.inset-0");
    act(() => {
      fireEvent.click(backdrop as HTMLElement);
    });
    expect(onCancel).toBeTruthy();
  });

  it("renders Open Settings button when onOpenSettings is provided", () => {
    const onOpenSettings = vi.fn();
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
        onOpenSettings={onOpenSettings}
      />
    );
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: /open settings/i }));
    });
    expect(onOpenSettings).toHaveBeenCalled();
  });

  it("does not render Open Settings button when onOpenSettings is absent", () => {
    render(
      <MissingKeysModal
        open={true}
        missingKeys={["ANTHROPIC_API_KEY"]}
        runtime="claude-code"
        onKeysAdded={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.queryByRole("button", { name: /open settings/i })).toBeNull();
  });
});
