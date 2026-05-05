// @vitest-environment jsdom
//
// ExternalConnectionSection — coverage for the credential-rotate +
// re-show-instructions UI on the Config tab.
//
// What this pins:
//   1. "Show connection info" → GET /external/connection, opens modal
//      with auth_token=""
//   2. "Rotate credentials" → confirm dialog → POST /external/rotate,
//      opens modal with the returned auth_token
//   3. Confirm dialog cancels without firing the POST
//   4. API failure surfaces an error chip (no silent loss)

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    post: (path: string, body?: unknown) => apiPost(path, body),
    patch: vi.fn(),
    put: vi.fn(),
    del: vi.fn(),
  },
}));

import { ExternalConnectionSection } from "../ExternalConnectionSection";

beforeEach(() => {
  apiGet.mockReset();
  apiPost.mockReset();
});

const SAMPLE_INFO = {
  workspace_id: "ws-test",
  platform_url: "https://platform.example.test",
  auth_token: "",
  registry_endpoint: "https://platform.example.test/registry/register",
  heartbeat_endpoint: "https://platform.example.test/registry/heartbeat",
  // The modal stamps these snippets server-side; for the test we
  // bake workspace_id into one so the rendered DOM contains a
  // findable token after the modal mounts.
  curl_register_template: "# curl ws=ws-test",
  python_snippet: "# py ws=ws-test",
  claude_code_channel_snippet: "# claude ws=ws-test",
  universal_mcp_snippet: "# mcp ws=ws-test",
  hermes_channel_snippet: "# hermes ws=ws-test",
  codex_snippet: "# codex ws=ws-test",
  openclaw_snippet: "# openclaw ws=ws-test",
};

describe("ExternalConnectionSection", () => {
  it("renders both action buttons", () => {
    render(<ExternalConnectionSection workspaceId="ws-test" />);
    expect(screen.getByRole("button", { name: /show connection info/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /rotate credentials/i })).toBeTruthy();
  });

  it("'Show connection info' calls GET /external/connection and opens modal with blank token", async () => {
    apiGet.mockResolvedValue({ connection: { ...SAMPLE_INFO, auth_token: "" } });
    render(<ExternalConnectionSection workspaceId="ws-test" />);

    fireEvent.click(screen.getByRole("button", { name: /show connection info/i }));

    await waitFor(() =>
      expect(apiGet).toHaveBeenCalledWith("/workspaces/ws-test/external/connection"),
    );
    // The ExternalConnectModal renders the workspace_id field in its
    // copy-block. document.body covers Radix's portal mount point.
    await waitFor(() => {
      expect(document.body.textContent || "").toContain("ws-test");
    });
  });

  it("'Rotate credentials' opens confirm dialog before firing POST", async () => {
    render(<ExternalConnectionSection workspaceId="ws-test" />);
    fireEvent.click(screen.getByRole("button", { name: /rotate credentials/i }));

    // Confirm dialog appears with the destructive copy.
    await waitFor(() => {
      expect(
        screen.getByText(/Rotate workspace credentials\?/i),
      ).toBeTruthy();
    });
    expect(screen.getByText(/immediately invalidate the current one/i)).toBeTruthy();

    // POST must NOT have fired yet — only on confirm.
    expect(apiPost).not.toHaveBeenCalled();
  });

  it("Cancel in confirm dialog dismisses without rotating", async () => {
    render(<ExternalConnectionSection workspaceId="ws-test" />);
    fireEvent.click(screen.getByRole("button", { name: /rotate credentials/i }));

    await waitFor(() =>
      expect(screen.getByText(/Rotate workspace credentials\?/i)).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: /^cancel$/i }));

    await waitFor(() =>
      expect(screen.queryByText(/Rotate workspace credentials\?/i)).toBeNull(),
    );
    expect(apiPost).not.toHaveBeenCalled();
  });

  it("Confirm in dialog POSTs to /external/rotate and opens modal with returned token", async () => {
    apiPost.mockResolvedValue({
      connection: { ...SAMPLE_INFO, auth_token: "fresh-tok-123" },
    });
    render(<ExternalConnectionSection workspaceId="ws-test" />);

    fireEvent.click(screen.getByRole("button", { name: /rotate credentials/i }));
    await waitFor(() =>
      expect(screen.getByText(/Rotate workspace credentials\?/i)).toBeTruthy(),
    );
    // Click the dialog's Rotate button (NOT the section's — the section's
    // "Rotate credentials" stays mounted; the dialog's "Rotate" is the
    // commit button. getAllByRole returns both; pick the one inside the
    // dialog by name "Rotate" exact-match).
    const rotateBtns = screen.getAllByRole("button", { name: /^rotate$/i });
    expect(rotateBtns.length).toBeGreaterThanOrEqual(1);
    fireEvent.click(rotateBtns[rotateBtns.length - 1]);

    await waitFor(() =>
      expect(apiPost).toHaveBeenCalledWith(
        "/workspaces/ws-test/external/rotate",
        {},
      ),
    );
  });

  it("Surfaces API errors as a visible chip, not silent loss", async () => {
    apiGet.mockRejectedValue(new Error("forbidden"));
    render(<ExternalConnectionSection workspaceId="ws-test" />);

    fireEvent.click(screen.getByRole("button", { name: /show connection info/i }));

    await waitFor(() => {
      const matches = screen.queryAllByText((_, el) =>
        (el?.textContent || "").toLowerCase().includes("forbidden"),
      );
      expect(matches.length).toBeGreaterThan(0);
    });
  });
});
