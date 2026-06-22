// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  waitFor,
  cleanup,
  fireEvent,
} from "@testing-library/react";
import { LLMBillingSection } from "../llm-billing-section";

// Tests for LLMBillingSection (internal#691). Locks in:
//  - the section renders the resolved mode + source label
//  - the dropdown maps "inherit" → PUT {mode: null}
//  - the dropdown maps "byok" → PUT {mode: "byok"}
//  - a garbled override surfaces the warning banner
//  - the post-write resolution updates the UI without a refetch

const apiGet = vi.fn();
const apiPut = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => apiGet(...args),
    put: (...args: unknown[]) => apiPut(...args),
    post: vi.fn().mockResolvedValue({}),
    del: vi.fn().mockResolvedValue({}),
    patch: vi.fn().mockResolvedValue({}),
  },
}));

// Collapsed-by-default Section wrapper would hide the content; replace
// it with a passthrough so the dropdown is reachable in the test DOM.
vi.mock("../form-inputs", async () => {
  const actual = await vi.importActual<typeof import("../form-inputs")>(
    "../form-inputs",
  );
  return {
    ...actual,
    Section: ({ children }: { children: React.ReactNode }) => (
      <div>{children}</div>
    ),
  };
});

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

describe("LLMBillingSection — internal#691", () => {
  it("renders the resolved mode + source for an inherited workspace", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "ws-1",
      resolved_mode: "platform_managed",
      workspace_override: null,
      org_default: "platform_managed",
      source: "org_default",
    });

    render(<LLMBillingSection workspaceId="ws-1" />);

    await waitFor(() => {
      expect(apiGet).toHaveBeenCalledWith(
        "/admin/workspaces/ws-1/llm-billing-mode",
      );
    });
    // Resolved mode appears.
    expect(screen.getByText(/Resolved mode:/i).textContent).toMatch(/platform_managed/);
    // Source label appears.
    expect(
      screen.getByText(/inherited from org default/i),
    ).toBeTruthy();
  });

  it('PUTs {mode: "byok"} when user picks BYOK and reflects the new resolution', async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "ws-2",
      resolved_mode: "platform_managed",
      workspace_override: null,
      org_default: "platform_managed",
      source: "org_default",
    });
    apiPut.mockResolvedValueOnce({
      workspace_id: "ws-2",
      resolved_mode: "byok",
      workspace_override: "byok",
      org_default: "platform_managed",
      source: "workspace_override",
    });

    render(<LLMBillingSection workspaceId="ws-2" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    const select = (await screen.findByLabelText(
      /llm billing mode override/i,
    )) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "byok" } });

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/ws-2/llm-billing-mode",
        { mode: "byok" },
      );
    });
    // Post-write resolution propagated to UI.
    await waitFor(() => {
      expect(
        screen.getByText(/explicit override on this workspace/i),
      ).toBeTruthy();
    });
  });

  it("PUTs {mode: null} when user picks Inherit (clears the override)", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "ws-3",
      resolved_mode: "byok",
      workspace_override: "byok",
      org_default: "platform_managed",
      source: "workspace_override",
    });
    apiPut.mockResolvedValueOnce({
      workspace_id: "ws-3",
      resolved_mode: "platform_managed",
      workspace_override: null,
      org_default: "platform_managed",
      source: "org_default",
    });

    render(<LLMBillingSection workspaceId="ws-3" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    const select = (await screen.findByLabelText(
      /llm billing mode override/i,
    )) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "inherit" } });

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/ws-3/llm-billing-mode",
        { mode: null },
      );
    });
  });

  it("surfaces a warning banner when the override value is garbled", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "ws-4",
      resolved_mode: "platform_managed", // resolver fell through, default-closed
      workspace_override: "byokk", // typo persisted somehow
      org_default: "platform_managed",
      source: "org_default",
    });

    render(<LLMBillingSection workspaceId="ws-4" />);

    await waitFor(() => {
      expect(
        screen.getByText(/non-standard value/i),
      ).toBeTruthy();
    });
  });

  it("renders an error banner when the GET fails", async () => {
    apiGet.mockRejectedValueOnce(new Error("network down"));

    render(<LLMBillingSection workspaceId="ws-5" />);

    await waitFor(() => {
      expect(screen.getByText(/network down/i)).toBeTruthy();
    });
  });
});
