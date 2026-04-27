import { describe, it, expect, beforeEach, vi } from "vitest";

// Mock fetch globally
global.fetch = vi.fn(() =>
  Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response),
);

import { useCanvasStore } from "../../store/canvas";
import type { WorkspaceData } from "../../store/socket";
import { DEFAULT_PROVISION_TIMEOUT_MS } from "../ProvisioningTimeout";
import {
  DEFAULT_RUNTIME_PROFILE,
  RUNTIME_PROFILES,
  getRuntimeProfile,
  provisionTimeoutForRuntime,
} from "@/lib/runtimeProfiles";

// Helper to build a WorkspaceData object
function makeWS(overrides: Partial<WorkspaceData> & { id: string }): WorkspaceData {
  return {
    name: "WS",
    role: "agent",
    tier: 1,
    status: "online",
    agent_card: null,
    url: "http://localhost:9000",
    parent_id: null,
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: "",
    uptime_seconds: 60,
    current_task: "",
    x: 0,
    y: 0,
    collapsed: false,
    runtime: "",
    budget_limit: null,
    ...overrides,
  };
}

beforeEach(() => {
  useCanvasStore.setState({
    nodes: [],
    edges: [],
    selectedNodeId: null,
    panelTab: "details",
    dragOverNodeId: null,
    contextMenu: null,
    searchOpen: false,
    viewport: { x: 0, y: 0, zoom: 1 },
  });
  vi.clearAllMocks();
});

describe("ProvisioningTimeout", () => {
  it("exports the default timeout constant", () => {
    expect(DEFAULT_PROVISION_TIMEOUT_MS).toBe(120_000);
  });

  it("can detect provisioning nodes in the store", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
      makeWS({ id: "ws-2", name: "Agent 2", status: "online" }),
      makeWS({ id: "ws-3", name: "Agent 3", status: "provisioning" }),
    ]);

    const nodes = useCanvasStore.getState().nodes;
    const provisioning = nodes.filter((n) => n.data.status === "provisioning");
    expect(provisioning).toHaveLength(2);
    expect(provisioning.map((n) => n.id)).toEqual(["ws-1", "ws-3"]);
  });

  it("transitions node from provisioning to online on event", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
    ]);

    useCanvasStore.getState().applyEvent({
      event: "WORKSPACE_ONLINE",
      workspace_id: "ws-1",
      timestamp: new Date().toISOString(),
      payload: {},
    });

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1");
    expect(node?.data.status).toBe("online");
  });

  it("transitions node from provisioning to failed on provision_failed event", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
    ]);

    useCanvasStore.getState().applyEvent({
      event: "WORKSPACE_PROVISION_FAILED",
      workspace_id: "ws-1",
      timestamp: new Date().toISOString(),
      payload: { error: "Docker daemon not running" },
    });

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1");
    expect(node?.data.status).toBe("failed");
    expect(node?.data.lastSampleError).toBe("Docker daemon not running");
  });

  it("handles WORKSPACE_PROVISION_FAILED with no error message", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
    ]);

    useCanvasStore.getState().applyEvent({
      event: "WORKSPACE_PROVISION_FAILED",
      workspace_id: "ws-1",
      timestamp: new Date().toISOString(),
      payload: {},
    });

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1");
    expect(node?.data.status).toBe("failed");
    expect(node?.data.lastSampleError).toBe("Unknown provisioning error");
  });

  it("restart API call can be made for provisioning recovery", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({}),
    } as Response);

    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "failed" }),
    ]);

    await useCanvasStore.getState().restartWorkspace("ws-1");

    expect(global.fetch).toHaveBeenCalledWith(
      expect.stringContaining("/workspaces/ws-1/restart"),
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("node removal works for cancelling a failed deployment", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
      makeWS({ id: "ws-2", name: "Agent 2", status: "online" }),
    ]);

    expect(useCanvasStore.getState().nodes).toHaveLength(2);

    useCanvasStore.getState().removeNode("ws-1");

    expect(useCanvasStore.getState().nodes).toHaveLength(1);
    expect(useCanvasStore.getState().nodes[0].id).toBe("ws-2");
  });

  it("selectNode and setPanelTab work for view logs action", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
    ]);

    useCanvasStore.getState().selectNode("ws-1");
    expect(useCanvasStore.getState().selectedNodeId).toBe("ws-1");

    useCanvasStore.getState().setPanelTab("terminal");
    expect(useCanvasStore.getState().panelTab).toBe("terminal");
  });

  it("multiple provisioning nodes can coexist", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent 1", status: "provisioning" }),
      makeWS({ id: "ws-2", name: "Agent 2", status: "provisioning" }),
      makeWS({ id: "ws-3", name: "Agent 3", status: "provisioning" }),
    ]);

    const provisioning = useCanvasStore
      .getState()
      .nodes.filter((n) => n.data.status === "provisioning");
    expect(provisioning).toHaveLength(3);

    // First one goes online
    useCanvasStore.getState().applyEvent({
      event: "WORKSPACE_ONLINE",
      workspace_id: "ws-1",
      timestamp: new Date().toISOString(),
      payload: {},
    });

    const stillProvisioning = useCanvasStore
      .getState()
      .nodes.filter((n) => n.data.status === "provisioning");
    expect(stillProvisioning).toHaveLength(2);
  });

  // ── Runtime-aware timeout regression tests (2026-04-24 outage) ────────────
  // Prior to this, a hermes workspace consistently false-alarmed at 2 min
  // into its 8-13 min cold boot, pushing users to retry something that
  // would have come online on its own. The runtime-aware override keeps
  // the 2-min floor for fast docker runtimes while giving hermes its
  // honest 12-min budget.

  describe("runtime profile resolution (@/lib/runtimeProfiles)", () => {
    describe("provisionTimeoutForRuntime", () => {
      it("returns the default for unknown/missing runtimes", () => {
        expect(provisionTimeoutForRuntime(undefined)).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
        expect(provisionTimeoutForRuntime("")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
        expect(provisionTimeoutForRuntime("some-future-runtime")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
      });

      it("returns default for known-fast runtimes (not in profile map)", () => {
        // If someone ever adds one of these to RUNTIME_PROFILES with a
        // slower value, this test catches the unintended regression.
        expect(provisionTimeoutForRuntime("claude-code")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
        expect(provisionTimeoutForRuntime("langgraph")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
        expect(provisionTimeoutForRuntime("crewai")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
      });

      it("hermes returns default — value moved server-side post-#2054 phase 3", () => {
        // RUNTIME_PROFILES.hermes was removed when template-hermes
        // started declaring provision_timeout_seconds in its
        // config.yaml. The value now flows server-side via the
        // workspace API → WorkspaceData.provision_timeout_ms →
        // resolver overrides path. With no override supplied, the
        // resolver falls through to the default — same as any other
        // runtime without a canvas-side override.
        expect(provisionTimeoutForRuntime("hermes")).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
        expect(RUNTIME_PROFILES.hermes).toBeUndefined();
      });

      it("server-side workspace override wins over runtime profile", () => {
        // The resolution order is: overrides → profile → default.
        // An operator-tunable per-workspace number on the backend
        // (e.g. via a template manifest field) should beat the canvas
        // runtime map.
        expect(
          provisionTimeoutForRuntime("hermes", {
            provisionTimeoutMs: 60_000,
          }),
        ).toBe(60_000);
        expect(
          provisionTimeoutForRuntime("some-unknown", {
            provisionTimeoutMs: 300_000,
          }),
        ).toBe(300_000);
      });
    });

    describe("getRuntimeProfile", () => {
      it("returns a structural profile with required fields", () => {
        const profile = getRuntimeProfile("hermes");
        expect(profile.provisionTimeoutMs).toBeTypeOf("number");
        expect(profile.provisionTimeoutMs).toBeGreaterThan(0);
      });

      it("default profile is a valid superset of every override", () => {
        // Every entry in RUNTIME_PROFILES must provide fields the
        // default does — otherwise consumers could get undefined where
        // they expected a number. This test enforces that contract so
        // future entries can't accidentally drop fields.
        for (const [runtime, profile] of Object.entries(RUNTIME_PROFILES)) {
          const resolved = getRuntimeProfile(runtime);
          expect(
            resolved.provisionTimeoutMs,
            `runtime=${runtime} must resolve to a number`,
          ).toBeTypeOf("number");
          expect(resolved.provisionTimeoutMs).toBeGreaterThan(0);
          // Profile's explicit value should be used iff present.
          if (profile.provisionTimeoutMs !== undefined) {
            expect(resolved.provisionTimeoutMs).toBe(profile.provisionTimeoutMs);
          }
        }
      });
    });

    describe("DEFAULT_PROVISION_TIMEOUT_MS backward-compat export", () => {
      it("still exports the same default for legacy importers", () => {
        expect(DEFAULT_PROVISION_TIMEOUT_MS).toBe(
          DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
        );
      });
    });

    // #2054 — per-workspace server override threading from socket
    // payload through node-data into ProvisioningTimeout's resolver.
    // Doesn't render the component; verifies the data path lands the
    // value where ProvisioningTimeout reads it from.
    describe("server-side per-workspace override (#2054)", () => {
      it("hydrate carries provision_timeout_ms onto node.data.provisionTimeoutMs", () => {
        useCanvasStore.getState().hydrate([
          makeWS({
            id: "ws-slow",
            name: "Slow",
            status: "provisioning",
            runtime: "future-runtime",
            provision_timeout_ms: 600_000,
          }),
        ]);
        const node = useCanvasStore
          .getState()
          .nodes.find((n) => n.id === "ws-slow");
        expect(node?.data.provisionTimeoutMs).toBe(600_000);
      });

      it("absent provision_timeout_ms hydrates to null (falls through to default post-cleanup)", () => {
        useCanvasStore.getState().hydrate([
          makeWS({ id: "ws-default", name: "Default", status: "provisioning", runtime: "hermes" }),
        ]);
        const node = useCanvasStore
          .getState()
          .nodes.find((n) => n.id === "ws-default");
        expect(node?.data.provisionTimeoutMs).toBeNull();
        // Post-#2054 phase 3: hermes no longer has a canvas-side
        // RUNTIME_PROFILES entry. With no node override the resolver
        // falls all the way through to DEFAULT_RUNTIME_PROFILE. In
        // production the workspace-server-side template lookup
        // populates node.provisionTimeoutMs to 720000 before this
        // resolver runs (#2094); this test isolates the fall-through
        // behavior when that population hasn't happened yet.
        expect(
          provisionTimeoutForRuntime("hermes", {
            provisionTimeoutMs: node?.data.provisionTimeoutMs ?? undefined,
          }),
        ).toBe(DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs);
      });

      it("server override wins over default via the resolver path the component uses", () => {
        // Mirrors ProvisioningTimeout.tsx where node.provisionTimeoutMs
        // is passed as overrides — verifies the resolver respects the
        // override regardless of the runtime's profile state.
        const override = 600_000;
        expect(
          provisionTimeoutForRuntime("hermes", {
            provisionTimeoutMs: override,
          }),
        ).toBe(override);
        // Sanity — the override is the path that wins (default is much smaller).
        expect(DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs).toBeLessThan(
          override,
        );
      });
    });
  });
});
