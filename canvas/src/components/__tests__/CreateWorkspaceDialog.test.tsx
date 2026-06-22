// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { CreateWorkspaceButton } from "../CreateWorkspaceDialog";
import { isPlatformManagedProvider } from "../ProviderModelSelector";

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

import { api } from "@/lib/api";

const mockGet = vi.mocked(api.get);
const mockPost = vi.mocked(api.post);

const SAMPLE_COMPUTE_METADATA = {
  providers: ["aws", "hetzner", "gcp"],
  instanceTypes: {
    aws: ["t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge", "m6i.large", "m6i.xlarge", "c6i.xlarge"],
    hetzner: ["cpx11", "cpx21", "cpx31", "cpx41", "cpx51", "cax11", "cax21", "cax31", "cax41"],
    gcp: ["e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8"],
  },
  defaults: {
    aws: "t3.medium",
    hetzner: "cpx31",
    gcp: "e2-standard-2",
  },
  display_defaults: {
    aws: "t3.xlarge",
    hetzner: "cpx41",
    gcp: "e2-standard-4",
  },
};

const SAMPLE_WORKSPACES = [
  { id: "ws-1", name: "Platform Team", tier: 1 },
  { id: "ws-2", name: "Research Agent", tier: 2 },
];

const SAMPLE_TEMPLATES = [
  {
    id: "claude-code-default",
    name: "Claude Code Agent",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax", "kimi-coding", "anthropic", "anthropic-oauth"],
    models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
      { id: "kimi-k2-turbo-preview", name: "Kimi K2 Turbo Preview", required_env: ["KIMI_API_KEY"] },
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "sonnet", name: "Claude Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "opus", name: "Claude Opus", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "haiku", name: "Claude Haiku", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
    ],
  },
  {
    id: "seo-agent",
    name: "SEO Agent",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax", "kimi-coding", "anthropic", "anthropic-oauth"],
    models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
      { id: "kimi-k2-turbo-preview", name: "Kimi K2 Turbo Preview", required_env: ["KIMI_API_KEY"] },
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "sonnet", name: "Claude Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "opus", name: "Claude Opus", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "haiku", name: "Claude Haiku", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
    ],
  },
  {
    id: "hermes",
    name: "Hermes",
    runtime: "hermes",
    model: "openai/gpt-4o",
    providers: ["openai", "anthropic", "platform"],
    models: [
      { id: "openai/gpt-4o", name: "GPT-4o", required_env: ["OPENAI_API_KEY"] },
      { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
    ],
  },
  // #2245 fixtures. The real registry `platform` provider declares
  // MOLECULE_LLM_USAGE_TOKEN in its auth_env — the default mock above masks the
  // bug by using required_env:[]. This template gives the platform provider a
  // non-empty auth env (matching production) so the credential-suppression
  // logic is actually exercised.
  {
    id: "platform-managed-test",
    name: "Platform Managed Test",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax"],
    models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: ["MOLECULE_LLM_USAGE_TOKEN"] },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
    ],
  },
  // BYOK-only template (no platform provider) — the credential requirement
  // MUST still hold for these (no-regression guard).
  {
    id: "byok-only-test",
    name: "BYOK Only Test",
    runtime: "claude-code",
    model: "openai/gpt-4o",
    providers: ["openai"],
    models: [
      { id: "openai/gpt-4o", name: "GPT-4o", required_env: ["OPENAI_API_KEY"] },
    ],
  },
];

beforeEach(() => {
  vi.clearAllMocks();
  mockGet.mockImplementation(async (url: string) => {
    if (url === "/templates") {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_TEMPLATES as any;
    }
    if (url === "/compute/metadata") {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_COMPUTE_METADATA as any;
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    return SAMPLE_WORKSPACES as any;
  });
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
    screen.getByLabelText("Workspace Template"),
    { target: { value } }
  );
}

async function setRuntime(value: string) {
  fireEvent.change(
    screen.getByLabelText("Runtime"),
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
    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    expect(select).toBeTruthy();
    const firstOption = select.options[0];
    expect(firstOption.value).toBe("");
    expect(firstOption.text.trim()).toBe("None (root level)");
  });

  it("populates select with workspace names from GET /workspaces", async () => {
    await openDialog();
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      const optionValues = Array.from(select.options).map((o) => o.value);
      expect(optionValues).toContain("ws-1");
      expect(optionValues).toContain("ws-2");
    });
    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    const optionTexts = Array.from(select.options).map((o) => o.text.trim());
    expect(optionTexts.some((t) => t.includes("Platform Team"))).toBe(true);
    expect(optionTexts.some((t) => t.includes("Research Agent"))).toBe(true);
  });

  it("sends parent_id in POST body when a workspace is selected", async () => {
    await openDialog();
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      expect(select.options.length).toBeGreaterThan(1);
    });

    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "My Agent" },
    });

    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
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

    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.parent_id).toBeUndefined();
  });

  it("sends the cost-efficient headless compute profile by default", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Plain Agent" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toEqual({
      instance_type: "t3.medium",
      volume: { root_gb: 30 },
      display: { mode: "none" },
    });
    expect(body.model).toBe("moonshot/kimi-k2.6");
    expect(body.llm_provider).toBe("platform");
    expect(body.runtime).toBe("claude-code");
    expect(body.secrets).toBeUndefined();
  });

  it("keeps runtime and workspace template as separate selectors", async () => {
    await openDialog();

    const runtimeSelect = screen.getByLabelText("Runtime") as HTMLSelectElement;
    const runtimeTexts = Array.from(runtimeSelect.options).map((o) => o.text.trim());
    expect(runtimeTexts).toEqual([
      "Claude Code",
      "OpenAI Codex CLI",
      "Google ADK",
      "Hermes",
      "OpenClaw",
    ]);
    expect(runtimeTexts).not.toContain("SEO Agent");

    await waitFor(() => {
      const templateSelect = screen.getByLabelText("Workspace Template") as HTMLSelectElement;
      const templateTexts = Array.from(templateSelect.options).map((o) => o.text.trim());
      expect(templateTexts).toContain("SEO Agent");
      expect(templateTexts).not.toContain("Hermes");
    });
  });

  it("does not send managed compute for external agents", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "External Agent" },
    });
    fireEvent.click(screen.getByLabelText(/External agent/));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toBeUndefined();
    expect(body.runtime).toBe("external");
  });

  it("sends display compute profile when desktop display is enabled", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Desktop Agent" },
    });
    fireEvent.click(screen.getByLabelText("Enable display"));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("moonshot/kimi-k2.6");
    expect(body.llm_provider).toBe("platform");
    expect(body.compute).toEqual({
      instance_type: "t3.xlarge",
      volume: { root_gb: 80 },
      display: {
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      },
    });
  });

  it("drives display instance-type options from /compute/metadata SSOT", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Desktop Agent" },
    });
    fireEvent.click(screen.getByLabelText("Enable display"));

    const instanceSelect = screen.getByLabelText("Instance") as HTMLSelectElement;
    await waitFor(() => {
      const optionValues = Array.from(instanceSelect.options).map((o) => o.value);
      expect(optionValues).toEqual(SAMPLE_COMPUTE_METADATA.instanceTypes.aws);
    });
  });

  it("consumes the SSOT display default instead of the in-bundle fallback", async () => {
    // Override the /compute/metadata mock so AWS display_default differs from the
    // bundled FALLBACK_COMPUTE_OPTIONS. This proves the dialog reads the live SSOT
    // value and does not silently fall back to the offline bundle.
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/compute/metadata") {
        return {
          providers: ["aws"],
          instanceTypes: { aws: ["t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge"] },
          defaults: { aws: "t3.medium" },
          display_defaults: { aws: "t3.2xlarge" },
        };
      }
      if (url === "/templates") return SAMPLE_TEMPLATES as any;
      return SAMPLE_WORKSPACES as any;
    });

    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "SSOT Display Agent" },
    });
    fireEvent.click(screen.getByLabelText("Enable display"));

    const instanceSelect = screen.getByLabelText("Instance") as HTMLSelectElement;
    await waitFor(() => expect(instanceSelect.value).toBe("t3.2xlarge"));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toEqual({
      instance_type: "t3.2xlarge",
      volume: { root_gb: 80 },
      display: {
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      },
    });
  });

  it("consumes a non-AWS SSOT display default when the cloud provider changes", async () => {
    // Make the canvas think it is running on a SaaS tenant so the cloud-provider
    // selector is rendered. Hetzner's SSOT display default (cpx51) differs from
    // the in-bundle fallback (cpx41), proving the dropdown reads display_defaults
    // for the selected provider rather than always defaulting to AWS.
    const originalLocation = window.location;
    vi.stubGlobal("location", { ...originalLocation, hostname: "acme.moleculesai.app" });

    mockGet.mockImplementation(async (url: string) => {
      if (url === "/compute/metadata") {
        return {
          providers: ["aws", "hetzner"],
          instanceTypes: {
            aws: ["t3.medium", "t3.xlarge"],
            hetzner: ["cpx31", "cpx41", "cpx51"],
          },
          defaults: { aws: "t3.medium", hetzner: "cpx31" },
          display_defaults: { aws: "t3.xlarge", hetzner: "cpx51" },
        };
      }
      if (url === "/templates") return SAMPLE_TEMPLATES as any;
      return SAMPLE_WORKSPACES as any;
    });

    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hetzner Display Agent" },
    });

    fireEvent.change(screen.getByLabelText("Cloud provider") as HTMLSelectElement, {
      target: { value: "hetzner" },
    });
    fireEvent.click(screen.getByLabelText("Enable display"));

    const instanceSelect = screen.getByLabelText("Instance") as HTMLSelectElement;
    await waitFor(() => expect(instanceSelect.value).toBe("cpx51"));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toEqual({
      instance_type: "cpx51",
      volume: { root_gb: 80 },
      display: {
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      },
      provider: "hetzner",
    });

    vi.stubGlobal("location", originalLocation);
  });

  it("sends BYOK API key secrets when API key auth mode is selected", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "BYOK Agent" },
    });
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "minimax|MINIMAX_API_KEY" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "sk-minimax-test" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("MiniMax-M2.7");
    expect(body.llm_provider).toBe("minimax");
    expect(body.secrets).toEqual({ MINIMAX_API_KEY: "sk-minimax-test" });
  });

  it("sends Claude OAuth token separately from platform-managed mode", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "OAuth Agent" },
    });
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "anthropic-oauth|CLAUDE_CODE_OAUTH_TOKEN" },
    });
    fireEvent.change(document.querySelector("[data-testid='model-select']") as HTMLSelectElement, {
      target: { value: "sonnet" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "oauth-token" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("sonnet");
    expect(body.llm_provider).toBe("anthropic-oauth");
    expect(body.secrets).toEqual({ CLAUDE_CODE_OAUTH_TOKEN: "oauth-token" });
  });

  it("lists all Claude Code subscription aliases for blank workspaces", async () => {
    await openDialog();

    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "anthropic-oauth|CLAUDE_CODE_OAUTH_TOKEN" },
    });

    const modelSelect = document.querySelector("[data-testid='model-select']") as HTMLSelectElement;
    const optionValues = Array.from(modelSelect.options).map((option) => option.value);
    expect(optionValues).toEqual(expect.arrayContaining(["sonnet", "opus", "haiku"]));
  });

  it("renders gracefully when GET /workspaces fails", async () => {
    mockGet.mockRejectedValueOnce(new Error("Network error"));
    await openDialog();

    // Dialog still renders; select exists with only the root option
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      expect(select.options.length).toBe(1);
      expect(select.options[0].value).toBe("");
    });
  });
});

// ---------------------------------------------------------------------------
// Dynamic runtime provider picker tests
// ---------------------------------------------------------------------------

describe("CreateWorkspaceDialog — dynamic runtime provider picker", () => {
  it("does not render the old Hermes-only provider section", async () => {
    await openDialog();
    await setRuntime("hermes");
    expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeNull();
  });

  it("derives Hermes provider and model options from the /templates runtime row", async () => {
    await openDialog();
    await setRuntime("hermes");

    const providerSelect = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
    await waitFor(() => expect(providerSelect.options.length).toBe(4));

    const providerValues = Array.from(providerSelect.options).map((option) => option.value);
    expect(providerValues).toEqual(expect.arrayContaining([
      "platform|",
      "openai|OPENAI_API_KEY",
      "anthropic|ANTHROPIC_API_KEY",
    ]));
    expect(providerValues).not.toContain("gemini|GEMINI_API_KEY");
  });

  it("uses the template-declared default provider/model for Hermes", async () => {
    await openDialog();
    await setRuntime("hermes");

    await waitFor(() => {
      const providerSelect = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
      expect(providerSelect.value).toBe("platform|");
    });
    const modelSelect = document.querySelector("[data-testid='model-select']") as HTMLSelectElement;
    expect(modelSelect.value).toBe("moonshot/kimi-k2.6");
  });

  it("prompts for the provider credential required by the selected Hermes model", async () => {
    await openDialog();
    await setRuntime("hermes");

    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });

    const keyInput = document.getElementById("llm-secret-input") as HTMLInputElement;
    expect(keyInput).toBeTruthy();
    expect(keyInput.type).toBe("password");
  });

  it("shows an error if the selected runtime provider requires a credential", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes Agent" },
    });
    await setRuntime("hermes");
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain("Provider credential");
    });
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("includes runtime-derived provider/model/secrets in POST body", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes OpenAI" },
    });
    await setRuntime("hermes");
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "sk-openai-test" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.runtime).toBe("hermes");
    expect(body.template).toBeUndefined();
    expect(body.model).toBe("openai/gpt-4o");
    expect(body.llm_provider).toBe("openai");
    expect(body.secrets).toEqual({ OPENAI_API_KEY: "sk-openai-test" });
  });

  it("does NOT include secrets field when provider is platform-managed", async () => {
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
});

// ---------------------------------------------------------------------------
// Registry-backed provider catalog (RFC#340 Fix C)
//
// Regression guard for the mis-bucketing bug: when a registry-backed
// claude-code template serves `moonshot/kimi-k2.6` whose DERIVED provider is
// `platform`, the dialog must build the dropdown from registry_providers/
// registry_models (buildProviderCatalogFromRegistry) — NOT the legacy
// inferVendor heuristic which slash-splits the id into "moonshot". The
// distinguishing trait of this fixture: the plain `models[]` array does NOT
// carry an explicit `provider` field, so the LEGACY path would bucket the
// model under "moonshot" and send llm_provider:"moonshot". Only the
// registry-backed path yields the Platform bucket + llm_provider:"platform".
// ---------------------------------------------------------------------------

// claude-code template whose plain models[] is UN-annotated (no explicit
// provider). The derived-provider annotation lives ONLY in registry_models.
const REGISTRY_TEMPLATE = {
  id: "claude-code-default",
  name: "Claude Code Agent",
  runtime: "claude-code",
  model: "moonshot/kimi-k2.6",
  // Legacy fields — note: NO explicit provider on the platform model, so the
  // legacy inferVendor path would slash-split it into "moonshot".
  providers: ["platform", "minimax", "anthropic"],
  models: [
    { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", required_env: [] },
    { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
    { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["ANTHROPIC_API_KEY"] },
  ],
  // Registry-served SSOT (internal#718 P3). DeriveProvider resolved
  // moonshot/kimi-k2.6 → "platform"; MiniMax-M2.7 → "minimax".
  registry_backed: true,
  registry_providers: [
    { name: "platform", display_name: "Platform", auth_env: [], billing_mode: "platform_managed" },
    { name: "minimax", display_name: "MiniMax", auth_env: ["MINIMAX_API_KEY"], billing_mode: "byok" },
    { name: "anthropic", display_name: "Anthropic API", auth_env: ["ANTHROPIC_API_KEY"], billing_mode: "byok" },
  ],
  registry_models: [
    { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", billing_mode: "platform_managed" },
    { id: "MiniMax-M2.7", name: "MiniMax M2.7", provider: "minimax", billing_mode: "byok" },
    { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", provider: "anthropic", billing_mode: "byok" },
  ],
};

// Registry-backed platform provider WITH a non-empty auth_env — this matches
// the PRODUCTION provider view, which ships the raw AuthEnv
// ([MOLECULE_LLM_USAGE_TOKEN]). REGISTRY_TEMPLATE above uses auth_env:[] so it
// never exercises suppression; this one drives the billingMode==="platform_
// managed" branch end-to-end through buildProviderCatalogFromRegistry. (#2245)
const REGISTRY_TEMPLATE_PLATFORM_AUTHENV = {
  ...REGISTRY_TEMPLATE,
  registry_providers: [
    {
      name: "platform",
      display_name: "Platform",
      auth_env: ["MOLECULE_LLM_USAGE_TOKEN"],
      billing_mode: "platform_managed",
    },
    { name: "minimax", display_name: "MiniMax", auth_env: ["MINIMAX_API_KEY"], billing_mode: "byok" },
    { name: "anthropic", display_name: "Anthropic API", auth_env: ["ANTHROPIC_API_KEY"], billing_mode: "byok" },
  ],
};

describe("CreateWorkspaceDialog — registry-backed provider catalog (RFC#340 Fix C)", () => {
  beforeEach(() => {
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/templates") {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        return [REGISTRY_TEMPLATE] as any;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_WORKSPACES as any;
    });
  });

  it("shows the Platform provider bucket for the registry-backed claude-code runtime", async () => {
    await openDialog();
    const providerSelect = await waitFor(() => {
      const sel = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
      expect(sel).toBeTruthy();
      return sel;
    });
    const labels = Array.from(providerSelect.options).map((o) => o.text.trim());
    // Registry display_name "Platform" appears — NOT "moonshot" from the
    // legacy slash-split heuristic.
    expect(labels).toContain("Platform");
    expect(labels).not.toContain("moonshot");
    // Bucket id is the registry-keyed id, vendor is the bare provider name.
    const values = Array.from(providerSelect.options).map((o) => o.value);
    expect(values).toContain("registry|platform");
  });

  it("sends llm_provider: platform (not moonshot) for moonshot/kimi-k2.6", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Kimi Agent" },
    });
    // Wait for the registry default to settle on the Platform bucket + model.
    await waitFor(() => {
      const modelSelect = document.querySelector("[data-testid='model-select']") as HTMLSelectElement;
      expect(modelSelect?.value).toBe("moonshot/kimi-k2.6");
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("moonshot/kimi-k2.6");
    expect(body.llm_provider).toBe("platform");
    // Platform is auth-env-free → no BYOK secret.
    expect(body.secrets).toBeUndefined();
  });

  it("buckets MiniMax-M2.7 under its derived provider and sends llm_provider: minimax", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "MiniMax Agent" },
    });
    await waitFor(() => {
      const sel = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
      expect(Array.from(sel.options).map((o) => o.value)).toContain("registry|minimax");
    });
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "registry|minimax" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "sk-minimax-test" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("MiniMax-M2.7");
    expect(body.llm_provider).toBe("minimax");
    expect(body.secrets).toEqual({ MINIMAX_API_KEY: "sk-minimax-test" });
  });

  it("suppresses the credential for a registry-backed platform provider that declares an auth_env — billingMode path (#2245)", async () => {
    // Override the default REGISTRY_TEMPLATE (auth_env:[]) with the production-
    // shaped one whose platform provider declares MOLECULE_LLM_USAGE_TOKEN.
    mockGet.mockImplementation(async (url: string) => {
      if (url === "/templates") {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        return [REGISTRY_TEMPLATE_PLATFORM_AUTHENV] as any;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_WORKSPACES as any;
    });
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Registry Platform Agent" },
    });
    // Platform is the default bucket; even with a non-empty auth_env the key
    // field must NOT render (suppressed via billingMode==="platform_managed").
    await waitFor(() => {
      const sel = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
      expect(sel?.value).toBe("registry|platform");
    });
    expect(screen.getByText("Platform-managed — no API key required.")).toBeTruthy();
    expect(document.getElementById("llm-secret-input")).toBeNull();

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    expect(screen.queryByText("Provider credential is required")).toBeNull();
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.llm_provider).toBe("platform");
    // The provisioner-injected MOLECULE_LLM_USAGE_TOKEN must NOT be clobbered.
    expect(body.secrets).toBeUndefined();
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

describe("CreateWorkspaceDialog — platform-managed credential suppression (#2245)", () => {
  describe("isPlatformManagedProvider", () => {
    it("is true for the platform proxy vendor", () => {
      expect(isPlatformManagedProvider({ vendor: "platform" })).toBe(true);
    });
    it("is true for a registry billingMode of platform_managed", () => {
      expect(
        isPlatformManagedProvider({ vendor: "minimax", billingMode: "platform_managed" }),
      ).toBe(true);
    });
    it("is false for a BYOK provider", () => {
      expect(isPlatformManagedProvider({ vendor: "anthropic", billingMode: "byok" })).toBe(false);
      expect(isPlatformManagedProvider({ vendor: "minimax" })).toBe(false);
    });
    it("is false for null/undefined", () => {
      expect(isPlatformManagedProvider(null)).toBe(false);
      expect(isPlatformManagedProvider(undefined)).toBe(false);
    });
  });

  it("platform-managed provider with a declared auth env requires NO credential, hides the key field, and sends NO secret", async () => {
    await openDialog();
    await setTemplate("platform-managed-test");
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Platform Agent" },
    });

    // The credential input must NOT render for platform-managed; a "no key
    // required" note appears instead.
    await waitFor(() =>
      expect(screen.getByText("Platform-managed — no API key required.")).toBeTruthy(),
    );
    expect(screen.queryByLabelText("MOLECULE_LLM_USAGE_TOKEN")).toBeNull();

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    // No validation error, and the provisioner-injected token is NOT clobbered
    // by an empty secret.
    expect(screen.queryByText("Provider credential is required")).toBeNull();
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.llm_provider).toBe("platform");
    expect(body.secrets).toBeUndefined();
  });

  it("BYOK provider still requires a credential and renders the key field (no-regression)", async () => {
    await openDialog();
    await setTemplate("byok-only-test");
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "BYOK Agent" },
    });

    // The credential field IS rendered for BYOK...
    await waitFor(() => expect(screen.getByLabelText("OPENAI_API_KEY")).toBeTruthy());

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    // ...and create stays blocked until it's filled.
    await waitFor(() =>
      expect(screen.getByText("Provider credential is required")).toBeTruthy(),
    );
    expect(mockPost).not.toHaveBeenCalled();
  });
});
