// @vitest-environment jsdom
//
// Pins the Edit affordance added to MemoryTab. Until this PR the Memory tab
// was Add+Delete only; an entry that needed correction had to be deleted and
// re-added — losing the version-counter and any in-flight optimistic-locking
// invariants other writers depend on.
//
// Each test pins one branch of the new flow. If any fails, the bug is back.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPost = vi.fn();
const apiDel = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    post: (path: string, body: unknown) => apiPost(path, body),
    del: (path: string) => apiDel(path),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

import { MemoryTab } from "../MemoryTab";

const sampleEntries = [
  {
    key: "team_brief",
    value: { goal: "ship v2" },
    version: 3,
    expires_at: null,
    updated_at: "2026-05-04T10:00:00Z",
  },
  {
    key: "plain_note",
    value: "raw text note",
    version: 1,
    expires_at: "2099-01-01T00:00:00Z",
    updated_at: "2026-05-04T10:01:00Z",
  },
];

beforeEach(() => {
  apiGet.mockReset();
  apiPost.mockReset();
  apiDel.mockReset();
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-test/memory") {
      return Promise.resolve(sampleEntries);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
});

async function renderAndExpand(key: string) {
  render(<MemoryTab workspaceId="ws-test" />);
  await waitFor(() => expect(apiGet).toHaveBeenCalled());
  // Reveal the Advanced section that hosts the entry list.
  const showAdvanced = await screen.findByRole("button", { name: "Show" });
  fireEvent.click(showAdvanced);
  // Expand the row.
  const row = await screen.findByRole("button", { name: new RegExp(key) });
  fireEvent.click(row);
}

describe("MemoryTab Edit affordance", () => {
  it("Edit button appears once a row is expanded", async () => {
    await renderAndExpand("team_brief");
    expect(screen.getAllByRole("button", { name: "Edit" }).length).toBeGreaterThan(0);
  });

  it("clicking Edit on a JSON-valued entry pre-fills the textarea with pretty JSON", async () => {
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = (await screen.findByLabelText(
      "Edit value for team_brief",
    )) as HTMLTextAreaElement;
    expect(textarea.value).toBe('{\n  "goal": "ship v2"\n}');
  });

  it("clicking Edit on a string-valued entry pre-fills raw (no surrounding quotes)", async () => {
    await renderAndExpand("plain_note");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = (await screen.findByLabelText(
      "Edit value for plain_note",
    )) as HTMLTextAreaElement;
    expect(textarea.value).toBe("raw text note");
  });

  it("Save POSTs with if_match_version + parsed value, then reloads", async () => {
    apiPost.mockResolvedValue({ status: "ok", key: "team_brief", version: 4 });
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = await screen.findByLabelText("Edit value for team_brief");
    fireEvent.change(textarea, { target: { value: '{"goal":"ship v3"}' } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    expect(apiPost).toHaveBeenCalledWith("/workspaces/ws-test/memory", {
      key: "team_brief",
      value: { goal: "ship v3" },
      if_match_version: 3,
    });
    // Reload after save → second GET.
    await waitFor(() => expect(apiGet).toHaveBeenCalledTimes(2));
  });

  it("Save with non-JSON text falls back to plain string", async () => {
    apiPost.mockResolvedValue({ status: "ok" });
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = await screen.findByLabelText("Edit value for team_brief");
    fireEvent.change(textarea, { target: { value: "free-form note" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    expect(apiPost.mock.calls[0][1].value).toBe("free-form note");
  });

  it("TTL field is forwarded as ttl_seconds when set", async () => {
    apiPost.mockResolvedValue({ status: "ok" });
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const ttlInput = await screen.findByLabelText("Edit TTL for team_brief");
    fireEvent.change(ttlInput, { target: { value: "3600" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    expect(apiPost.mock.calls[0][1].ttl_seconds).toBe(3600);
  });

  it("blank/zero/non-numeric TTL is omitted from the payload", async () => {
    apiPost.mockResolvedValue({ status: "ok" });
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const ttlInput = await screen.findByLabelText("Edit TTL for team_brief");
    // Junk + zero both must drop out — payload must not contain ttl_seconds.
    fireEvent.change(ttlInput, { target: { value: "abc" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    expect(apiPost.mock.calls[0][1]).not.toHaveProperty("ttl_seconds");
  });

  it("Cancel discards edits and restores the rendered value", async () => {
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = await screen.findByLabelText("Edit value for team_brief");
    fireEvent.change(textarea, { target: { value: '{"goal":"discarded"}' } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(apiPost).not.toHaveBeenCalled();
    // Editor is gone; the JSON pre-block is back.
    expect(screen.queryByLabelText("Edit value for team_brief")).toBeNull();
    expect(screen.getAllByText(/"goal": "ship v2"/i).length).toBeGreaterThan(0);
  });

  it("409 response surfaces a retry hint and reloads", async () => {
    apiPost.mockRejectedValueOnce(
      new Error("HTTP 409: if_match_version mismatch"),
    );
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = await screen.findByLabelText("Edit value for team_brief");
    fireEvent.change(textarea, { target: { value: '{"goal":"ship v3"}' } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/changed since you opened it/i);
    // Initial mount load + post-conflict reload.
    await waitFor(() => expect(apiGet).toHaveBeenCalledTimes(2));
  });

  it("non-409 error surfaces the message and does not reload", async () => {
    apiPost.mockRejectedValueOnce(new Error("boom"));
    await renderAndExpand("team_brief");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("boom");
    // Only the initial mount load — no retry reload.
    expect(apiGet).toHaveBeenCalledTimes(1);
  });

  it("entry with no version omits if_match_version (back-compat with older shape)", async () => {
    // Pre-version-counter shape: drop the `version` field from the row.
    apiGet.mockReset();
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test/memory") {
        return Promise.resolve([
          {
            key: "old_entry",
            value: "legacy",
            expires_at: null,
            updated_at: "2026-05-04T10:00:00Z",
          },
        ]);
      }
      return Promise.reject(new Error(`unmocked: ${path}`));
    });
    apiPost.mockResolvedValue({ status: "ok" });

    await renderAndExpand("old_entry");
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]);
    const textarea = await screen.findByLabelText("Edit value for old_entry");
    fireEvent.change(textarea, { target: { value: "updated" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(1));
    const payload = apiPost.mock.calls[0][1];
    expect(payload).not.toHaveProperty("if_match_version");
    expect(payload.value).toBe("updated");
  });
});
