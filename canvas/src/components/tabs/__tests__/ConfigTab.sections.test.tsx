// @vitest-environment jsdom
//
// Regression tests for the ConfigTab section restructure (user feedback
// 2026-05-04: "Skills and Tools are having their own tab as plugin, and
// Prompt Files are in the file system which can be directly edited. Am
// I missing something?" + "Tools should be merged into plugin then, and
// for prompt files... should be in another section than in skill& tools").
//
// What this pins:
//   1. The "Skills & Tools" section title is gone.
//   2. Editable Skills + Tools tag inputs are gone (managed elsewhere).
//   3. A dedicated "Prompt Files" section exists with explanatory text.
//
// If a future PR re-adds the Skills/Tools tag inputs to ConfigTab, this
// suite catches it.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: vi.fn(),
    put: vi.fn(),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

const storeUpdateNodeData = vi.fn();
const storeRestartWorkspace = vi.fn();
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) =>
      selector({ restartWorkspace: storeRestartWorkspace, updateNodeData: storeUpdateNodeData }),
    {
      getState: () => ({
        restartWorkspace: storeRestartWorkspace,
        updateNodeData: storeUpdateNodeData,
      }),
    },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

beforeEach(() => {
  apiGet.mockReset();
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-test`) {
      return Promise.resolve({ runtime: "claude-code" });
    }
    if (path === `/workspaces/ws-test/model`) {
      return Promise.resolve({ model: "claude-opus-4-7" });
    }
    if (path === `/workspaces/ws-test/provider`) {
      return Promise.resolve({ provider: "anthropic-oauth", source: "default" });
    }
    if (path === `/workspaces/ws-test/files/config.yaml`) {
      return Promise.resolve({ content: "name: test\nruntime: claude-code\n" });
    }
    if (path === "/templates") {
      return Promise.resolve([
        { id: "claude-code", name: "Claude Code", runtime: "claude-code", providers: [] },
      ]);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
});

describe("ConfigTab section restructure", () => {
  it("does not render a 'Skills & Tools' section title", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // Section button uses the title as its accessible name; should be absent.
    expect(screen.queryByRole("button", { name: /Skills\s*&\s*Tools/i })).toBeNull();
  });

  it("does not render an editable Skills tag input", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // TagList renders its label; check no input labelled "Skills" in the form.
    // (Skills are managed via the dedicated Skills tab.)
    const skillsLabels = screen
      .queryAllByText(/^Skills$/)
      .filter((el) => el.tagName.toLowerCase() === "label");
    expect(skillsLabels).toHaveLength(0);
  });

  it("does not render an editable Tools tag input", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // Tools are managed via the Plugins tab — install a plugin → its tools
    // become available. No reason to type tool names here.
    const toolsLabels = screen
      .queryAllByText(/^Tools$/)
      .filter((el) => el.tagName.toLowerCase() === "label");
    expect(toolsLabels).toHaveLength(0);
  });

  it("renders a dedicated 'Prompt Files' section with explanatory copy", async () => {
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // Section is collapsed by default — find + expand first.
    const sectionButton = screen.getByRole("button", { name: /Prompt Files/i });
    expect(sectionButton).toBeTruthy();
    fireEvent.click(sectionButton);
    // Explanatory copy mentions system-prompt.md (split across <code> tags
    // so use textContent on any element rather than the default text matcher).
    await waitFor(() => {
      const matches = screen.queryAllByText((_, el) =>
        (el?.textContent || "").includes("system-prompt.md"),
      );
      expect(matches.length).toBeGreaterThan(0);
    });
  });
});
