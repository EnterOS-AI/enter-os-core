// @vitest-environment jsdom
//
// SaaS-mode coverage for the per-workspace cloud-provider picker. The main
// CreateWorkspaceDialog.test.tsx runs non-SaaS (the picker is hidden and the
// payload omits `provider`); this file forces SaaS by mocking isSaaSTenant so
// the picker renders and the selected provider flows into compute.provider.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { CreateWorkspaceButton } from "../CreateWorkspaceDialog";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn() },
}));

// Force SaaS so the Cloud provider picker is shown and the payload carries it.
vi.mock("@/lib/tenant", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/tenant")>()),
  isSaaSTenant: () => true,
}));

import { api } from "@/lib/api";

const mockGet = vi.mocked(api.get);
const mockPost = vi.mocked(api.post);

const SAMPLE_TEMPLATES = [
  {
    id: "claude-code-default",
    name: "Claude Code Agent",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax"],
    models: [{ id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] }],
  },
];

beforeEach(() => {
  vi.clearAllMocks();
  mockGet.mockImplementation(async (url: string) => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if (url === "/templates") return SAMPLE_TEMPLATES as any;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    return [] as any;
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockPost.mockResolvedValue({} as any);
});

afterEach(() => cleanup());

async function openDialog() {
  render(<CreateWorkspaceButton />);
  const btn = screen.getAllByRole("button").find((b) => b.textContent?.includes("New Workspace"));
  fireEvent.click(btn!);
  await waitFor(() => expect(screen.getByText("Create Workspace")).toBeTruthy());
}

describe("CreateWorkspaceDialog — cloud provider (SaaS)", () => {
  it("shows the Cloud provider picker, defaulting to AWS", async () => {
    await openDialog();
    const select = screen.getByLabelText("Cloud provider") as HTMLSelectElement;
    expect(select).toBeTruthy();
    expect(select.value).toBe("aws");
  });

  it("defaults compute.provider to aws when the picker is untouched", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), { target: { value: "AWS Agent" } });
    fireEvent.click(screen.getAllByRole("button").find((b) => b.textContent === "Create")!);
    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toMatchObject({ provider: "aws" });
  });

  it("threads the selected cloud provider into compute.provider", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), { target: { value: "GCP Agent" } });
    fireEvent.change(screen.getByLabelText("Cloud provider"), { target: { value: "gcp" } });
    fireEvent.click(screen.getAllByRole("button").find((b) => b.textContent === "Create")!);
    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toMatchObject({ provider: "gcp" });
  });
});
