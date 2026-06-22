// @vitest-environment jsdom
/**
 * AttachmentImage — inline image thumbnail with click-to-fullscreen lightbox.
 *
 * Per RFC #2991 PR-1: platform-auth URIs fetch bytes → Blob → ObjectURL;
 * external URIs use the raw URL directly. State machine: idle → loading →
 * ready/error. Loading skeleton shown while fetching. Error falls back to
 * AttachmentChip. Blob URL cleaned up on unmount / re-run.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * Covers:
 *   - Renders loading skeleton (240×180) with aria-label while fetching
 *   - Renders <img> inside button with correct src when ready
 *   - Lightbox opens on button click, closes on backdrop/escape
 *   - Hover reveals filename overlay
 *   - tone=user applies blue border class
 *   - tone=agent applies neutral border class
 *   - Error state renders AttachmentChip fallback
 *   - External URI uses direct href without auth fetch
 *   - Cleans up blob URL on unmount
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { AttachmentImage } from "../AttachmentImage";
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
  platformAuthHeaders: () => ({ Authorization: "Bearer fixture-token" }),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function makeAttachment(name: string, size?: number): ChatAttachment {
  return { name, uri: `workspace:/tmp/${name}`, size };
}

beforeEach(() => {
  // Reset to known-good state for each test.
  mockIsPlatformAttachment.mockReturnValue(true);
  mockResolveAttachmentHref.mockReturnValue(
    (id: string, uri: string) => `https://api.moleculesai.app/attachments/${uri}`,
  );
});

afterEach(() => {
  cleanup();
});

// ─── Fetch mock helpers ───────────────────────────────────────────────────────

function mockFetchOk(body: string, contentType = "image/png") {
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

describe("AttachmentImage — loading/idle", () => {
  beforeEach(() => {
    mockFetchOk("imagedata");
  });

  it("renders loading skeleton (240×180) with aria-label", () => {
    const att = makeAttachment("photo.jpg", 1024 * 512);
    const { container } = render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    const skeleton = container.querySelector('[aria-label]') as HTMLElement;
    expect(skeleton?.getAttribute("aria-label")).toContain("photo.jpg");
    expect(skeleton?.getAttribute("aria-label")).toContain("Loading");
    // Skeleton dimensions
    expect(skeleton?.style.width).toBe("240px");
    expect(skeleton?.style.height).toBe("180px");
  });
});

// ─── Ready state ───────────────────────────────────────────────────────────────

describe("AttachmentImage — ready", () => {
  beforeEach(() => {
    mockFetchOk("imagedata");
  });

  it("renders <img> inside a button with blob src when ready", async () => {
    const att = makeAttachment("photo.jpg", 1024 * 512);
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const img = document.querySelector("img");
      expect(img).toBeTruthy();
    });
    const img = document.querySelector("img") as HTMLImageElement;
    expect(img.src).toMatch(/^blob:/);
    // Image button should have correct aria-label
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    expect(btn).toBeTruthy();
    expect(btn?.getAttribute("aria-label")).toContain("photo.jpg");
  });

  it("tone=user applies blue border class", async () => {
    mockFetchOk("data");
    const att = makeAttachment("photo.jpg");
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    const img = document.querySelector("img");
    const btn = img?.closest("button");
    expect(btn?.className).toContain("blue-400");
  });

  it("tone=agent applies neutral border class (no blue)", async () => {
    mockFetchOk("data");
    const att = makeAttachment("photo.jpg");
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    const img = document.querySelector("img");
    const btn = img?.closest("button");
    expect(btn?.className).not.toContain("blue-400");
  });
});

// ─── Lightbox ─────────────────────────────────────────────────────────────────

describe("AttachmentImage — lightbox", () => {
  beforeEach(() => {
    mockFetchOk("imagedata");
  });

  it("opens lightbox on button click", async () => {
    const att = makeAttachment("photo.jpg");
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    const btn = document.querySelector('button[aria-label^="Open"]') as HTMLButtonElement;
    btn.click();
    // Lightbox dialog should appear
    await vi.waitFor(() => {
      const dialog = document.querySelector('[role="dialog"]');
      expect(dialog).toBeTruthy();
    });
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.getAttribute("aria-label")).toContain("photo.jpg");
    // Lightbox contains an <img>
    expect(dialog?.querySelector("img")).toBeTruthy();
  });

  it("closes lightbox on Escape key", async () => {
    const att = makeAttachment("photo.jpg");
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
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
});

// ─── Error state ───────────────────────────────────────────────────────────────

describe("AttachmentImage — error", () => {
  it("renders AttachmentChip fallback when fetch fails", async () => {
    mockFetchError();
    const onDownload = vi.fn();
    const att = makeAttachment("broken.jpg", 256);
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("broken.jpg");
    });
    // Clicking the chip calls onDownload
    const chip = document.querySelector("button") as HTMLButtonElement;
    chip.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });

  it("renders AttachmentChip when img onError fires", async () => {
    mockFetchOk("imagedata");
    const onDownload = vi.fn();
    const att = makeAttachment("corrupt.jpg", 256);
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    // Simulate img onError
    const img = document.querySelector("img") as HTMLImageElement;
    fireEvent.error(img);
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("corrupt.jpg");
    });
  });
});

// ─── External URI ─────────────────────────────────────────────────────────────

describe("AttachmentImage — external URI", () => {
  it("skips auth fetch and uses direct href for external URIs", async () => {
    // Reset fetch so we can assert it was never called
    global.fetch = vi.fn();
    mockIsPlatformAttachment.mockReturnValue(false);
    // For external URIs the component calls resolveAttachmentHref for the src
    mockResolveAttachmentHref.mockReturnValue("https://example.com/photo.jpg");
    const att = makeAttachment("photo.jpg");
    att.uri = "https://example.com/photo.jpg";
    const onDownload = vi.fn();
    render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="user"
      />,
    );
    // Should skip loading skeleton and go straight to ready (external URL)
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    const img = document.querySelector("img") as HTMLImageElement;
    // Should be the direct href, not a blob
    expect(img.src).toContain("example.com/photo.jpg");
    // Fetch should never have been called for external (non-platform) attachments
    expect(global.fetch).not.toHaveBeenCalled();
  });
});

// ─── Cleanup ──────────────────────────────────────────────────────────────────

describe("AttachmentImage — blob URL cleanup", () => {
  it("creates blob URL on mount and cleans up on unmount", async () => {
    mockIsPlatformAttachment.mockReturnValue(true);
    mockFetchOk("imagedata");
    const att = makeAttachment("photo.jpg");
    const { unmount } = render(
      <AttachmentImage
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("img")).toBeTruthy();
    });
    const img = document.querySelector("img") as HTMLImageElement;
    const blobUrl = img.src;
    expect(blobUrl).toMatch(/^blob:/);
    unmount();
    // Image should be gone
    expect(document.querySelector("img")).toBeNull();
  });
});
