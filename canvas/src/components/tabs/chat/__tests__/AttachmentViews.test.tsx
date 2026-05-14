// @vitest-environment jsdom
/**
 * AttachmentViews — pure presentational components for chat attachments.
 *
 * Covers:
 *   - PendingAttachmentPill renders file name, formatted size, × button
 *   - PendingAttachmentPill × button has correct aria-label
 *   - PendingAttachmentPill calls onRemove when × clicked
 *   - PendingAttachmentPill renders exactly one button
 *   - AttachmentChip renders attachment name and download glyph
 *   - AttachmentChip renders size when provided
 *   - AttachmentChip omits size span when size is undefined
 *   - AttachmentChip calls onDownload(attachment) on click
 *   - AttachmentChip title attribute for hover tooltip
 *   - AttachmentChip tone=user applies blue accent classes
 *   - AttachmentChip tone=agent applies surface classes
 *   - AttachmentChip renders exactly one button
 *
 * NOTE: No @testing-library/jest-dom import — use textContent / className /
 * getAttribute checks to avoid "expect is not defined" errors in this vitest
 * configuration.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import React from "react";

import { AttachmentChip, PendingAttachmentPill } from "../AttachmentViews";
import type { ChatAttachment } from "../types";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Helpers ────────────────────────────────────────────────────────────────────

/** Create a File with actual content so size > 0 in jsdom. */
function makeFile(name: string, content: string): File {
  return new File([content], name, { type: "application/octet-stream" });
}

function makeAttachment(name: string, size?: number): ChatAttachment {
  return { name, uri: `workspace:/tmp/${name}`, size };
}

// ─── PendingAttachmentPill ─────────────────────────────────────────────────────

describe("PendingAttachmentPill", () => {
  it("renders the file name", () => {
    const file = makeFile("report.pdf", "PDF content here");
    const { container } = render(
      <PendingAttachmentPill file={file} onRemove={vi.fn()} />,
    );
    expect(container.textContent).toContain("report.pdf");
  });

  it("renders the formatted file size (KB)", () => {
    // 50 KB = 50 * 1024 bytes
    const content = "x".repeat(50 * 1024);
    const file = makeFile("data.csv", content);
    const { container } = render(
      <PendingAttachmentPill file={file} onRemove={vi.fn()} />,
    );
    expect(container.textContent).toContain("50 KB");
  });

  it("renders 0 B for empty file", () => {
    const file = makeFile("empty.txt", "");
    const { container } = render(
      <PendingAttachmentPill file={file} onRemove={vi.fn()} />,
    );
    expect(container.textContent).toContain("0 B");
  });

  it("renders size in MB for files >= 1 MB", () => {
    // 2.5 MB = 2.5 * 1024 * 1024 bytes
    const content = "x".repeat(Math.round(2.5 * 1024 * 1024));
    const file = makeFile("video.mp4", content);
    const { container } = render(
      <PendingAttachmentPill file={file} onRemove={vi.fn()} />,
    );
    expect(container.textContent).toContain("2.5 MB");
  });

  it("× button has aria-label with file name", () => {
    const file = makeFile("notes.txt", "some content");
    render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
    const btn = screen.getByRole("button");
    expect(btn.getAttribute("aria-label")).toBe("Remove notes.txt");
  });

  it("calls onRemove when × button is clicked", () => {
    const file = makeFile("doc.pdf", "pdf data");
    const onRemove = vi.fn();
    render(<PendingAttachmentPill file={file} onRemove={onRemove} />);
    screen.getByRole("button").click();
    expect(onRemove).toHaveBeenCalledTimes(1);
  });

  it("renders exactly one button (the × remove button)", () => {
    const file = makeFile("img.png", "image bytes");
    const { container } = render(
      <PendingAttachmentPill file={file} onRemove={vi.fn()} />,
    );
    expect(container.querySelectorAll("button")).toHaveLength(1);
  });
});

// ─── AttachmentChip ───────────────────────────────────────────────────────────

describe("AttachmentChip", () => {
  it("renders the attachment name", () => {
    const att = makeAttachment("chart.svg", 2048);
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="user" />,
    );
    expect(container.textContent).toContain("chart.svg");
  });

  it("renders size when provided", () => {
    const att = makeAttachment("dump.sql", 1024 * 150); // 150 KB
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="user" />,
    );
    expect(container.textContent).toContain("150 KB");
  });

  it("omits size span when attachment.size is undefined", () => {
    const att = makeAttachment("notes.md"); // no size
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="user" />,
    );
    // The only <span> should be the truncated filename; no size <span>
    const spans = Array.from(container.querySelectorAll("span"));
    const sizeSpans = spans.filter(
      (s) => s.className && s.className.includes("tabular-nums"),
    );
    expect(sizeSpans).toHaveLength(0);
  });

  it("has title attribute with download hint", () => {
    const att = makeAttachment("readme.txt", 64);
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="agent" />,
    );
    const btn = container.querySelector("button");
    expect(btn?.getAttribute("title")).toBe("Download readme.txt");
  });

  it("calls onDownload with the attachment on click", () => {
    const att = makeAttachment("export.csv", 8192);
    const onDownload = vi.fn();
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />,
    );
    container.querySelector("button")!.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });

  it("tone=user applies blue accent class", () => {
    const att = makeAttachment("photo.jpg", 512);
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="user" />,
    );
    const btn = container.querySelector("button")!;
    expect(btn.className).toContain("blue-400");
  });

  it("tone=agent does not apply blue accent class", () => {
    const att = makeAttachment("photo.jpg", 512);
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="agent" />,
    );
    const btn = container.querySelector("button")!;
    expect(btn.className).not.toContain("blue-400");
  });

  it("renders exactly one button", () => {
    const att = makeAttachment("icon.svg", 128);
    const { container } = render(
      <AttachmentChip attachment={att} onDownload={vi.fn()} tone="user" />,
    );
    expect(container.querySelectorAll("button")).toHaveLength(1);
  });
});
