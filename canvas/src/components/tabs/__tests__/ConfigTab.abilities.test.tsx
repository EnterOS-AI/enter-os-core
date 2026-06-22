// @vitest-environment jsdom
//
// Tests for the always-visible "Agent Abilities" section added to ConfigTab
// (internal#510 broadcast_enabled, internal#511 talk_to_user_enabled; backend
// wired in commit 29b4bffb).
//
// Problem this pins: the two workspace ability flags had complete wired
// backends but NO canvas control — broadcast had none at all, talk-to-user
// only surfaced as a ChatTab recovery banner that is invisible under its
// TRUE default. The CTO could not see or toggle either from canvas.
//
// What this suite pins:
//   1. An "Agent Abilities" section renders (always visible, not gated).
//   2. Both toggles render and reflect the store node's ability fields,
//      including the asymmetric defaults (broadcast FALSE, talk TRUE).
//   3. Toggling a switch calls PATCH /workspaces/:id/abilities with the
//      correct snake_case body and optimistically updates the store.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPatch = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: (path: string, body?: unknown) => apiPatch(path, body),
    put: vi.fn(),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

// Store node carries the ability flags hydrated by the platform stream
// (canvas-topology.ts maps broadcast_enabled/talk_to_user_enabled onto
// node.data). Mirror that shape so the section reads real values.
const storeUpdateNodeData = vi.fn();
const storeRestartWorkspace = vi.fn();
let nodeData: { broadcastEnabled?: boolean; talkToUserEnabled?: boolean } = {};
const makeState = () => ({
  nodes: [{ id: "ws-test", data: nodeData }],
  restartWorkspace: storeRestartWorkspace,
  updateNodeData: storeUpdateNodeData,
});
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector(makeState()),
    { getState: () => makeState() },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPatch.mockResolvedValue({ status: "updated" });
  storeUpdateNodeData.mockReset();
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

describe("ConfigTab Agent Abilities section", () => {
  it("renders an always-visible 'Agent Abilities' section with both toggles", async () => {
    nodeData = {}; // unset → defaults
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(
      await screen.findByRole("button", { name: /Agent Abilities/i }),
    ).toBeTruthy();
    expect(screen.getByText("Talk to user")).toBeTruthy();
    expect(screen.getByText("Broadcast to peers")).toBeTruthy();
  });

  it("reflects the asymmetric defaults: talk-to-user ON, broadcast OFF", async () => {
    nodeData = {}; // unset → backend defaults
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    const talk = (await screen.findByText("Talk to user"))
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    const broadcast = screen
      .getByText("Broadcast to peers")
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    expect(talk.checked).toBe(true);
    expect(broadcast.checked).toBe(false);
  });

  it("reflects explicit store values", async () => {
    nodeData = { broadcastEnabled: true, talkToUserEnabled: false };
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    const talk = (await screen.findByText("Talk to user"))
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    const broadcast = screen
      .getByText("Broadcast to peers")
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    expect(talk.checked).toBe(false);
    expect(broadcast.checked).toBe(true);
  });

  it("PATCHes /abilities with talk_to_user_enabled and optimistically updates the store", async () => {
    nodeData = {}; // talk defaults true
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    const talk = (await screen.findByText("Talk to user"))
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    fireEvent.click(talk); // true → false
    await waitFor(() =>
      expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-test/abilities", {
        talk_to_user_enabled: false,
      }),
    );
    expect(storeUpdateNodeData).toHaveBeenCalledWith("ws-test", {
      talkToUserEnabled: false,
    });
  });

  it("PATCHes /abilities with broadcast_enabled when the broadcast toggle is flipped", async () => {
    nodeData = {}; // broadcast defaults false
    render(<ConfigTab workspaceId="ws-test" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    const broadcast = (await screen.findByText("Broadcast to peers"))
      .closest("label")!
      .querySelector("input") as HTMLInputElement;
    fireEvent.click(broadcast); // false → true
    await waitFor(() =>
      expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-test/abilities", {
        broadcast_enabled: true,
      }),
    );
    expect(storeUpdateNodeData).toHaveBeenCalledWith("ws-test", {
      broadcastEnabled: true,
    });
  });
});
