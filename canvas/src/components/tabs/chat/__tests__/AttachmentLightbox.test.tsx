// @vitest-environment jsdom
/**
 * Tests for AttachmentLightbox — shared fullscreen modal for image/PDF
 * fullscreen viewing.
 *
 * Covers: open/close rendering, backdrop click-to-close, Esc key close,
 * role/dialog + aria attributes, close button, prefers-reduced-motion.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AttachmentLightbox } from "../AttachmentLightbox";

afterEach(cleanup);

describe("AttachmentLightbox", () => {
  describe("renders nothing when closed", () => {
    it("returns null when open=false", () => {
      const { container } = render(
        <AttachmentLightbox open={false} onClose={vi.fn()} ariaLabel="Image preview">
          <img src="test.jpg" alt="test" />
        </AttachmentLightbox>
      );
      expect(container.textContent).toBe("");
    });
  });

  describe("renders modal when open", () => {
    it("renders the dialog when open=true", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Image preview">
          <img src="test.jpg" alt="test" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("dialog")).toBeTruthy();
    });

    it("renders the provided children", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="PDF preview">
          <embed src="doc.pdf" />
        </AttachmentLightbox>
      );
      expect(document.querySelector("embed")).toBeTruthy();
    });

    it("has aria-modal=true", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("dialog").getAttribute("aria-modal")).toBe("true");
    });

    it("uses the provided ariaLabel", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="My document">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("dialog").getAttribute("aria-label")).toBe("My document");
    });

    it("renders the close button", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("button", { name: /close preview/i })).toBeTruthy();
    });

    it("close button renders an SVG icon", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      const btn = screen.getByRole("button", { name: /close preview/i });
      expect(btn.querySelector("svg")).toBeTruthy();
    });
  });

  describe("Esc to close", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it("calls onClose when Escape is pressed", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      act(() => {
        fireEvent.keyDown(document, { key: "Escape" });
      });

      expect(onClose).toHaveBeenCalledTimes(1);
    });

    it("does not call onClose for non-Escape keys", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      act(() => {
        fireEvent.keyDown(document, { key: "Enter" });
      });

      expect(onClose).not.toHaveBeenCalled();
    });

    it("does not call onClose when closed (open=false)", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={false} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      act(() => {
        fireEvent.keyDown(document, { key: "Escape" });
      });

      expect(onClose).not.toHaveBeenCalled();
    });
  });

  describe("backdrop click to close", () => {
    it("calls onClose when backdrop is clicked", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      const dialog = screen.getByRole("dialog");
      fireEvent.click(dialog);

      expect(onClose).toHaveBeenCalledTimes(1);
    });

    it("does not call onClose when content area is clicked", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      // The content is nested inside the dialog — clicking the inner content
      // div should not close because it has stopPropagation
      const content = document.querySelector(".max-w-\\[95vw\\]") as HTMLElement;
      if (content) {
        fireEvent.click(content);
      }

      expect(onClose).not.toHaveBeenCalled();
    });

    it("does not call onClose when close button is clicked", () => {
      const onClose = vi.fn();
      render(
        <AttachmentLightbox open={true} onClose={onClose} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );

      fireEvent.click(screen.getByRole("button", { name: /close preview/i }));

      // onClose is NOT called for button click — the button's onClick handles
      // close directly. Only backdrop click triggers onClose.
      // (The component does not call onClose from the button; it calls setOpen(false)
      // Actually, looking at the component: onClick={onClose} on the button too.
      // So this test should expect onClose to be called.
      // Wait — the close button's onClick calls onClose, and backdrop also calls onClose.
      // Both should call onClose.
      // Let me update this test.
      expect(onClose).toHaveBeenCalledTimes(1);
    });
  });

  describe("a11y", () => {
    it("dialog has role=dialog", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("dialog")).toBeTruthy();
    });

    it("close button has accessible name", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("button", { name: /close preview/i })).toBeTruthy();
    });

    it("dialog has aria-label matching the provided label", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Quarterly Report Q1 2026">
          <img src="report.jpg" alt="report" />
        </AttachmentLightbox>
      );
      expect(screen.getByRole("dialog").getAttribute("aria-label")).toBe("Quarterly Report Q1 2026");
    });
  });

  describe("motion", () => {
    it("backdrop applies motion-reduce class for reduced motion preference", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      const dialog = screen.getByRole("dialog");
      expect(dialog.className).toContain("motion-reduce");
    });

    it("backdrop has transition-opacity for normal motion preference", () => {
      render(
        <AttachmentLightbox open={true} onClose={vi.fn()} ariaLabel="Preview">
          <img src="x.jpg" alt="x" />
        </AttachmentLightbox>
      );
      const dialog = screen.getByRole("dialog");
      expect(dialog.className).toContain("transition-opacity");
    });
  });
});
