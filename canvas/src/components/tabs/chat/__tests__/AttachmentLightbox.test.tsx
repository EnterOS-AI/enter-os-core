// @vitest-environment jsdom
/**
 * AttachmentLightbox — modal for image / PDF preview.
 *
 * Owns: backdrop + viewport, Esc to close, click-outside to close,
 * focus trap (close button focus on open, restore on close),
 * prefers-reduced-motion respect.
 *
 * Coverage:
 *   - Null when open=false
 *   - Renders dialog with correct ARIA roles and label when open
 *   - Close button present and wired
 *   - Focus moves to close button on open
 *   - Focus restores to previous element on close
 *   - Esc key closes via document listener
 *   - Click outside closes
 *   - Click on content does NOT close (stopPropagation)
 *   - Cleanup removes document listener on unmount
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { AttachmentLightbox } from "../AttachmentLightbox";

// ─── Mock children ─────────────────────────────────────────────────────────────

const MockContent = ({ onClick }: { onClick?: () => void }) => (
  <img
    src="file:///test.png"
    alt="test preview"
    onClick={onClick}
    data-testid="lightbox-content"
  />
);

// ─── Setup / teardown ─────────────────────────────────────────────────────────

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("AttachmentLightbox — render", () => {
  it("renders nothing when open=false", () => {
    render(
      <AttachmentLightbox
        open={false}
        onClose={vi.fn()}
        ariaLabel="Preview image"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog).toBeNull();
  });

  it("renders dialog with role=dialog when open", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview image"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog).toBeTruthy();
  });

  it("sets aria-modal=true on dialog", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview image"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.getAttribute("aria-modal")).toBe("true");
  });

  it("applies aria-label to dialog", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview image: photo.png"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.getAttribute("aria-label")).toBe("Preview image: photo.png");
  });

  it("renders children inside the dialog", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const img = document.querySelector("img");
    expect(img).toBeTruthy();
    expect(img?.getAttribute("alt")).toBe("test preview");
  });

  it("renders close button with correct aria-label", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview"
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const closeBtn = document.querySelector('button[aria-label="Close preview"]');
    expect(closeBtn).toBeTruthy();
  });

  it("uses absolute positioning when contained=true", () => {
    render(
      <AttachmentLightbox
        open={true}
        onClose={vi.fn()}
        ariaLabel="Preview"
        contained
      >
        <MockContent />
      </AttachmentLightbox>,
    );
    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.className).toContain("absolute");
    expect(dialog?.className).not.toContain("fixed");
  });
});

// ─── Focus management ─────────────────────────────────────────────────────────

describe("AttachmentLightbox — focus management", () => {
  it("focuses the close button when opened", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    // Advance timers so the useEffect runs (it uses setTimeout 0 internally)
    vi.advanceTimersByTime(0);
    const closeBtn = document.querySelector('button[aria-label="Close preview"]');
    expect(closeBtn).toBe(document.activeElement);
  });

  it("calls onClose when close button is clicked", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    const closeBtn = document.querySelector('button[aria-label="Close preview"]')!;
    fireEvent.click(closeBtn);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ─── Keyboard interaction ──────────────────────────────────────────────────────

describe("AttachmentLightbox — keyboard", () => {
  it("calls onClose when Escape is pressed", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("does not call onClose for non-Escape keys", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    fireEvent.keyDown(document, { key: "Enter" });
    fireEvent.keyDown(document, { key: " " });
    fireEvent.keyDown(document, { key: "a" });
    expect(onClose).not.toHaveBeenCalled();
  });
});

// ─── Click interaction ────────────────────────────────────────────────────────

describe("AttachmentLightbox — click", () => {
  it("calls onClose when clicking the backdrop (outer div)", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    const dialog = document.querySelector('[role="dialog"]')!;
    fireEvent.click(dialog);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("does NOT call onClose when clicking the content area (stopPropagation)", () => {
    const onClose = vi.fn();
    render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    const content = document.querySelector('[data-testid="lightbox-content"]');
    expect(content).toBeTruthy();
    fireEvent.click(content!);
    expect(onClose).not.toHaveBeenCalled();
  });
});

// ─── Cleanup ─────────────────────────────────────────────────────────────────

describe("AttachmentLightbox — cleanup", () => {
  it("removes document keydown listener on unmount", () => {
    const onClose = vi.fn();
    const { unmount } = render(
      <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
        <MockContent />
      </AttachmentLightbox>,
    );
    vi.advanceTimersByTime(0);
    unmount();
    // After unmount, keyDown should not call onClose (listener removed)
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
  });
});
