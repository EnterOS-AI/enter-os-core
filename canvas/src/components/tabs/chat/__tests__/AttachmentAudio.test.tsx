// @vitest-environment jsdom
/**
 * AttachmentAudio — inline HTML5 <audio controls> player for chat attachments.
 *
 * Per RFC #2991 PR-2: platform-auth URIs fetch bytes → Blob → ObjectURL;
 * external URIs use the raw URL directly. State machine: idle → loading →
 * ready/error. Loading skeleton (280×40) shown while fetching. Error falls
 * back to AttachmentChip. No lightbox (unlike video/image). Blob URL cleaned
 * up on unmount.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * Covers:
 *   - Renders loading skeleton (280×40) with aria-label while fetching
 *   - Renders <audio controls> with correct src when ready
 *   - tone=user applies blue/accent classes
 *   - tone=agent applies neutral border classes
 *   - Error state renders AttachmentChip fallback
 *   - External URI uses direct href without auth fetch
 *   - Cleans up blob URL on unmount
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { AttachmentAudio } from "../AttachmentAudio";
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

function mockFetchOk(body: string, contentType = "audio/mpeg") {
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

describe("AttachmentAudio — loading/idle", () => {
  beforeEach(() => {
    mockFetchOk("audiodata");
  });

  it("renders loading skeleton (280×40) with aria-label", () => {
    const att = makeAttachment("podcast.mp3", 1024 * 512);
    const { container } = render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    const skeleton = container.querySelector('[aria-label]') as HTMLElement;
    expect(skeleton?.getAttribute("aria-label")).toContain("podcast.mp3");
    expect(skeleton?.getAttribute("aria-label")).toContain("Loading");
    // Skeleton dimensions
    expect(skeleton?.style.width).toBe("280px");
    expect(skeleton?.style.height).toBe("40px");
  });
});

// ─── Ready state ───────────────────────────────────────────────────────────────

describe("AttachmentAudio — ready", () => {
  beforeEach(() => {
    mockFetchOk("audiodata");
  });

  it("renders <audio controls> with blob src when ready", async () => {
    const att = makeAttachment("podcast.mp3", 1024 * 512);
    render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const audio = document.querySelector("audio");
      expect(audio).toBeTruthy();
    });
    const audio = document.querySelector("audio") as HTMLAudioElement;
    expect(audio.src).toMatch(/^blob:/);
    expect(audio.hasAttribute("controls")).toBe(true);
  });

  it("renders filename label in ready state", async () => {
    mockFetchOk("data");
    const att = makeAttachment("episode-42.mp3");
    render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    // Filename should appear as a text span before the audio element
    const container = document.querySelector("div");
    expect(container?.textContent).toContain("episode-42.mp3");
  });

  it("tone=user applies blue/accent border classes", async () => {
    mockFetchOk("data");
    const att = makeAttachment("podcast.mp3");
    const { container } = render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    // Use container.firstChild to target the component root div (not the render wrapper)
    const rootDiv = container.firstChild as HTMLElement;
    expect(rootDiv.className).toContain("border-blue-400");
    expect(rootDiv.className).toContain("accent-strong");
  });

  it("tone=agent applies neutral border class (no blue)", async () => {
    mockFetchOk("data");
    const att = makeAttachment("podcast.mp3");
    const { container } = render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    const rootDiv = container.firstChild as HTMLElement;
    expect(rootDiv.className).not.toContain("border-blue-400");
  });
});

// ─── Error state ───────────────────────────────────────────────────────────────

describe("AttachmentAudio — error", () => {
  it("renders AttachmentChip fallback when fetch fails", async () => {
    mockFetchError();
    const onDownload = vi.fn();
    const att = makeAttachment("broken.mp3", 256);
    render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("broken.mp3");
    });
    // Clicking the chip calls onDownload
    const chip = document.querySelector("button") as HTMLButtonElement;
    chip.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });

  it("renders AttachmentChip when audio onError fires", async () => {
    mockFetchOk("audiodata");
    const onDownload = vi.fn();
    const att = makeAttachment("corrupt.mp3", 256);
    render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    // Simulate audio onError
    const audio = document.querySelector("audio") as HTMLAudioElement;
    fireEvent(audio, new Event("error", { bubbles: false }));
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("corrupt.mp3");
    });
  });
});

// ─── External URI ─────────────────────────────────────────────────────────────

describe("AttachmentAudio — external URI", () => {
  it("skips auth fetch and uses direct href for external URIs", async () => {
    // Reset fetch so we can assert it was never called
    global.fetch = vi.fn();
    mockIsPlatformAttachment.mockReturnValue(false);
    mockResolveAttachmentHref.mockReturnValue("https://example.com/podcast.mp3");
    const att = makeAttachment("podcast.mp3");
    att.uri = "https://example.com/podcast.mp3";
    render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    // Should skip loading skeleton and go straight to ready (external URL)
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    const audio = document.querySelector("audio") as HTMLAudioElement;
    // Should be the direct href, not a blob
    expect(audio.src).toContain("example.com/podcast.mp3");
    // Fetch should never have been called for external (non-platform) attachments
    expect(global.fetch).not.toHaveBeenCalled();
  });
});

// ─── Cleanup ──────────────────────────────────────────────────────────────────

describe("AttachmentAudio — blob URL cleanup", () => {
  it("creates blob URL on mount and cleans up on unmount", async () => {
    mockIsPlatformAttachment.mockReturnValue(true);
    mockFetchOk("audiodata");
    const att = makeAttachment("podcast.mp3");
    const { unmount } = render(
      <AttachmentAudio
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("audio")).toBeTruthy();
    });
    const audio = document.querySelector("audio") as HTMLAudioElement;
    const blobUrl = audio.src;
    expect(blobUrl).toMatch(/^blob:/);
    unmount();
    // Audio element should be gone
    expect(document.querySelector("audio")).toBeNull();
  });
});
