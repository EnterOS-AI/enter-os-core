// @vitest-environment jsdom
//
// Behavioral coverage for the install flow. Two regressions to pin
// down:
//
//  1. The install POST URL has to include the workspace id. A pre-fix
//     bug routed it to /workspaces/undefined/plugins because the
//     component read `data.id`, but `WorkspaceNodeData` has no `id`
//     field — its `extends Record<string, unknown>` index signature
//     hid the bad access from TS. The component now takes
//     `workspaceId` as an explicit prop; this test asserts the URL.
//
//  2. The optimistic install update has to flip the registry row to
//     "Installed" without waiting for the 15s reload timer (the
//     PLUGIN_RELOAD_DELAY_MS gap). This test asserts the row's "Install"
//     button is replaced by the green "Installed" tag synchronously
//     after the POST resolves.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";

const mockApiGet = vi.fn();
const mockApiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => mockApiGet(...args),
    post: (...args: unknown[]) => mockApiPost(...args),
    put: vi.fn().mockResolvedValue({}),
    del: vi.fn().mockResolvedValue({}),
    patch: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: Record<string, unknown>) => unknown) =>
      selector({ setPanelTab: vi.fn() } as Record<string, unknown>),
    ),
    { getState: () => ({ setPanelTab: vi.fn() }) },
  ),
  summarizeWorkspaceCapabilities: vi.fn(() => ({ skills: [], tools: [] })),
}));

vi.mock("../Toaster", () => ({ showToast: vi.fn() }));

import { SkillsTab } from "../tabs/SkillsTab";

function makeData() {
  return {
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

const REGISTRY = [
  {
    name: "browser-automation",
    version: "1.1.0",
    description: "Browser automation + testing",
    author: "molecule",
    tags: ["browser", "playwright"],
    skills: [],
    runtimes: ["claude-code"],
  },
];

beforeEach(() => {
  // Order matches the component's loadInstalled / loadRegistry
  // /loadSourceSchemes calls. Schemes endpoint resolves with an
  // empty list so the Install-from-source input doesn't blow up.
  mockApiGet.mockReset();
  mockApiPost.mockReset();
  mockApiGet.mockImplementation((path: string) => {
    if (path.endsWith("/plugins") && path.startsWith("/workspaces/")) {
      return Promise.resolve([]); // installed
    }
    if (path === "/plugins") {
      return Promise.resolve(REGISTRY); // registry
    }
    if (path === "/plugins/sources") {
      return Promise.resolve({ schemes: ["github://", "local://"] });
    }
    return Promise.resolve(null);
  });
  mockApiPost.mockResolvedValue({ status: "installed", plugin: "browser-automation" });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Returns the registry row's Install button. The custom-source input
// also renders an "Install" button, so `findByRole({name: /install/})`
// throws on multiple matches; scope by the row's plugin-name text.
async function findRowInstallButton() {
  const nameNode = await screen.findByText("browser-automation");
  const row = nameNode.closest("div.flex.items-center.justify-between") as HTMLElement;
  if (!row) throw new Error("could not locate row container for browser-automation");
  const buttons = row.querySelectorAll("button");
  const install = Array.from(buttons).find((b) => b.textContent?.trim() === "Install");
  if (!install) throw new Error("row has no Install button (already installed?)");
  return install;
}

describe("SkillsTab install flow", () => {
  it("POSTs to /workspaces/<workspaceId>/plugins (no `undefined` in URL)", async () => {
    render(<SkillsTab workspaceId="ws-abc-123" data={makeData() as never} />);

    fireEvent.click(await findRowInstallButton());

    await waitFor(() => expect(mockApiPost).toHaveBeenCalled());
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces/ws-abc-123/plugins",
      { source: "local://browser-automation" },
    );
  });

  it("flips the registry row to 'Installed' synchronously after POST resolves (no 15s wait)", async () => {
    render(<SkillsTab workspaceId="ws-abc-123" data={makeData() as never} />);

    fireEvent.click(await findRowInstallButton());

    // The "Installed" green tag must appear without advancing the
    // reload timer — the optimistic update is the entire point of
    // this fix. If this test ever regresses to needing fake timers
    // + advanceTimersByTime, the optimistic path is broken.
    const installedTag = await screen.findByText(/^Installed$/i);
    expect(installedTag).toBeDefined();
  });
});
