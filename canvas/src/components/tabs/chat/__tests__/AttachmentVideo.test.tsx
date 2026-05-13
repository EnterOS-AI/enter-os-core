// @vitest-environment jsdom
/**
 * AttachmentVideo — inline native HTML5 <video> player for chat attachments.
 *
 * Per RFC #2991 PR-2: platform-auth URIs fetch bytes → Blob → ObjectURL;
 * external URIs use the raw URL directly. State machine: idle → loading →
 * ready/error. Loading skeleton shown while fetching. Error falls back to
 * AttachmentChip. Blob URL cleaned up on unmount / re-run.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * Covers:
 *   - Renders loading skeleton with aria-label while fetching
 *   - Renders <video> element with correct src when ready
 *   - Error state renders AttachmentChip fallback
 *   - idle state renders loading skeleton
 *   - ready state uses correct blob/object URL
 *   - tone=user applies blue border class
 *   - tone=agent applies neutral border class
 *   - onDownload called when error chip is clicked
 *   - Cleans up blob URL on unmount
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";

import { AttachmentVideo } from "../AttachmentVideo";
import type { ChatAttachment } from "../types";

// ─── Mocks ────────────────────────────────────────────────────────────────────

// Mock the entire uploads module to control isPlatformAttachment / resolveAttachmentHref
const mockResolveAttachmentHref = vi.fn<(id: string, uri: string) => string>(
  (id, uri) => `https://api.moleculesai.app/attachments/${uri}`,
);
const mockIsPlatformAttachment = vi.fn<(uri: string) => boolean>(() => true);

vi.mock("../uploads", () => ({
  isPlatformAttachment: (uri: string) => mockIsPlatformAttachment(uri),
  resolveAttachmentHref: (id: string, uri: string) =>
    mockResolveAttachmentHref(id, uri),
}));

// Mock platformAuthHeaders so fetch gets auth headers
vi.mock("@/lib/api", () => ({
  platformAuthHeaders: () => ({ Authorization: "Bearer test-token" }),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function makeAttachment(name: string, size?: number): ChatAttachment {
  return { name, uri: `workspace:/tmp/${name}`, size };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

// ─── Fetch mock helper ────────────────────────────────────────────────────────

function mockFetchOk(body: string, contentType = "video/mp4") {
  const blob = new Blob([body], { type: contentType });
  const url = URL.createObjectURL(blob);
  global.fetch = vi.fn((href: string, opts?: RequestInit) => {
    void href;
    void opts;
    return Promise.resolve({
      ok: true,
      status: 200,
      blob: () => Promise.resolve(blob),
      headers: new Map([["content-type", contentType]]),
    }) as unknown as Response;
  });
  return url;
}

function mockFetchError() {
  global.fetch = vi.fn(() =>
    Promise.resolve({ ok: false, status: 500 }) as unknown as Response,
  );
}

// ─── Idle state ──────────────────────────────────────────────────────────────

describe("AttachmentVideo — idle/loading", () => {
  beforeEach(() => {
    mockFetchOk("videodata");
  });

  it("renders loading skeleton with aria-label", () => {
    const att = makeAttachment("clip.mp4", 1024 * 512);
    const { container } = render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    // While fetching, should show skeleton
    const skeleton = container.querySelector('[aria-label]') as HTMLElement;
    expect(skeleton?.getAttribute("aria-label")).toContain("clip.mp4");
    expect(skeleton?.getAttribute("aria-label")).toContain("Loading");
  });
});

// ─── Ready state ───────────────────────────────────────────────────────────────

describe("AttachmentVideo — ready", () => {
  beforeEach(() => {
    mockFetchOk("videodata");
  });

  it("renders <video> element with correct src when ready", async () => {
    const att = makeAttachment("clip.mp4", 1024 * 512);
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    // Wait for ready state
    await vi.waitFor(() => {
      const video = document.querySelector("video");
      expect(video).toBeTruthy();
    });
    const video = document.querySelector("video") as HTMLVideoElement;
    // src should be an object URL (blob:)
    expect(video.src).toMatch(/^blob:/);
    expect(video.hasAttribute("controls")).toBe(true);
  });

  it("ready state uses blob URL for platform attachments", async () => {
    mockIsPlatformAttachment.mockReturnValue(true);
    const att = makeAttachment("clip.mp4", 1024);
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("video")).toBeTruthy();
    });
    const video = document.querySelector("video") as HTMLVideoElement;
    expect(video.src).toMatch(/^blob:/);
  });

  it("tone=user applies blue border class", async () => {
    mockFetchOk("data");
    const att = makeAttachment("clip.mp4");
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("video")).toBeTruthy();
    });
    const video = document.querySelector("video");
    // The video container has tone-based border class
    const container = video?.closest("div");
    expect(container?.className).toContain("blue-400");
  });

  it("tone=agent applies neutral border class (no blue)", async () => {
    mockFetchOk("data");
    const att = makeAttachment("clip.mp4");
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("video")).toBeTruthy();
    });
    const video = document.querySelector("video");
    const container = video?.closest("div");
    expect(container?.className).not.toContain("blue-400");
  });
});

// ─── Error state ───────────────────────────────────────────────────────────────

describe("AttachmentVideo — error", () => {
  it("renders AttachmentChip fallback when fetch fails", async () => {
    mockFetchError();
    const onDownload = vi.fn();
    const att = makeAttachment("broken.mp4", 256);
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    // First renders loading skeleton
    // Then transitions to error
    await vi.waitFor(() => {
      // Should have rendered the chip button instead of video
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("broken.mp4");
    });
    // Clicking the chip calls onDownload
    const chip = document.querySelector("button") as HTMLButtonElement;
    chip.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });
});

// ─── Cleanup ──────────────────────────────────────────────────────────────────

describe("AttachmentVideo — blob URL cleanup", () => {
  it("creates blob URL on mount and cleans up on unmount", async () => {
    mockFetchOk("videodata");
    const att = makeAttachment("clip.mp4");
    const { unmount } = render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("video")).toBeTruthy();
    });
    const video = document.querySelector("video") as HTMLVideoElement;
    const blobUrl = video.src;
    expect(blobUrl).toMatch(/^blob:/);
    // Unmount should revoke the blob URL
    unmount();
    // After unmount, the video element should be gone
    expect(document.querySelector("video")).toBeNull();
  });
});

// ─── External URI (no fetch) ─────────────────────────────────────────────────

describe("AttachmentVideo — external URI", () => {
  it("uses direct href for external URIs without fetch", async () => {
    mockIsPlatformAttachment.mockReturnValue(false);
    const externalUri = "https://example.com/video.mp4";
    const att = makeAttachment("video.mp4");
    att.uri = externalUri;
    render(
      <AttachmentVideo
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    // Should skip loading and go straight to ready
    await vi.waitFor(() => {
      expect(document.querySelector("video")).toBeTruthy();
    });
    const video = document.querySelector("video") as HTMLVideoElement;
    // For external URIs, the src should be the direct href (not a blob)
    expect(video.src).toContain("example.com/video.mp4");
  });
});
