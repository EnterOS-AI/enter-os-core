// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { CreateWorkspaceButton, HERMES_PROVIDERS } from "../CreateWorkspaceDialog";

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

import { api } from "@/lib/api";

const mockGet = vi.mocked(api.get);
const mockPost = vi.mocked(api.post);

const SAMPLE_WORKSPACES = [
  { id: "ws-1", name: "Platform Team", tier: 1 },
  { id: "ws-2", name: "Research Agent", tier: 2 },
];

beforeEach(() => {
  vi.clearAllMocks();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockGet.mockResolvedValue(SAMPLE_WORKSPACES as any);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockPost.mockResolvedValue({} as any);
});

afterEach(() => {
  cleanup();
});

async function openDialog() {
  render(<CreateWorkspaceButton />);
  const btn = screen.getAllByRole("button").find((b) => b.textContent?.includes("New Workspace"));
  expect(btn).toBeTruthy();
  fireEvent.click(btn!);
  await waitFor(() => expect(screen.getByText("Create Workspace")).toBeTruthy());
}

async function setTemplate(value: string) {
  fireEvent.change(
    screen.getByPlaceholderText("e.g. seo-agent (from workspace-configs-templates/)"),
    { target: { value } }
  );
}

describe("CreateWorkspaceDialog", () => {
  it("opens the dialog when New Workspace button is clicked", async () => {
    await openDialog();
    expect(screen.getByText("Create Workspace")).toBeTruthy();
  });

  it("renders a <select> for parent workspace — not a text input", async () => {
    await openDialog();
    const selects = document.querySelectorAll("select");
    expect(selects.length).toBeGreaterThanOrEqual(1);
    // The old raw UUID text input is gone
    expect(screen.queryByPlaceholderText("Leave empty for root-level")).toBeNull();
  });

  it('first option is "None (root level)" with empty value', async () => {
    await openDialog();
    const select = document.querySelector("select") as HTMLSelectElement;
    expect(select).toBeTruthy();
    const firstOption = select.options[0];
    expect(firstOption.value).toBe("");
    expect(firstOption.text.trim()).toBe("None (root level)");
  });

  it("populates select with workspace names from GET /workspaces", async () => {
    await openDialog();
    await waitFor(() => {
      const select = document.querySelector("select") as HTMLSelectElement;
      const optionValues = Array.from(select.options).map((o) => o.value);
      expect(optionValues).toContain("ws-1");
      expect(optionValues).toContain("ws-2");
    });
    const select = document.querySelector("select") as HTMLSelectElement;
    const optionTexts = Array.from(select.options).map((o) => o.text.trim());
    expect(optionTexts.some((t) => t.includes("Platform Team"))).toBe(true);
    expect(optionTexts.some((t) => t.includes("Research Agent"))).toBe(true);
  });

  it("sends parent_id in POST body when a workspace is selected", async () => {
    await openDialog();
    await waitFor(() => {
      const select = document.querySelector("select") as HTMLSelectElement;
      expect(select.options.length).toBeGreaterThan(1);
    });

    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "My Agent" },
    });

    const select = document.querySelector("select") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "ws-1" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.parent_id).toBe("ws-1");
  });

  it("sends parent_id as undefined when None (root level) is selected", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Root Agent" },
    });

    const select = document.querySelector("select") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.parent_id).toBeUndefined();
  });

  it("renders gracefully when GET /workspaces fails", async () => {
    mockGet.mockRejectedValueOnce(new Error("Network error"));
    await openDialog();

    // Dialog still renders; select exists with only the root option
    await waitFor(() => {
      const select = document.querySelector("select") as HTMLSelectElement;
      expect(select.options.length).toBe(1);
      expect(select.options[0].value).toBe("");
    });
  });
});

// ---------------------------------------------------------------------------
// Hermes provider picker tests
// ---------------------------------------------------------------------------

describe("CreateWorkspaceDialog — Hermes provider picker", () => {
  it("does NOT show hermes provider section for non-hermes templates", async () => {
    await openDialog();
    await setTemplate("seo-agent");
    expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeNull();
  });

  it("shows hermes provider section when template is 'hermes'", async () => {
    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
  });

  it("shows hermes provider section for template 'HERMES' (case-insensitive)", async () => {
    await openDialog();
    await setTemplate("HERMES");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
  });

  it("hermes provider dropdown defaults to 'anthropic'", async () => {
    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    expect(providerSelect).toBeTruthy();
    expect(providerSelect.value).toBe("anthropic");
  });

  it("hermes provider dropdown lists all 15 providers", async () => {
    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    expect(providerSelect.options.length).toBe(HERMES_PROVIDERS.length);
    const ids = Array.from(providerSelect.options).map((o) => o.value);
    expect(ids).toContain("anthropic");
    expect(ids).toContain("openai");
    expect(ids).toContain("gemini");
    expect(ids).toContain("deepseek");
    expect(ids).toContain("hermes");
  });

  // Pins the dynamic-providers behavior: when the matched template's
  // /templates row declares `providers`, the dropdown filters to that
  // subset instead of showing the full HERMES_PROVIDERS catalog. Same
  // data source ConfigTab uses (PR #2454) — keeps the modal and the
  // settings tab honest about which providers a template supports.
  it("hermes provider dropdown filters to template-declared providers when /templates ships them", async () => {
    // Per-URL mock: /workspaces returns the existing fixture, /templates
    // returns a hermes row that only allows anthropic + minimax + openai.
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/templates") {
        return [
          { id: "hermes", name: "Hermes", runtime: "hermes", providers: ["anthropic", "minimax", "openai"] },
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        ] as any;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_WORKSPACES as any;
    });

    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    // Filtered list arrives async after /templates fetch resolves —
    // keep waiting until the dropdown shrinks below the full catalog.
    await waitFor(() => expect(providerSelect.options.length).toBe(3));
    const ids = Array.from(providerSelect.options).map((o) => o.value);
    expect(ids).toEqual(expect.arrayContaining(["anthropic", "minimax", "openai"]));
    expect(ids).not.toContain("gemini");
    expect(ids).not.toContain("deepseek");
  });

  // Back-compat: a template that hasn't migrated to runtime_config.providers
  // (older templates, self-hosted setups without /templates server) keeps
  // showing the full provider catalog. Operators picking from those
  // templates can't be locked out of providers we know hermes supports.
  it("hermes provider dropdown falls back to all providers when template declares no providers list", async () => {
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/templates") {
        // No `providers` field — empty/missing → fall back to full catalog.
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        return [{ id: "hermes", name: "Hermes", runtime: "hermes" }] as any;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_WORKSPACES as any;
    });

    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    expect(providerSelect.options.length).toBe(HERMES_PROVIDERS.length);
  });

  // Defensive: a template's declared list with NO matches against our
  // static catalog (e.g. a brand-new provider id we don't have label/
  // envVar metadata for yet) must not render an empty <select> — the
  // operator can't pick a provider, the form locks. Component falls
  // back to the full catalog so the user can still proceed.
  it("hermes provider dropdown falls back to all providers when template declares only unknown providers", async () => {
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/templates") {
        return [
          { id: "hermes", name: "Hermes", runtime: "hermes", providers: ["totally-new-provider-2030"] },
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        ] as any;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_WORKSPACES as any;
    });

    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    // Stays at full catalog length — no flapping to 0 then back.
    expect(providerSelect.options.length).toBe(HERMES_PROVIDERS.length);
  });

  it("hermes API key field is a password input (masked)", async () => {
    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );
    const keyInput = document.getElementById("hermes-api-key-input") as HTMLInputElement;
    expect(keyInput).toBeTruthy();
    expect(keyInput.type).toBe("password");
  });

  it("shows an error if hermes template is set but API key is empty on submit", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes Agent" },
    });
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );

    // Submit without API key
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain("API key");
    });
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("includes secrets in POST body with correct env var for selected provider", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes Agent" },
    });
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );

    // Fill in the API key
    const keyInput = document.getElementById("hermes-api-key-input") as HTMLInputElement;
    fireEvent.change(keyInput, { target: { value: "sk-test-anthropic-key" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.secrets).toEqual({ ANTHROPIC_API_KEY: "sk-test-anthropic-key" });
    expect(body.template).toBe("hermes");
  });

  it("uses the correct env var when a non-default provider is selected", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes OpenAI" },
    });
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );

    // Switch to openai
    const providerSelect = document.getElementById("hermes-provider-select") as HTMLSelectElement;
    fireEvent.change(providerSelect, { target: { value: "openai" } });

    const keyInput = document.getElementById("hermes-api-key-input") as HTMLInputElement;
    fireEvent.change(keyInput, { target: { value: "sk-openai-test" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.secrets).toEqual({ OPENAI_API_KEY: "sk-openai-test" });
  });

  it("does NOT include secrets field when template is not hermes", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Normal Agent" },
    });
    await setTemplate("seo-agent");

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.secrets).toBeUndefined();
  });

  it("hides hermes section and resets state when template is cleared", async () => {
    await openDialog();
    await setTemplate("hermes");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeTruthy()
    );

    // Clear template
    await setTemplate("");
    await waitFor(() =>
      expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeNull()
    );
  });
});

// ---------------------------------------------------------------------------
// budget_limit field tests (#541)
// ---------------------------------------------------------------------------

describe("CreateWorkspaceDialog — budget_limit field", () => {
  it("renders a Budget limit (USD) input", async () => {
    await openDialog();
    const budgetInput = screen.getByPlaceholderText("e.g. 100");
    expect(budgetInput).toBeTruthy();
  });

  it("renders helper text 'Leave blank for unlimited'", async () => {
    await openDialog();
    expect(screen.getByText("Leave blank for unlimited")).toBeTruthy();
  });

  it("sends budget_limit as a number when a value is entered", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Budget Agent" },
    });
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "250" },
    });
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBe(250);
  });

  it("sends budget_limit as null when the field is left blank", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Unlimited Agent" },
    });
    // Leave budget_limit empty
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBeNull();
  });

  it("sends budget_limit as a float when a decimal value is entered", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Float Budget Agent" },
    });
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "49.99" },
    });
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBeCloseTo(49.99);
  });

  it("resets budget_limit to empty when dialog is reopened", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "500" },
    });

    // Close dialog
    const cancelBtn = screen.getAllByRole("button").find((b) =>
      b.textContent === "Cancel"
    );
    fireEvent.click(cancelBtn!);
    cleanup();

    // Re-open
    await openDialog();
    const budgetInput = screen.getByPlaceholderText("e.g. 100") as HTMLInputElement;
    expect(budgetInput.value).toBe("");
  });
});
