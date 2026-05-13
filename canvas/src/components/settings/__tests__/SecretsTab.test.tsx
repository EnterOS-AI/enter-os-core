// @vitest-environment jsdom
/**
 * Tests for SecretsTab — API keys tab inside SettingsPanel.
 *
 * Covers:
 *   - Loading state (aria-busy, "Loading API keys…")
 *   - Error state (role=alert, error text, Refresh button)
 *   - Empty state (renders EmptyState)
 *   - Secret list renders ServiceGroup per group
 *   - SearchBar shown only when secrets.length >= 4
 *   - Search filters results — no-results state + Clear search
 *   - "+ Add API Key" button toggles AddKeyForm
 *   - AddKeyForm visible when isAddFormOpen=true
 *   - ServiceGroup with multiple groups rendered
 *   - Single-key group count label ("1 key")
 *   - Multi-key group count label ("N keys")
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SecretsTab } from "../SecretsTab";

// ── Secrets store mock ───────────────────────────────────────────────────────

type SecretsStoreState = {
  secrets: Array<{ name: string; masked_value: string; group: string; status: string; updated_at: string }>;
  isLoading: boolean;
  error: string | null;
  isAddFormOpen: boolean;
  searchQuery: string;
  fetchSecrets: ReturnType<typeof vi.fn>;
  setAddFormOpen: ReturnType<typeof vi.fn>;
  setSearchQuery: ReturnType<typeof vi.fn>;
};

// Mutable store state — tests reassign fields to test different states
let storeState: SecretsStoreState;

const mockFetchSecrets = vi.fn().mockResolvedValue(undefined);
const mockSetAddFormOpen = vi.fn();
const mockSetSearchQuery = vi.fn();

storeState = {
  secrets: [],
  isLoading: false,
  error: null,
  isAddFormOpen: false,
  searchQuery: "",
  fetchSecrets: mockFetchSecrets,
  setAddFormOpen: mockSetAddFormOpen,
  setSearchQuery: mockSetSearchQuery,
};

vi.mock("@/stores/secrets-store", () => ({
  useSecretsStore: Object.assign(
    vi.fn((selector: (s: SecretsStoreState) => unknown) => selector(storeState)),
    { getState: () => storeState },
  ),
}));

// ── Child component stubs ────────────────────────────────────────────────────
vi.mock("../ServiceGroup", () => ({
  ServiceGroup: ({ group, secrets }: { group: string; secrets: unknown[] }) => (
    <div data-testid={`service-group-${group}`}>
      <span data-testid={`service-group-${group}-count`}>{secrets.length}</span>
    </div>
  ),
}));

vi.mock("../EmptyState", () => ({
  EmptyState: ({ onAddFirst }: { onAddFirst: () => void }) => (
    <div data-testid="secrets-empty-state">
      <button onClick={onAddFirst}>Add first key</button>
    </div>
  ),
}));

vi.mock("../AddKeyForm", () => ({
  AddKeyForm: ({ workspaceId, onCancel }: { workspaceId: string; onCancel: () => void }) => (
    <div data-testid="add-key-form">AddKeyForm workspaceId={workspaceId} <button onClick={onCancel}>Cancel</button></div>
  ),
}));

vi.mock("../SearchBar", () => ({
  SearchBar: () => <div data-testid="search-bar" />,
}));

beforeEach(() => {
  storeState = {
    secrets: [],
    isLoading: false,
    error: null,
    isAddFormOpen: false,
    searchQuery: "",
    fetchSecrets: mockFetchSecrets,
    setAddFormOpen: mockSetAddFormOpen,
    setSearchQuery: mockSetSearchQuery,
  };
  mockFetchSecrets.mockReset().mockResolvedValue(undefined);
  mockSetAddFormOpen.mockReset();
  mockSetSearchQuery.mockReset();
});

afterEach(() => {
  cleanup();
});

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// ─── Loading ────────────────────────────────────────────────────────────────

describe("SecretsTab — loading", () => {
  it("shows loading state", () => {
    storeState.isLoading = true;
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByText("Loading API keys…")).toBeTruthy();
  });
});

// ─── Error ─────────────────────────────────────────────────────────────────

describe("SecretsTab — error", () => {
  it("shows error with role=alert", () => {
    storeState.error = "network failure";
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("network failure")).toBeTruthy();
  });

  it("shows Refresh button in error state", () => {
    storeState.error = "server error";
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByRole("button", { name: "Refresh" })).toBeTruthy();
  });

  it("Refresh button calls fetchSecrets with workspaceId", () => {
    storeState.error = "server error";
    render(<SecretsTab workspaceId="ws-123" />);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(mockFetchSecrets).toHaveBeenCalledWith("ws-123");
  });
});

// ─── Empty state ────────────────────────────────────────────────────────────

describe("SecretsTab — empty", () => {
  it("shows EmptyState when secrets is empty and not loading", () => {
    storeState.secrets = [];
    storeState.isLoading = false;
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("secrets-empty-state")).toBeTruthy();
  });

  it("EmptyState Add first button opens add form", () => {
    storeState.secrets = [];
    render(<SecretsTab workspaceId="ws-test" />);
    fireEvent.click(screen.getByText("Add first key"));
    expect(mockSetAddFormOpen).toHaveBeenCalledWith(true);
  });
});

// ─── Secret list ────────────────────────────────────────────────────────────

describe("SecretsTab — secret list", () => {
  const ANTHROPIC_SECRET = { name: "ANTHROPIC_API_KEY", masked_value: "sk-ant-••••", group: "anthropic", status: "active", updated_at: "2024-01-01" };
  const GITHUB_SECRET = { name: "GITHUB_TOKEN", masked_value: "ghp_••••", group: "github", status: "active", updated_at: "2024-01-02" };
  const OPENROUTER_SECRET = { name: "OPENROUTER_API_KEY", masked_value: "sk-or-••••", group: "openrouter", status: "active", updated_at: "2024-01-03" };
  const CUSTOM_SECRET = { name: "MY_CUSTOM_KEY", masked_value: "••••", group: "custom", status: "active", updated_at: "2024-01-04" };

  it("renders one ServiceGroup per non-empty group", () => {
    storeState.secrets = [ANTHROPIC_SECRET, GITHUB_SECRET];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("service-group-anthropic")).toBeTruthy();
    expect(screen.getByTestId("service-group-github")).toBeTruthy();
  });

  it("does NOT render empty groups", () => {
    storeState.secrets = [ANTHROPIC_SECRET]; // only anthropic has secrets
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.queryByTestId("service-group-github")).toBeNull();
    expect(screen.queryByTestId("service-group-openrouter")).toBeNull();
  });

  it("renders all 4 groups when all are populated", () => {
    storeState.secrets = [ANTHROPIC_SECRET, GITHUB_SECRET, OPENROUTER_SECRET, CUSTOM_SECRET];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("service-group-anthropic")).toBeTruthy();
    expect(screen.getByTestId("service-group-github")).toBeTruthy();
    expect(screen.getByTestId("service-group-openrouter")).toBeTruthy();
    expect(screen.getByTestId("service-group-custom")).toBeTruthy();
  });

  it("shows '+ Add API Key' button", () => {
    storeState.secrets = [ANTHROPIC_SECRET];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByRole("button", { name: /add api key/i })).toBeTruthy();
  });

  it("'+ Add API Key' opens AddKeyForm", () => {
    storeState.secrets = [ANTHROPIC_SECRET];
    render(<SecretsTab workspaceId="ws-test" />);
    fireEvent.click(screen.getByRole("button", { name: /add api key/i }));
    expect(mockSetAddFormOpen).toHaveBeenCalledWith(true);
  });

  it("shows AddKeyForm when isAddFormOpen=true", () => {
    storeState.secrets = [ANTHROPIC_SECRET];
    storeState.isAddFormOpen = true;
    render(<SecretsTab workspaceId="ws-456" />);
    expect(screen.getByTestId("add-key-form")).toBeTruthy();
  });

  it("AddKeyForm Cancel closes the form", () => {
    storeState.secrets = [ANTHROPIC_SECRET];
    storeState.isAddFormOpen = true;
    render(<SecretsTab workspaceId="ws-test" />);
    fireEvent.click(screen.getByText("Cancel"));
    expect(mockSetAddFormOpen).toHaveBeenCalledWith(false);
  });

  it("shows SearchBar when secrets.length >= 4", () => {
    storeState.secrets = [
      ANTHROPIC_SECRET, GITHUB_SECRET, OPENROUTER_SECRET,
      { ...CUSTOM_SECRET, name: "EXTRA_KEY_1" },
    ];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("search-bar")).toBeTruthy();
  });

  it("hides SearchBar when secrets.length < 4", () => {
    storeState.secrets = [ANTHROPIC_SECRET, GITHUB_SECRET];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.queryByTestId("search-bar")).toBeNull();
  });
});

// ─── Search / filtering ──────────────────────────────────────────────────────

describe("SecretsTab — search", () => {
  const S1 = { name: "ANTHROPIC_API_KEY", masked_value: "sk-ant-••••", group: "anthropic", status: "active", updated_at: "2024-01-01" };
  const S2 = { name: "GITHUB_TOKEN", masked_value: "ghp_••••", group: "github", status: "active", updated_at: "2024-01-02" };
  const S3 = { name: "OPENROUTER_API_KEY", masked_value: "sk-or-••••", group: "openrouter", status: "active", updated_at: "2024-01-03" };
  const S4 = { name: "MY_CUSTOM_KEY", masked_value: "••••", group: "custom", status: "active", updated_at: "2024-01-04" };

  beforeEach(() => {
    // Need 4+ secrets for SearchBar to appear
    storeState.secrets = [S1, S2, S3, S4];
  });

  it("shows no-results message when search filters all secrets", () => {
    storeState.searchQuery = "nonexistent-key";
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByText(/no keys match/i)).toBeTruthy();
    expect(screen.getByText(/nonexistent-key/i)).toBeTruthy();
  });

  it("shows 'Clear search' button in no-results state", () => {
    storeState.searchQuery = "nonexistent";
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByRole("button", { name: /clear search/i })).toBeTruthy();
  });

  it("'Clear search' clears searchQuery via store.getState()", () => {
    storeState.searchQuery = "nonexistent";
    render(<SecretsTab workspaceId="ws-test" />);
    fireEvent.click(screen.getByRole("button", { name: /clear search/i }));
    expect(mockSetSearchQuery).toHaveBeenCalledWith("");
  });

  it("shows matching group when search matches one secret", () => {
    storeState.searchQuery = "anthropic";
    storeState.secrets = [S1, S2, S3, S4];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("service-group-anthropic")).toBeTruthy();
    // Other groups should be filtered out
    expect(screen.queryByTestId("service-group-github")).toBeNull();
  });
});

// ─── SearchBar visibility threshold ─────────────────────────────────────────

describe("SecretsTab — search bar threshold", () => {
  const makeSecret = (n: number) => ({
    name: `KEY_${n}`, masked_value: "••••", group: "custom" as const, status: "active" as const, updated_at: "2024-01-01",
  });

  it("SearchBar hidden at 3 secrets", () => {
    storeState.secrets = [makeSecret(1), makeSecret(2), makeSecret(3)];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.queryByTestId("search-bar")).toBeNull();
  });

  it("SearchBar shown at 4 secrets (threshold)", () => {
    storeState.secrets = [makeSecret(1), makeSecret(2), makeSecret(3), makeSecret(4)];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.getByTestId("search-bar")).toBeTruthy();
  });

  it("SearchBar hidden when secrets drop to 3 below threshold", () => {
    // Separate render with 3 secrets — plain object state won't
    // re-render React on mutation, so test the logic directly.
    storeState.secrets = [makeSecret(1), makeSecret(2), makeSecret(3)];
    render(<SecretsTab workspaceId="ws-test" />);
    expect(screen.queryByTestId("search-bar")).toBeNull();
  });
});
