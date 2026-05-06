// @vitest-environment jsdom
//
// AttachmentPreview component tests — pin the dispatch contract:
// each kind goes to its dedicated renderer; kind=file falls back to
// the chip; failure modes don't strand the user without a download.
//
// Per RFC #2991 Phase 4: every test must be able to fail. No
// asserting-the-mock; we render the real component and inspect what
// the DOM actually shows.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// Mock the auth-token env var so AttachmentImage's fetch doesn't
// hit a real network. The fetch is itself mocked below.
vi.stubEnv("NEXT_PUBLIC_ADMIN_TOKEN", "test-token");

// Mock fetch so the AttachmentImage path can return a synthetic blob.
// Tests override per-case to simulate success / 404 / network fail.
const fetchMock = vi.fn();
beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  // jsdom doesn't implement URL.createObjectURL — stub.
  global.URL.createObjectURL = vi.fn(() => "blob:test-url");
  global.URL.revokeObjectURL = vi.fn();
});

import { AttachmentPreview } from "../AttachmentPreview";
import type { ChatAttachment } from "../types";

const onDownload = vi.fn();

function preview(att: ChatAttachment) {
  return render(
    <AttachmentPreview
      workspaceId="ws-1"
      attachment={att}
      onDownload={onDownload}
      tone="agent"
    />,
  );
}

describe("AttachmentPreview dispatch", () => {
  it("kind=file → renders the AttachmentChip download button (existing fallback)", () => {
    preview({ uri: "workspace:/workspace/tmp/foo.zip", name: "foo.zip", mimeType: "application/zip" });
    // The chip's button title is `Download <name>`. Pre-fix this was
    // the only render path; now it's the kind=file fallback.
    expect(screen.getByTitle(/Download foo\.zip/i)).toBeTruthy();
  });

  it("kind=image (mime) → renders the AttachmentImage path (loading placeholder until fetch resolves)", async () => {
    // never-resolving fetch → component sits in loading state. Pin
    // the loading placeholder shape.
    fetchMock.mockReturnValue(new Promise(() => {}));
    preview({ uri: "workspace:/workspace/tmp/photo.png", name: "photo.png", mimeType: "image/png" });
    expect(await screen.findByLabelText(/Loading photo\.png/i)).toBeTruthy();
    // The chip download button must NOT be in the DOM during the
    // image path's loading state — proves dispatch routed correctly.
    expect(screen.queryByTitle(/Download photo\.png/i)).toBeNull();
  });

  it("kind=image (extension fallback when mime is empty) → image path", async () => {
    fetchMock.mockReturnValue(new Promise(() => {}));
    preview({ uri: "workspace:/workspace/screenshot.jpg", name: "screenshot.jpg" /* no mime */ });
    expect(await screen.findByLabelText(/Loading screenshot\.jpg/i)).toBeTruthy();
  });

  it("kind=image fetch fails (404) → falls back to AttachmentChip so the user can still download", async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 404 });
    preview({ uri: "workspace:/workspace/tmp/missing.png", name: "missing.png", mimeType: "image/png" });
    // The fallback chip shows up on error.
    await waitFor(() => {
      expect(screen.getByTitle(/Download missing\.png/i)).toBeTruthy();
    });
  });

  it("kind=image fetch network error → falls back to chip", async () => {
    fetchMock.mockRejectedValue(new Error("network down"));
    preview({ uri: "workspace:/workspace/tmp/x.png", name: "x.png", mimeType: "image/png" });
    await waitFor(() => {
      expect(screen.getByTitle(/Download x\.png/i)).toBeTruthy();
    });
  });

  it("kind=image success → renders <img> + clicking opens the lightbox", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      blob: async () => new Blob(["fake-png-bytes"], { type: "image/png" }),
    });
    preview({ uri: "workspace:/workspace/tmp/ok.png", name: "ok.png", mimeType: "image/png" });

    // Image element shows up after the fetch resolves.
    const img = await screen.findByAltText(/ok\.png/);
    expect(img).toBeTruthy();
    expect((img as HTMLImageElement).src).toBe("blob:test-url");

    // Lightbox closed initially — the dialog must not be in the DOM.
    expect(screen.queryByRole("dialog")).toBeNull();

    // Click the thumbnail button (the surrounding <button>) → lightbox opens.
    const button = screen.getByLabelText(/Open ok\.png preview/i);
    fireEvent.click(button);

    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByLabelText(/Close preview/i)).toBeTruthy();
  });

  it("kind=image lightbox closes on Esc keypress", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      blob: async () => new Blob(["b"], { type: "image/png" }),
    });
    preview({ uri: "workspace:/workspace/tmp/x.png", name: "x.png", mimeType: "image/png" });
    await screen.findByAltText(/x\.png/);
    fireEvent.click(screen.getByLabelText(/Open x\.png preview/i));
    expect(await screen.findByRole("dialog")).toBeTruthy();

    // Esc on document — lightbox listens there per design (not on
    // the modal element) so the user can press Esc anywhere.
    act(() => {
      const event = new KeyboardEvent("keydown", { key: "Escape", bubbles: true });
      document.dispatchEvent(event);
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });
  });

  it("kind=image lightbox closes on backdrop click but not on inner content click", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      blob: async () => new Blob(["b"], { type: "image/png" }),
    });
    preview({ uri: "workspace:/workspace/tmp/x.png", name: "x.png", mimeType: "image/png" });
    await screen.findByAltText(/x\.png/);
    fireEvent.click(screen.getByLabelText(/Open x\.png preview/i));
    const dialog = await screen.findByRole("dialog");

    // Click on the inner content (the lightbox image) — must NOT close.
    const lightboxImg = dialog.querySelector("img");
    if (!lightboxImg) throw new Error("lightbox img missing");
    fireEvent.click(lightboxImg);
    expect(screen.queryByRole("dialog")).toBeTruthy();

    // Click on the backdrop (the dialog itself) — closes.
    fireEvent.click(dialog);
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });
  });

  it("kind=file is the universal fallback for unknown MIME (regression: don't try to preview a zip)", () => {
    // Critical safety: agent could attach a misnamed file. Pre-fix
    // the chip path was unconditional; we want unknown MIME to
    // STILL go to the chip even though the extension matches an
    // image kind.
    preview({ uri: "workspace:/workspace/tmp/x.docx", name: "x.docx", mimeType: "application/vnd.zip-disguised-as-doc" });
    expect(screen.getByTitle(/Download x\.docx/i)).toBeTruthy();
  });
});
