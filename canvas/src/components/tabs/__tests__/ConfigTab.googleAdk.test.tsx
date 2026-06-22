// @vitest-environment jsdom
//
// Regression: project_canvas_runtime_dropdown_ssot_fix — a google-adk
// workspace's Config tab showed the wrong runtime ("LangGraph (default)"
// / first option) because a hardcoded frontend allowlist
// (SUPPORTED_RUNTIME_VALUES) dropped google-adk from the /templates-derived
// options even though the backend served it. A Save from that state would
// PATCH runtime to the wrong value and break the ADK agent.
//
// The fix: the dropdown is SSOT-driven — it trusts GET /templates (which the
// backend already gates to the manifest maintained set) and hides a runtime
// only when its row carries `displayable: false`. This pins: a google-adk
// workspace shows "google-adk" selected, and a displayable:false template is
// not offered.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPatch = vi.fn();
const apiPut = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: (path: string, body: unknown) => apiPatch(path, body),
    put: (path: string, body: unknown) => apiPut(path, body),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }),
    { getState: () => ({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }) },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

function wireApi(templates: Array<{ id: string; name?: string; runtime?: string; models?: unknown[]; displayable?: boolean }>) {
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-adk") return Promise.resolve({ runtime: "google-adk" });
    if (path === "/workspaces/ws-adk/model") return Promise.resolve({ model: "vertex:gemini-2.5-pro" });
    if (path === "/workspaces/ws-adk/files/config.yaml") return Promise.resolve({ content: "name: adk\nruntime: google-adk\n" });
    if (path === "/templates") return Promise.resolve(templates);
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
});

describe("ConfigTab — google-adk runtime (SSOT dropdown)", () => {
  it("shows google-adk selected in the runtime dropdown (#ssot-fix)", async () => {
    wireApi([
      { id: "claude-code", name: "Claude Code", runtime: "claude-code", models: [] },
      { id: "google-adk", name: "Google ADK", runtime: "google-adk", models: [] },
    ]);
    render(<ConfigTab workspaceId="ws-adk" />);
    const select = await waitFor(() => screen.getByRole("combobox", { name: /runtime/i }));
    expect((select as HTMLSelectElement).value).toBe("google-adk");
    const opts = Array.from((select as HTMLSelectElement).options).map((o) => o.value);
    expect(opts).toContain("google-adk");
  });

  it("hides a template flagged displayable:false", async () => {
    wireApi([
      { id: "google-adk", name: "Google ADK", runtime: "google-adk", models: [] },
      { id: "legacy", name: "Legacy", runtime: "legacy", models: [], displayable: false },
    ]);
    render(<ConfigTab workspaceId="ws-adk" />);
    const select = await waitFor(() => screen.getByRole("combobox", { name: /runtime/i }));
    const opts = Array.from((select as HTMLSelectElement).options).map((o) => o.value);
    expect(opts).toContain("google-adk");
    expect(opts).not.toContain("legacy");
  });
});
