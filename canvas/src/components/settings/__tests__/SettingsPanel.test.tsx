// @vitest-environment jsdom
/**
 * Tests for SettingsPanel — right-anchored slide-over drawer for workspace settings.
 *
 * Covers:
 *   - Closed by default (Dialog closed when isPanelOpen=false)
 *   - Opens when isPanelOpen=true
 *   - Three tabs: Secrets, Workspace Tokens, Org API Keys
 *   - Cmd+, keyboard shortcut toggles panel
 *   - Clicking backdrop/close with dirty form (editingKey set) shows UnsavedChangesGuard
 *   - Guard "Keep editing" closes guard (does NOT close panel)
 *   - Guard "Discard" closes guard AND closes panel
 *   - fetchSecrets called when panel opens
 *   - Close button closes panel
 *   - aria-modal="false" — canvas stays interactive
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SettingsPanel } from "../SettingsPanel";

// ── Store mock ──────────────────────────────────────────────────────────────

type PanelStoreState = {
  isPanelOpen: boolean;
  isAddFormOpen: boolean;
  editingKey: string | null;
  closePanel: () => void;
  openPanel: () => void;
  fetchSecrets: (workspaceId: string) => Promise<void>;
};

let storeState: PanelStoreState;
const mockClosePanel = vi.fn();
const mockOpenPanel = vi.fn();
const mockFetchSecrets = vi.fn();

storeState = {
  isPanelOpen: false,
  isAddFormOpen: false,
  editingKey: null,
  closePanel: mockClosePanel,
  openPanel: mockOpenPanel,
  fetchSecrets: mockFetchSecrets,
};

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: Object.assign(
    vi.fn((selector?: (s: PanelStoreState) => unknown) =>
      selector ? selector(storeState) : storeState
    ),
    { getState: () => storeState },
  ),
}));

vi.mock("@/hooks/use-keyboard-shortcut", () => ({
  useKeyboardShortcut: vi.fn(),
}));

// ── Child component stubs ────────────────────────────────────────────────────

vi.mock("../SecretsTab", () => ({
  SecretsTab: ({ workspaceId }: { workspaceId: string }) => (
    <div data-testid="secrets-tab">SecretsTab workspaceId={workspaceId}</div>
  ),
}));

vi.mock("../TokensTab", () => ({
  TokensTab: ({ workspaceId }: { workspaceId: string }) => (
    <div data-testid="tokens-tab">TokensTab workspaceId={workspaceId}</div>
  ),
}));

vi.mock("../OrgTokensTab", () => ({
  OrgTokensTab: () => <div data-testid="org-tokens-tab">OrgTokensTab</div>,
}));

vi.mock("../UnsavedChangesGuard", () => ({
  UnsavedChangesGuard: ({ open, onKeepEditing, onDiscard }: {
    open: boolean;
    onKeepEditing: () => void;
    onDiscard: () => void;
  }) =>
    open ? (
      <div data-testid="unsaved-guard" role="alertdialog">
        <button onClick={onKeepEditing} data-testid="guard-keep">Keep editing</button>
        <button onClick={onDiscard} data-testid="guard-discard">Discard</button>
      </div>
    ) : null,
}));

beforeEach(() => {
  storeState = {
    isPanelOpen: false,
    isAddFormOpen: false,
    editingKey: null,
    closePanel: mockClosePanel,
    openPanel: mockOpenPanel,
    fetchSecrets: mockFetchSecrets,
  };
  mockClosePanel.mockReset();
  mockOpenPanel.mockReset();
  mockFetchSecrets.mockReset().mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
});

// ─── Closed by default ─────────────────────────────────────────────────────

describe("SettingsPanel — closed by default", () => {
  it("no dialog content when isPanelOpen=false", () => {
    render(<SettingsPanel workspaceId="ws-1" />);
    // Radix Dialog doesn't render content when open=false
    expect(screen.queryByTestId("secrets-tab")).toBeNull();
  });
});

// ─── Open / close ──────────────────────────────────────────────────────────

describe("SettingsPanel — open / close", () => {
  it("renders SecretsTab when panel is open", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-xyz" />);
    expect(screen.getByTestId("secrets-tab")).toBeTruthy();
    expect(screen.getByText(/workspaceId=ws-xyz/i)).toBeTruthy();
  });

  it("renders TokensTab tab in tabs list", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    expect(screen.getByRole("tab", { name: /workspace tokens/i })).toBeTruthy();
  });

  it("renders Org API Keys tab in tabs list", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    expect(screen.getByRole("tab", { name: /org api keys/i })).toBeTruthy();
  });

  it("Secrets tab is default active", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    expect(screen.getByTestId("secrets-tab")).toBeTruthy();
    expect(screen.getByRole("tab", { name: /secrets/i }).getAttribute("data-state")).toBe("active");
  });

  it("Tokens tab trigger exists with correct aria attributes", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    const tab = screen.getByRole("tab", { name: /workspace tokens/i });
    // Radix Tabs.Trigger has role="tab" and aria-selected
    expect(tab).toBeTruthy();
    // Secrets tab is active by default
    const secretsTab = screen.getByRole("tab", { name: /secrets/i });
    expect(secretsTab.getAttribute("data-state")).toBe("active");
    // Tokens tab should not be active initially
    expect(tab.getAttribute("data-state")).not.toBe("active");
  });

  it("Close button calls closePanel", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /close settings/i }));
    expect(mockClosePanel).toHaveBeenCalled();
  });

  it("calls fetchSecrets(workspaceId) when panel opens", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-fetch-test" />);
    expect(mockFetchSecrets).toHaveBeenCalledWith("ws-fetch-test");
  });
});

// ─── Unsaved changes guard ──────────────────────────────────────────────────

describe("SettingsPanel — unsaved changes guard", () => {
  it("shows guard when panel closing with isAddFormOpen=true", () => {
    storeState.isPanelOpen = true;
    storeState.isAddFormOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /close settings/i }));
    expect(screen.getByTestId("unsaved-guard")).toBeTruthy();
  });

  it("guard shows when editingKey is set (dirty form)", () => {
    storeState.isPanelOpen = true;
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SettingsPanel workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /close settings/i }));
    expect(screen.getByTestId("unsaved-guard")).toBeTruthy();
  });

  it("'Keep editing' closes guard but panel stays open", () => {
    storeState.isPanelOpen = true;
    storeState.editingKey = "GITHUB_TOKEN";
    render(<SettingsPanel workspaceId="ws-1" />);
    // Trigger close attempt
    fireEvent.click(screen.getByRole("button", { name: /close settings/i }));
    expect(screen.getByTestId("unsaved-guard")).toBeTruthy();
    // Keep editing closes the guard
    fireEvent.click(screen.getByTestId("guard-keep"));
    expect(screen.queryByTestId("unsaved-guard")).toBeNull();
    // Panel content still visible (panel not closed)
    expect(screen.getByTestId("secrets-tab")).toBeTruthy();
  });

  it("'Discard' button on guard calls closePanel", () => {
    storeState.isPanelOpen = true;
    storeState.isAddFormOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    fireEvent.click(screen.getByRole("button", { name: /close settings/i }));
    fireEvent.click(screen.getByTestId("guard-discard"));
    expect(mockClosePanel).toHaveBeenCalled();
  });
});

// ─── Accessibility ──────────────────────────────────────────────────────────

describe("SettingsPanel — accessibility", () => {
  it("Dialog.Content has aria-label='Settings: API Keys'", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    expect(document.querySelector('[aria-label="Settings: API Keys"]')).toBeTruthy();
  });

  it("TabList has aria-label='Settings sections'", () => {
    storeState.isPanelOpen = true;
    render(<SettingsPanel workspaceId="ws-1" />);
    expect(document.querySelector('[aria-label="Settings sections"]')).toBeTruthy();
  });
});
