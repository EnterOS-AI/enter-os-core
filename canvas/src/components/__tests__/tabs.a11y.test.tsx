// @vitest-environment jsdom
/**
 * WCAG 1.3.1 — label↔input association tests for SkillsTab, FilesTab,
 * ChannelsTab, and ScheduleTab.
 *
 * Each test verifies that every form control has an accessible name either via:
 *   - `aria-label` (bare inputs without a visible label element)
 *   - `htmlFor` + matching `id` wired through `useId()` (label↔control pairs)
 *
 * `getByLabelText` is the definitive assertion for the htmlFor/id pattern —
 * if it resolves, the association is valid per the AT accessibility tree.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";

// ── Global mocks (hoisted before imports) ────────────────────────────────────

const mockApiGet = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => mockApiGet(...args),
    post: vi.fn().mockResolvedValue({}),
    put: vi.fn().mockResolvedValue({}),
    del: vi.fn().mockResolvedValue({}),
    patch: vi.fn().mockResolvedValue({}),
  },
}));

const mockCanvasTabState = {
  setPanelTab: vi.fn(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: Record<string, unknown>) => unknown) =>
      selector(mockCanvasTabState as Record<string, unknown>)
    ),
    { getState: () => mockCanvasTabState }
  ),
  summarizeWorkspaceCapabilities: vi.fn(() => ({ skills: [], tools: [] })),
}));

vi.mock("../Toaster", () => ({ showToast: vi.fn() }));

// FilesTab sub-module stubs — stub them so we control the onNewFile callback
vi.mock("../tabs/FilesTab/FilesToolbar", () => ({
  FilesToolbar: ({ onNewFile }: { onNewFile: () => void }) => (
    <button onClick={onNewFile} data-testid="new-file-btn">New File</button>
  ),
}));
vi.mock("../tabs/FilesTab/FileTree", () => ({
  FileTree: () => <div data-testid="file-tree" />,
}));
vi.mock("../tabs/FilesTab/FileEditor", () => ({
  FileEditor: () => <div data-testid="file-editor" />,
}));
vi.mock("../tabs/FilesTab/useFilesApi", () => ({
  useFilesApi: () => ({
    files: [],
    loading: false,
    loadFiles: vi.fn(),
    expandedDirs: new Set<string>(),
    loadingDir: null,
    toggleDir: vi.fn(),
    readFile: vi.fn().mockResolvedValue({ content: "" }),
    writeFile: vi.fn().mockResolvedValue({}),
    deleteFile: vi.fn().mockResolvedValue({}),
    downloadAllFiles: vi.fn(),
    uploadFiles: vi.fn(),
    deleteAllFiles: vi.fn(),
  }),
}));
vi.mock("../tabs/FilesTab/tree", () => ({
  buildTree: vi.fn(() => []),
}));

vi.mock("../ConfirmDialog", () => ({
  ConfirmDialog: () => null,
}));

// ── Static imports (after mocks) ─────────────────────────────────────────────

import { SkillsTab } from "../tabs/SkillsTab";
import { FilesTab } from "../tabs/FilesTab";
import { ChannelsTab } from "../tabs/ChannelsTab";
import { ScheduleTab } from "../tabs/ScheduleTab";

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeSkillsData() {
  return {
    id: "ws-1",
    name: "Test WS",
    status: "online",
    tier: 1,
    agentCard: null,
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "http://localhost:9000",
    parentId: null,
    currentTask: "",
    runtime: "langgraph",
    needsRestart: false,
    budgetLimit: null,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ────────────────────────────────────────────────────────────────────────────
// 1. SkillsTab — aria-label on the "Install from source" bare input
// ────────────────────────────────────────────────────────────────────────────

describe("SkillsTab — aria-label on bare source input (WCAG 1.3.1)", () => {
  beforeEach(() => {
    mockApiGet.mockResolvedValue([]);
  });

  it('install source input has aria-label="Install from source URL"', async () => {
    render(<SkillsTab workspaceId="ws-test-id" data={makeSkillsData() as never} />);

    // The source input is inside the registry section (showRegistry=false initially).
    // Click the "+ Install Plugin" button to reveal it.
    const installBtn = screen.getByRole("button", { name: /install plugin/i });
    fireEvent.click(installBtn);

    const input = screen.getByRole("textbox", {
      name: /install from source url/i,
    });
    expect(input).toBeDefined();
    expect(input.getAttribute("aria-label")).toBe("Install from source URL");
  });

  it("install source input is a text input (not hidden)", async () => {
    render(<SkillsTab workspaceId="ws-test-id" data={makeSkillsData() as never} />);

    const installBtn = screen.getByRole("button", { name: /install plugin/i });
    fireEvent.click(installBtn);

    const input = screen.getByRole("textbox", {
      name: /install from source url/i,
    });
    expect(input.tagName.toLowerCase()).toBe("input");
    expect((input as HTMLInputElement).type).toBe("text");
  });
});

// ────────────────────────────────────────────────────────────────────────────
// 2. FilesTab — aria-label on the new file path bare input
// ────────────────────────────────────────────────────────────────────────────

describe("FilesTab — aria-label on new file path input (WCAG 1.3.1)", () => {
  it('new file input has aria-label="New file path"', () => {
    render(<FilesTab workspaceId="ws-1" />);

    // Trigger showNewFile via the FilesToolbar stub
    const btn = screen.getByTestId("new-file-btn");
    fireEvent.click(btn);

    const input = screen.getByRole("textbox", { name: /new file path/i });
    expect(input).toBeDefined();
    expect(input.getAttribute("aria-label")).toBe("New file path");
  });

  it("new file input is not shown before clicking the new file button", () => {
    render(<FilesTab workspaceId="ws-1" />);

    expect(screen.queryByRole("textbox", { name: /new file path/i })).toBeNull();
  });
});

// ────────────────────────────────────────────────────────────────────────────
// 3. ChannelsTab — htmlFor/id label associations via useId()
// ────────────────────────────────────────────────────────────────────────────

describe("ChannelsTab — htmlFor/id label associations (WCAG 1.3.1)", () => {
  beforeEach(() => {
    mockApiGet.mockImplementation((url: string) => {
      if (url.includes("/channels/adapters")) {
        // Mirror the real GET /channels/adapters shape — schema-driven form
        // relies on config_schema arriving from the adapter. A bare
        // {type, display_name} mock renders an empty form and every
        // getByLabelText below fails.
        return Promise.resolve([
          {
            type: "telegram",
            display_name: "Telegram",
            config_schema: [
              {
                key: "bot_token",
                label: "Bot Token",
                type: "password",
                required: true,
                sensitive: true,
              },
              {
                key: "chat_id",
                label: "Chat IDs",
                type: "text",
                required: true,
              },
            ],
          },
        ]);
      }
      return Promise.resolve([]);
    });
  });

  async function renderAndOpenForm() {
    render(<ChannelsTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByRole("button", { name: /\+ connect/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ connect/i }));
  }

  it("Platform label is associated with the select via htmlFor/id", async () => {
    await renderAndOpenForm();
    const platformSelect = screen.getByLabelText("Platform");
    expect(platformSelect.tagName.toLowerCase()).toBe("select");
  });

  it("Bot Token label is associated with the password input via htmlFor/id", async () => {
    await renderAndOpenForm();
    const botTokenInput = screen.getByLabelText("Bot Token");
    expect(botTokenInput.tagName.toLowerCase()).toBe("input");
    expect((botTokenInput as HTMLInputElement).type).toBe("password");
  });

  it("Chat IDs label is associated with the input via htmlFor/id", async () => {
    await renderAndOpenForm();
    const chatIdInput = screen.getByLabelText("Chat IDs");
    expect(chatIdInput.tagName.toLowerCase()).toBe("input");
  });

  it("Allowed Users label is associated with the input via htmlFor/id", async () => {
    await renderAndOpenForm();
    // Label contains "(optional, comma-separated)" in a nested span — use regex
    const allowedUsersInput = screen.getByLabelText(/allowed users/i);
    expect(allowedUsersInput.tagName.toLowerCase()).toBe("input");
  });

  it("all form control ids are unique and non-empty", async () => {
    await renderAndOpenForm();

    const platformSelect = screen.getByLabelText("Platform");
    const botTokenInput = screen.getByLabelText("Bot Token");
    const chatIdInput = screen.getByLabelText("Chat IDs");
    const allowedUsersInput = screen.getByLabelText(/allowed users/i);

    const ids = [
      platformSelect.id,
      botTokenInput.id,
      chatIdInput.id,
      allowedUsersInput.id,
    ];
    const uniqueIds = new Set(ids);
    expect(uniqueIds.size).toBe(4);
    ids.forEach((id) => expect(id).toBeTruthy());
  });
});

// ────────────────────────────────────────────────────────────────────────────
// 4. ScheduleTab — aria-label on name + htmlFor/id associations via useId()
// ────────────────────────────────────────────────────────────────────────────

describe("ScheduleTab — aria-label + htmlFor/id label associations (WCAG 1.3.1)", () => {
  beforeEach(() => {
    mockApiGet.mockResolvedValue([]);
  });

  async function renderAndOpenForm() {
    render(<ScheduleTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByRole("button", { name: /\+ add schedule/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
  }

  it('Schedule name input has aria-label="Schedule name"', async () => {
    await renderAndOpenForm();
    const nameInput = screen.getByRole("textbox", { name: /^schedule name$/i });
    expect(nameInput.getAttribute("aria-label")).toBe("Schedule name");
  });

  it("Cron Expression label is associated with the input via htmlFor/id", async () => {
    await renderAndOpenForm();
    const cronInput = screen.getByLabelText("Cron Expression");
    expect(cronInput.tagName.toLowerCase()).toBe("input");
    expect((cronInput as HTMLInputElement).type).toBe("text");
  });

  it("Timezone label is associated with the select via htmlFor/id", async () => {
    await renderAndOpenForm();
    const timezoneSelect = screen.getByLabelText("Timezone");
    expect(timezoneSelect.tagName.toLowerCase()).toBe("select");
  });

  it("Prompt / Task label is associated with the textarea via htmlFor/id", async () => {
    await renderAndOpenForm();
    const promptTextarea = screen.getByLabelText(/prompt \/ task/i);
    expect(promptTextarea.tagName.toLowerCase()).toBe("textarea");
  });

  it("all form control ids are unique and non-empty", async () => {
    await renderAndOpenForm();

    const cronInput = screen.getByLabelText("Cron Expression");
    const timezoneSelect = screen.getByLabelText("Timezone");
    const promptTextarea = screen.getByLabelText(/prompt \/ task/i);

    const ids = [cronInput.id, timezoneSelect.id, promptTextarea.id];
    const uniqueIds = new Set(ids);
    expect(uniqueIds.size).toBe(3);
    ids.forEach((id) => expect(id).toBeTruthy());
  });
});
