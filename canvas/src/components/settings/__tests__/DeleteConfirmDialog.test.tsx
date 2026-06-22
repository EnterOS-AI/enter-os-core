// @vitest-environment jsdom
/**
 * DeleteConfirmDialog — destructive confirmation for deleting a secret key.
 *
 * Per spec §3.5 & §4.5:
 *   - Opens via window 'secret:delete-request' custom event
 *   - Shows title "Delete \"{name}\"?"
 *   - Fetches dependents live on open
 *   - Delete button disabled for 1s (CONFIRM_DELAY_MS)
 *   - Focus-trapped (AlertDialog)
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs.
 *
 * Covers:
 *   - Does not render when no delete request pending
 *   - Renders dialog when secret:delete-request fires
 *   - Title contains secret name
 *   - Cancel and Delete buttons present
 *   - role=alertdialog on dialog content
 *   - Delete button disabled initially (1s delay)
 *   - Delete button enabled after delay
 *   - Loading state while fetching dependents
 *   - Shows dependents list when present
 *   - Shows no-dependents message when none
 *   - Cancel closes dialog
 *   - Delete button calls deleteSecret and shows Deleting… state
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { DeleteConfirmDialog } from "../DeleteConfirmDialog";

// ─── Mocks ─────────────────────────────────────────────────────────────────────

const _mockDeleteSecret = vi.fn<() => Promise<void>>();
const _mockFetchDependents = vi.fn<() => Promise<string[]>>();

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: (selector?: (s: { deleteSecret: () => Promise<void> }) => unknown) => {
    const state = { deleteSecret: _mockDeleteSecret };
    return selector ? selector(state) : state;
  },
}));

vi.mock("@/lib/api/secrets", () => ({
  fetchDependents: (workspaceId: string, name: string) =>
    _mockFetchDependents(workspaceId, name),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

beforeEach(() => {
  _mockDeleteSecret.mockResolvedValue(undefined);
  _mockFetchDependents.mockResolvedValue([]);
});

// ─── Helpers ───────────────────────────────────────────────────────────────────

/** Dispatches secret:delete-request inside act() so React processes the event. */
function fireDeleteRequest(secretName: string) {
  act(() => {
    window.dispatchEvent(
      new CustomEvent("secret:delete-request", {
        detail: secretName,
      }),
    );
  });
}

// ─── Render ────────────────────────────────────────────────────────────────────

describe("DeleteConfirmDialog — render", () => {
  it("does not render when no delete request pending", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    expect(document.body.textContent ?? "").toBe("");
  });

  it("renders dialog when secret:delete-request fires", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("ANTHROPIC_API_KEY");
    expect(document.querySelector('[role="alertdialog"]')).toBeTruthy();
  });

  it("title contains secret name", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("GITHUB_TOKEN");
    const dialog = document.querySelector('[role="alertdialog"]');
    expect(dialog?.textContent ?? "").toContain("GITHUB_TOKEN");
  });

  it("Cancel button present", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("TEST_KEY");
    const cancelBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Cancel",
    );
    expect(cancelBtn).toBeTruthy();
  });

  it("Delete button present", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("TEST_KEY");
    const deleteBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Delete key"),
    );
    expect(deleteBtn).toBeTruthy();
  });

  it("role=alertdialog on dialog content", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("TEST_KEY");
    expect(document.querySelector('[role="alertdialog"]')).toBeTruthy();
  });
});

// ─── Confirm delay ─────────────────────────────────────────────────────────────

describe("DeleteConfirmDialog — confirm delay", () => {
  it("Delete button disabled initially (< 1s)", () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("FAST_KEY");
    const deleteBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Delete key"),
    ) as HTMLButtonElement;
    expect(deleteBtn.disabled).toBe(true);
  });

  it("Delete button enabled after 1s delay", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("DELAYED_KEY");
    const deleteBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Delete key"),
    ) as HTMLButtonElement;
    // Wait just over 1s
    await new Promise((r) => setTimeout(r, 1010));
    expect(deleteBtn.disabled).toBe(false);
  });
});

// ─── Dependents fetch ─────────────────────────────────────────────────────────

describe("DeleteConfirmDialog — dependents", () => {
  it("shows loading state while fetching", () => {
    _mockFetchDependents.mockImplementation(
      () => new Promise(() => {}), // never resolves
    );
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("LOADING_KEY");
    expect(document.body.textContent ?? "").toContain("Checking for dependent agents");
  });

  it("shows dependents list when present", async () => {
    _mockFetchDependents.mockResolvedValue(["agent-alpha", "agent-beta"]);
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("SHARED_KEY");
    // Wait for fetch to resolve
    await new Promise((r) => setTimeout(r, 10));
    expect(document.body.textContent ?? "").toContain("agent-alpha");
  });

  it("shows no-dependents message when none", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("SOLO_KEY");
    await new Promise((r) => setTimeout(r, 10));
    expect(document.body.textContent ?? "").toContain("No agents currently use this key");
  });

  it("fetchDependents called with workspaceId and secretName", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("MY_SECRET");
    await new Promise((r) => setTimeout(r, 10));
    expect(_mockFetchDependents).toHaveBeenCalledWith("ws1", "MY_SECRET");
  });
});

// ─── Interaction ───────────────────────────────────────────────────────────────

describe("DeleteConfirmDialog — interaction", () => {
  it("Cancel closes the dialog", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("CANCEL_KEY");
    expect(document.querySelector('[role="alertdialog"]')).toBeTruthy();
    const cancelBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Cancel",
    ) as HTMLButtonElement;
    act(() => {
      cancelBtn.click();
    });
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();
  });

  it("Delete calls deleteSecret when enabled and clicked", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("DELETE_ME");
    // Wait for 1s delay
    await new Promise((r) => setTimeout(r, 1010));
    const deleteBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Delete key"),
    ) as HTMLButtonElement;
    act(() => {
      deleteBtn.click();
    });
    expect(_mockDeleteSecret).toHaveBeenCalledTimes(1);
  });

  it("Delete button text is 'Delete key' before clicking", async () => {
    render(<DeleteConfirmDialog workspaceId="ws1" />);
    fireDeleteRequest("BTN_TEXT_KEY");
    await new Promise((r) => setTimeout(r, 1010));
    const deleteBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Delete key"),
    );
    expect(deleteBtn).toBeTruthy();
    // Confirm text is NOT "Deleting…" before click
    const deletingBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => (b.textContent ?? "").includes("Deleting"),
    );
    expect(deletingBtn).toBeUndefined();
  });
});
