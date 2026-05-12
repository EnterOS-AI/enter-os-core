// @vitest-environment jsdom
/**
 * Tests for AttachmentViews.tsx — PendingAttachmentPill + AttachmentChip.
 *
 * 16 cases covering:
 * - PendingAttachmentPill: name, size, aria-label, onRemove, one-button guard
 * - AttachmentChip: name+glyph, size, no-size, title, onDownload, tone=user/agent, one-button guard
 *
 * Pattern: render the real component, inspect actual DOM output.
 * No mocking of the components themselves.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import React from "react";

import {
  PendingAttachmentPill,
  AttachmentChip,
} from "../AttachmentViews";
import type { ChatAttachment } from "../types";

afterEach(cleanup);

// ─── Shared test fixtures ────────────────────────────────────────────────────

const makeFile = (name: string, size: number): File =>
  new File([new Uint8Array(size)], name, { type: "application/octet-stream" });

const makeAttachment = (overrides: Partial<ChatAttachment> = {}): ChatAttachment => ({
  name: "report.pdf",
  uri: "workspace:/workspace/report.pdf",
  mimeType: "application/pdf",
  size: 42_000,
  ...overrides,
});

// ─── PendingAttachmentPill ───────────────────────────────────────────────────

describe("PendingAttachmentPill", () => {
  describe("renders", () => {
    it("displays the file name", () => {
      const file = makeFile("notes.txt", 128);
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      expect(screen.getByText("notes.txt")).toBeTruthy();
    });

    it("displays formatted size in bytes", () => {
      // File([], name) gives size 0; pass a Uint8Array to set actual byte size.
      const file = new File([new Uint8Array(512)], "tiny.bin");
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      expect(screen.getByText("512 B")).toBeTruthy();
    });

    it("displays formatted size in KB", () => {
      const file = new File([new Uint8Array(5 * 1024)], "medium.zip");
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      expect(screen.getByText("5 KB")).toBeTruthy();
    });

    it("displays formatted size in MB", () => {
      const file = new File([new Uint8Array(Math.floor(1.5 * 1024 * 1024))], "large.tar");
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      // formatSize uses toFixed(1) for MB → "1.5 MB"
      expect(screen.getByText("1.5 MB")).toBeTruthy();
    });

    it('× button has aria-label "Remove <filename>"', () => {
      const file = makeFile("memo.pdf", 1_000);
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      expect(screen.getByRole("button", { name: /remove memo\.pdf/i })).toBeTruthy();
    });

    it("calls onRemove when × button is clicked", () => {
      const onRemove = vi.fn();
      const file = makeFile("photo.png", 999);
      render(<PendingAttachmentPill file={file} onRemove={onRemove} />);
      fireEvent.click(screen.getByRole("button", { name: /remove photo\.png/i }));
      expect(onRemove).toHaveBeenCalledTimes(1);
    });

    it("renders exactly one button (no stray click targets)", () => {
      const file = makeFile("doc.docx", 20_000);
      render(<PendingAttachmentPill file={file} onRemove={vi.fn()} />);
      const buttons = screen.getAllByRole("button");
      expect(buttons).toHaveLength(1);
    });
  });
});

// ─── AttachmentChip ────────────────────────────────────────────────────────

describe("AttachmentChip", () => {
  let onDownload: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    onDownload = vi.fn();
  });

  describe("renders", () => {
    it("displays the attachment name", () => {
      const att = makeAttachment({ name: "analysis.csv" });
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      expect(screen.getByText("analysis.csv")).toBeTruthy();
    });

    it("displays the download glyph (SVG icon) inside the button", () => {
      const att = makeAttachment();
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      const button = screen.getByRole("button");
      // DownloadGlyph is an <svg aria-hidden="true"> inside the button
      const svg = button.querySelector("svg");
      expect(svg).not.toBeNull();
    });

    it("displays size when provided", () => {
      const att = makeAttachment({ size: 41_000 }); // ~40 KB
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      // 41 000 / 1024 ≈ 40 → "40 KB"
      expect(screen.getByText("40 KB")).toBeTruthy();
    });

    it("omits size span when size is undefined", () => {
      const att = makeAttachment({ size: undefined });
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      // "KB" should not appear; only the name + download glyph are visible
      expect(screen.queryByText(/KB/i)).toBeNull();
    });

    it('has title attribute for hover tooltip', () => {
      const att = makeAttachment({ name: "readme.md" });
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      const button = screen.getByRole("button");
      expect(button.getAttribute("title")).toBe("Download readme.md");
    });

    it("calls onDownload with the attachment when clicked", () => {
      const att = makeAttachment({ name: "data.json" });
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      fireEvent.click(screen.getByRole("button"));
      expect(onDownload).toHaveBeenCalledTimes(1);
      expect(onDownload).toHaveBeenCalledWith(att);
    });

    it("tone=user applies blue-400 accent class", () => {
      const att = makeAttachment();
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="user" />);
      const button = screen.getByRole("button");
      // The user tone includes blue-400/blue-100 accent classes.
      // We check the rendered class string includes the accent class.
      expect(button.className).toMatch(/blue-400/);
    });

    it("tone=agent omits blue-400 accent class", () => {
      const att = makeAttachment();
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="agent" />);
      const button = screen.getByRole("button");
      expect(button.className).not.toMatch(/blue-400/);
    });

    it("renders exactly one button (no duplicate download targets)", () => {
      const att = makeAttachment({ name: "budget.xlsx", size: 80_000 });
      render(<AttachmentChip attachment={att} onDownload={onDownload} tone="user" />);
      const buttons = screen.getAllByRole("button");
      expect(buttons).toHaveLength(1);
    });
  });
});
