import { describe, it, expect, beforeEach, vi } from "vitest";

global.fetch = vi.fn();

import {
  fetchOrgTemplates,
  importOrgTemplate,
  type OrgTemplate,
} from "../TemplatePalette";

const mockFetch = global.fetch as ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.clearAllMocks();
});

describe("fetchOrgTemplates", () => {
  it("returns the parsed list when the platform responds 200", async () => {
    const sample: OrgTemplate[] = [
      { dir: "molecule-dev", name: "Molecule AI Dev Team", description: "PM + research + dev", workspaces: 11 },
      { dir: "acme-demo", name: "Reno Stars", description: "compact 6-agent team", workspaces: 6 },
    ];
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => sample,
    });
    const result = await fetchOrgTemplates();
    expect(result).toEqual(sample);
    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining("/org/templates"),
      expect.objectContaining({ method: "GET" })
    );
  });

  it("returns [] when the platform errors so the UI shows the empty state", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      text: async () => "boom",
    });
    expect(await fetchOrgTemplates()).toEqual([]);
  });

  it("returns [] when the network call rejects", async () => {
    mockFetch.mockRejectedValueOnce(new Error("offline"));
    expect(await fetchOrgTemplates()).toEqual([]);
  });
});

describe("importOrgTemplate", () => {
  it("POSTs the dir to /org/import", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => ({ created: 11 }) });
    await importOrgTemplate("molecule-dev");
    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining("/org/import"),
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ dir: "molecule-dev" }),
      })
    );
  });

  it("propagates platform errors so the caller can surface them", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 404,
      text: async () => "org template not found: missing",
    });
    await expect(importOrgTemplate("missing")).rejects.toThrow(/404.*not found/);
  });

  it("propagates network failures verbatim", async () => {
    mockFetch.mockRejectedValueOnce(new Error("offline"));
    await expect(importOrgTemplate("x")).rejects.toThrow("offline");
  });

  it("treats 2xx with `error` field as a failure (StatusMultiStatus partial)", async () => {
    // Server returns 207 — `api.post` treats the 2xx as success and
    // returns the body. Without the post-check, a partial failure
    // (0 workspaces created) would surface as a green "Imported"
    // toast and the user would see no canvas change.
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 207,
      json: async () => ({
        org: "Data Team",
        workspaces: [],
        count: 0,
        error: 'pq: column "collapsed" of relation "workspaces" does not exist',
      }),
    });
    await expect(importOrgTemplate("data-team")).rejects.toThrow(
      /collapsed.*relation.*workspaces.*created 0 workspaces/,
    );
  });
});

describe("module exports", () => {
  it("exports the OrgTemplatesSection component", async () => {
    const mod = await import("../TemplatePalette");
    expect(mod.OrgTemplatesSection).toBeDefined();
    expect(typeof mod.OrgTemplatesSection).toBe("function");
  });
});
