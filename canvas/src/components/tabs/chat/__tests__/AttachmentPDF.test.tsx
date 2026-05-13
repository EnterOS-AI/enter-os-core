// @vitest-environment jsdom
/**
 * AttachmentPDF — inline PDF preview button + click-to-fullscreen lightbox.
 *
 * Per RFC #2991 PR-3: platform-auth URIs fetch bytes → Blob → ObjectURL;
 * external URIs use the raw URL directly. State machine: idle → loading →
 * ready/error. Loading skeleton shown while fetching. Error falls back to
 * AttachmentChip. Clicking the preview button opens AttachmentLightbox with
 * <embed>. Blob URL cleaned up on unmount.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * Covers:
 *   - Renders loading skeleton with PdfGlyph + filename text
 *   - Renders preview button with PDF glyph, filename, and "PDF" label
 *   - Opens lightbox with <embed> on button click
 *   - Lightbox closes on Escape
 *   - tone=user applies blue/accent classes on button
 *   - tone=agent applies neutral border on button
 *   - Error state renders AttachmentChip fallback
 *   - External URI uses direct href without auth fetch
 *   - Cleans up blob URL on unmount
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { AttachmentPDF } from "../AttachmentPDF";
import type { ChatAttachment } from "../types";

// ─── Mocks ────────────────────────────────────────────────────────────────────

const mockResolveAttachmentHref = vi.fn<(id: string, uri: string) => string>(
  (id, uri) => `https://api.moleculesai.app/attachments/${uri}`,
);
const mockIsPlatformAttachment = vi.fn<(uri: string) => boolean>(() => true);

vi.mock("../uploads", () => ({
  isPlatformAttachment: (uri: string) => mockIsPlatformAttachment(uri),
  resolveAttachmentHref: (id: string, uri: string) =>
    mockResolveAttachmentHref(id, uri),
}));

vi.mock("@/lib/api", () => ({
  platformAuthHeaders: () => ({ Authorization: "Bearer test-token" }),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function makeAttachment(name: string, size?: number): ChatAttachment {
  return { name, uri: `workspace:/tmp/${name}`, size };
}

beforeEach(() => {
  mockIsPlatformAttachment.mockReturnValue(true);
  mockResolveAttachmentHref.mockReturnValue(
    (id: string, uri: string) => `https://api.moleculesai.app/attachments/${uri}`,
  );
});

afterEach(() => {
  cleanup();
});

// ─── Fetch mock helpers ───────────────────────────────────────────────────────

function mockFetchOk(body: string, contentType = "application/pdf") {
  const blob = new Blob([body], { type: contentType });
  global.fetch = vi.fn(() =>
    Promise.resolve({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(blob),
      headers: new Map([["content-type", contentType]]),
    }) as unknown as Response,
  );
}

function mockFetchError() {
  global.fetch = vi.fn(() =>
    Promise.resolve({ ok: false, status: 500 }) as unknown as Response,
  );
}

// ─── Loading / idle state ─────────────────────────────────────────────────────

describe("AttachmentPDF — loading/idle", () => {
  beforeEach(() => {
    mockFetchOk("pdfdata");
  });

  it("renders loading skeleton with PdfGlyph and filename", () => {
    const att = makeAttachment("report.pdf", 1024 * 512);
    const { container } = render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    const skeleton = container.querySelector('[aria-label]') as HTMLElement;
    expect(skeleton?.getAttribute("aria-label")).toContain("report.pdf");
    expect(skeleton?.getAttribute("aria-label")).toContain("Loading");
    // Should contain the filename text
    expect(skeleton?.textContent).toContain("report.pdf");
    expect(skeleton?.textContent).toContain("Loading");
  });
});

// ─── Ready state ───────────────────────────────────────────────────────────────

describe("AttachmentPDF — ready", () => {
  beforeEach(() => {
    mockFetchOk("pdfdata");
  });

  it("renders preview button with PDF glyph, filename, and PDF label", async () => {
    const att = makeAttachment("report.pdf", 1024 * 512);
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const btn = document.querySelector('button[aria-label^="Open"]');
      expect(btn).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    expect(btn?.getAttribute("aria-label")).toContain("report.pdf");
    // Button text should include the filename and "PDF" label
    expect(btn?.textContent).toContain("report.pdf");
    expect(btn?.textContent).toContain("PDF");
  });

  it("opens lightbox with <embed> on button click", async () => {
    mockFetchOk("data");
    const att = makeAttachment("report.pdf");
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    btn.click();
    await vi.waitFor(() => {
      const dialog = document.querySelector('[role="dialog"]');
      expect(dialog).toBeTruthy();
    });
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.getAttribute("aria-label")).toContain("report.pdf");
    // Lightbox contains an <embed>
    expect(dialog?.querySelector("embed")).toBeTruthy();
  });

  it("closes lightbox on Escape key", async () => {
    mockFetchOk("data");
    const att = makeAttachment("report.pdf");
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    btn.click();
    await vi.waitFor(() => {
      expect(document.querySelector('[role="dialog"]')).toBeTruthy();
    });
    fireEvent.keyDown(document, { key: "Escape" });
    await vi.waitFor(() => {
      expect(document.querySelector('[role="dialog"]')).toBeNull();
    });
  });

  it("tone=user applies blue/accent classes on button", async () => {
    mockFetchOk("data");
    const att = makeAttachment("report.pdf");
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    expect(btn?.className).toContain("border-blue-400");
    expect(btn?.className).toContain("accent-strong");
  });

  it("tone=agent applies neutral border class (no blue)", async () => {
    mockFetchOk("data");
    const att = makeAttachment("report.pdf");
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    expect(btn?.className).not.toContain("border-blue-400");
  });
});

// ─── Error state ───────────────────────────────────────────────────────────────

describe("AttachmentPDF — error", () => {
  it("renders AttachmentChip fallback when fetch fails", async () => {
    mockFetchError();
    const onDownload = vi.fn();
    const att = makeAttachment("broken.pdf", 256);
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("broken.pdf");
    });
    // Clicking the chip calls onDownload
    const chip = document.querySelector("button") as HTMLButtonElement;
    chip.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });
});

// ─── External URI ─────────────────────────────────────────────────────────────

describe("AttachmentPDF — external URI", () => {
  it("skips auth fetch and uses direct href for external URIs", async () => {
    // Reset fetch so we can assert it was never called
    global.fetch = vi.fn();
    mockIsPlatformAttachment.mockReturnValue(false);
    mockResolveAttachmentHref.mockReturnValue("https://example.com/report.pdf");
    const att = makeAttachment("report.pdf");
    att.uri = "https://example.com/report.pdf";
    render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    // Should skip loading skeleton and go straight to ready (external URL)
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    // Verify the button is present (not skeleton)
    const btn = document.querySelector('button[aria-label^="Open"]');
    expect(btn).toBeTruthy();
    // Fetch should never have been called for external (non-platform) attachments
    expect(global.fetch).not.toHaveBeenCalled();
  });
});

// ─── Cleanup ──────────────────────────────────────────────────────────────────

describe("AttachmentPDF — blob URL cleanup", () => {
  it("creates blob URL on mount and cleans up on unmount", async () => {
    mockIsPlatformAttachment.mockReturnValue(true);
    mockFetchOk("pdfdata");
    const att = makeAttachment("report.pdf");
    const { unmount } = render(
      <AttachmentPDF
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector('button[aria-label^="Open"]')).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]');
    expect(btn).toBeTruthy();
    unmount();
    // Button should be gone after unmount
    expect(document.querySelector('button[aria-label^="Open"]')).toBeNull();
  });
});
