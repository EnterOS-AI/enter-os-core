// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiPatch = vi.fn();
const apiGet = vi.fn();
const updateNodeData = vi.fn();
const restartWorkspace = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    patch: (path: string, body: unknown) => apiPatch(path, body),
    get: (path: string) => apiGet(path),
  },
}));

vi.mock("@/lib/runtime-names", () => ({
  runtimeDisplayName: (runtime: string) => runtime,
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace, updateNodeData }),
    { getState: () => ({ restartWorkspace, updateNodeData }) },
  ),
}));

// SaaS so the editable cloud-provider selector renders (non-SaaS shows a read-only
// badge). Existing tests keep provider=aws (default), which is omitted from the
// PATCH payload, so their assertions are unaffected.
vi.mock("@/lib/tenant", () => ({
  isSaaSTenant: () => true,
}));

import { ContainerConfigTab } from "../ContainerConfigTab";

afterEach(() => {
  cleanup();
});

beforeEach(() => {
  apiPatch.mockReset();
  apiGet.mockReset();
  // Default: /compute/metadata fetch rejects → component keeps its in-bundle
  // fallback SSOT. Existing assertions (t3.medium / cpx31 / provider list) are
  // satisfied by the fallback, which mirrors the server. Individual tests that
  // exercise the fetch path override this with mockResolvedValueOnce.
  apiGet.mockRejectedValue(new Error("no /compute/metadata in this test"));
  restartWorkspace.mockReset();
  updateNodeData.mockReset();
});

describe("ContainerConfigTab", () => {
  it("defaults missing compute to the cost-efficient headless profile", () => {
    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: undefined,
        }}
      />,
    );

    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.medium");
    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "30");
  });

  it("renders persisted compute and status settings", () => {
    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 2,
          maxConcurrentTasks: 3,
          workspaceAccess: "read_write",
          deliveryMode: "poll",
          compute: {
            instance_type: "t3.xlarge",
            volume: { root_gb: 80 },
            display: { mode: "desktop-control", protocol: "novnc", width: 1920, height: 1080 },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Runtime image")).toHaveProperty("value", "claude-code");
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.xlarge");
    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "80");
    expect(screen.getByLabelText("Enable display")).toHaveProperty("checked", true);
    expect(screen.getByLabelText("Resolution")).toHaveProperty("value", "1920x1080");
    expect(screen.getByText("Workspace access")).toBeTruthy();
    expect(screen.getByText("read-write")).toBeTruthy();
  });

  it("does not reset dirty form edits on unrelated status rerender", () => {
    const { rerender } = render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "120" } });

    rerender(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 1,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "120");
  });

  it("saves runtime and compute changes through workspace PATCH", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Runtime image"), { target: { value: "hermes" } });
    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "m6i.xlarge" } });
    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "100" } });
    fireEvent.click(screen.getByLabelText("Enable display"));
    fireEvent.change(screen.getByLabelText("Resolution"), { target: { value: "2560x1440" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-compute", {
      runtime: "hermes",
      compute: {
        instance_type: "m6i.xlarge",
        volume: { root_gb: 100 },
        display: { mode: "desktop-control", protocol: "novnc", width: 2560, height: 1440 },
      },
    });
    expect(updateNodeData).toHaveBeenCalledWith("ws-compute", {
      runtime: "hermes",
      compute: {
        instance_type: "m6i.xlarge",
        volume: { root_gb: 100 },
        display: { mode: "desktop-control", protocol: "novnc", width: 2560, height: 1440 },
      },
      needsRestart: true,
      applyTemplateOnRestart: true,
    });
    expect(restartWorkspace).not.toHaveBeenCalled();
  });

  it("preserves existing custom display mode and resolution when saving unrelated compute", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "gpu-desktop-control", protocol: "dcv", width: 1600, height: 1000 },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Resolution")).toHaveProperty("value", "1600x1000");

    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "t3.xlarge" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-compute", {
      runtime: "claude-code",
      compute: {
        instance_type: "t3.xlarge",
        volume: { root_gb: 50 },
        display: { mode: "gpu-desktop-control", protocol: "dcv", width: 1600, height: 1000 },
      },
    });
  });

  it("can save changed compute and restart the workspace to apply it", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "t3.xlarge" } });
    fireEvent.click(screen.getByRole("button", { name: "Save & Restart" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: false }));
  });

  it("requests template re-apply when saving a runtime change and restarting", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Runtime image"), { target: { value: "hermes" } });
    fireEvent.click(screen.getByRole("button", { name: "Save & Restart" }));

    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: true }));
  });

  it("can restart without re-saving when changes are already pending", async () => {
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: true,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          applyTemplateOnRestart: true,
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Restart to apply" }));

    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: true }));
    expect(apiPatch).not.toHaveBeenCalled();
  });

  it("switches cloud provider — keys the instance-type list to the provider, confirms the recreate, and PATCHes the new provider", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(
      <ContainerConfigTab
        workspaceId="ws-switch"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "read-write",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    const providerSel = screen.getByLabelText("Cloud provider");
    expect(providerSel).toHaveProperty("value", "aws");
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.large");

    // Switch to Hetzner → the instance type resets to the Hetzner default (an AWS
    // t3.* is invalid on Hetzner) and the options become Hetzner sizes.
    fireEvent.change(providerSel, { target: { value: "hetzner" } });
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "cpx31");

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(confirmSpy).toHaveBeenCalled(); // destructive recreate confirmed
    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string; instance_type?: string } };
    expect(body.compute.provider).toBe("hetzner");
    expect(body.compute.instance_type).toBe("cpx31");
    confirmSpy.mockRestore();
  });

  // core#2489: the provider + instance-type dropdowns are populated from the
  // workspace-server SSOT (GET /compute/metadata), so the UI can't offer an
  // option the backend then rejects. This proves the fetch drives the
  // dropdowns: a server-only instance type appears once the fetch resolves.
  it("populates instance-type options from the /compute/metadata SSOT endpoint", async () => {
    apiGet.mockResolvedValueOnce({
      providers: ["aws", "hetzner", "gcp"],
      instanceTypes: {
        aws: ["t3.medium", "t3.large", "z9.future"],
        hetzner: ["cpx31"],
        gcp: ["e2-standard-2"],
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
    });

    render(
      <ContainerConfigTab
        workspaceId="ws-opts"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/compute/metadata"));
    // The server-only instance type appears in the dropdown after the fetch.
    await waitFor(() =>
      expect(
        Array.from(screen.getByLabelText("Instance type").querySelectorAll("option")).map((o) => o.getAttribute("value")),
      ).toContain("z9.future"),
    );
  });

  // core#2489: if the /compute/metadata fetch fails, the dropdowns must stay
  // usable via the in-bundle fallback (no crash, no empty selector).
  it("falls back to the in-bundle option set when the /compute/metadata fetch fails", async () => {
    apiGet.mockRejectedValueOnce(new Error("network down"));

    render(
      <ContainerConfigTab
        workspaceId="ws-opts"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // Fallback list still renders the known AWS sizes.
    const values = Array.from(
      screen.getByLabelText("Instance type").querySelectorAll("option"),
    ).map((o) => o.getAttribute("value"));
    expect(values).toContain("t3.medium");
    expect(values).toContain("m6i.xlarge");
  });

  // core#2489 SSOT pin: the in-bundle FALLBACK_COMPUTE_OPTIONS in
  // ContainerConfigTab.tsx is the last drift surface against the
  // workspace-server SSOT (the canvas already fetches the live
  // /compute/metadata when the server is reachable; the fallback
  // is the safety net for offline / 5xx / dev-mode). The test
  // asserts the FALLBACK mirrors the server's data shape so a
  // future server-side change (e.g. a new provider added to
  // workspaceComputeProvidersOrdered) is caught HERE rather than
  // surfacing as a silent empty dropdown in the field.
  //
  // Pinned against the workspace-server SSOT (workspace_compute.go):
  //   providers ordered: aws, hetzner, gcp
  //   aws instances (7): t3.medium, t3.large, t3.xlarge, t3.2xlarge,
  //                      m6i.large, m6i.xlarge, c6i.xlarge
  //   aws default: t3.medium
  //   hetzner instances (9): cpx11, cpx21, cpx31, cpx41, cpx51,
  //                          cax11, cax21, cax31, cax41
  //   hetzner default: cpx31
  //   gcp instances (5): e2-small, e2-medium, e2-standard-2,
  //                      e2-standard-4, e2-standard-8
  //   gcp default: e2-standard-2
  //   display defaults: aws="t3.xlarge", gcp="e2-standard-4",
  //                     hetzner="cpx41"
  //
  // The test exercises the fallback path by making the live fetch
  // fail; the assertions then read what the dropdowns actually
  // rendered, not a re-imported constant (so a future change to
  // the in-bundle fallback that breaks the UX is caught here, not
  // by a unit test on a constant that the UX would no longer
  // use).
  it("fallback instance-type dropdowns cover the full server-side SSOT (drift pin)", async () => {
    apiGet.mockRejectedValueOnce(new Error("server unreachable — fallback path"));

    render(
      <ContainerConfigTab
        workspaceId="ws-fallback-pin"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: { instance_type: "t3.medium", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Switch through each provider and assert the instance-type
    // dropdown contains the FULL SSOT set. Catches a future
    // server change that adds a new instance without
    // updating the canvas fallback (the canvas would silently
    // not offer the new size until the live fetch succeeds).
    const providers = ["aws", "hetzner", "gcp"] as const;
    const wantInstances: Record<typeof providers[number], string[]> = {
      aws: ["t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge", "m6i.large", "m6i.xlarge", "c6i.xlarge"],
      hetzner: ["cpx11", "cpx21", "cpx31", "cpx41", "cpx51", "cax11", "cax21", "cax31", "cax41"],
      gcp: ["e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8"],
    };
    for (const p of providers) {
      // The provider <select> drives the instance-type options.
      const providerSelect = screen.getByLabelText("Cloud provider") as HTMLSelectElement;
      fireEvent.change(providerSelect, { target: { value: p } });
      const instanceSelect = screen.getByLabelText("Instance type");
      const values = Array.from(instanceSelect.querySelectorAll("option")).map(
        (o) => o.getAttribute("value"),
      );
      for (const want of wantInstances[p]) {
        expect(values, `provider ${p} fallback missing ${want}`).toContain(want);
      }
    }
  });

  it("does not treat a non-provider edit as a recreate (no confirm; aws default omitted)", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(
      <ContainerConfigTab
        workspaceId="ws-noswitch"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "read-write",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "60" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(confirmSpy).not.toHaveBeenCalled();
    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string } };
    expect(body.compute.provider).toBeUndefined(); // aws default omitted (wire unchanged)
    confirmSpy.mockRestore();
  });
});
