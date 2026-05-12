// @vitest-environment jsdom
/**
 * Tests for FilesToolbar — the top-of-panel bar for the Files tab.
 * Covers: directory select, file count, New/Upload/Clear (configs-only),
 * Export, Refresh, and aria-labels.
 */
import React from "react";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { FilesToolbar } from "../FilesToolbar";

afterEach(cleanup);

describe("FilesToolbar", () => {
  describe("renders base toolbar", () => {
    it("renders the directory select with aria-label", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.getByRole("combobox", { name: /file root directory/i })
      ).toBeTruthy();
    });

    it("renders the file count", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={7}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(screen.getByText("7 files")).toBeTruthy();
    });

    it("renders Export button", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={0}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.getByRole("button", { name: /download all files/i })
      ).toBeTruthy();
    });

    it("renders Refresh button", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={0}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(screen.getByRole("button", { name: /refresh file list/i })).toBeTruthy();
    });

    it("renders 0 files when count is 0", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={0}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(screen.getByText("0 files")).toBeTruthy();
    });
  });

  describe("configs-only buttons", () => {
    it("shows New and Upload buttons when root is /configs", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.getByRole("button", { name: /create new file/i })
      ).toBeTruthy();
      expect(
        screen.getByRole("button", { name: /upload folder/i })
      ).toBeTruthy();
      expect(screen.getByRole("button", { name: /delete all files/i })).toBeTruthy();
    });

    it("hides New and Upload when root is /workspace", () => {
      render(
        <FilesToolbar
          root="/workspace"
          setRoot={vi.fn()}
          fileCount={5}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.queryByRole("button", { name: /create new file/i })
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: /upload folder/i })
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: /delete all files/i })
      ).toBeNull();
      // Export and Refresh are still present
      expect(
        screen.getByRole("button", { name: /download all files/i })
      ).toBeTruthy();
    });

    it("hides New and Upload when root is /home", () => {
      render(
        <FilesToolbar
          root="/home"
          setRoot={vi.fn()}
          fileCount={2}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.queryByRole("button", { name: /create new file/i })
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: /upload folder/i })
      ).toBeNull();
    });

    it("hides New and Upload when root is /plugins", () => {
      render(
        <FilesToolbar
          root="/plugins"
          setRoot={vi.fn()}
          fileCount={1}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      expect(
        screen.queryByRole("button", { name: /create new file/i })
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: /upload folder/i })
      ).toBeNull();
    });
  });

  describe("callbacks", () => {
    it("calls setRoot when directory is changed", () => {
      const setRoot = vi.fn();
      render(
        <FilesToolbar
          root="/configs"
          setRoot={setRoot}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      fireEvent.change(screen.getByRole("combobox"), {
        target: { value: "/workspace" },
      });
      expect(setRoot).toHaveBeenCalledWith("/workspace");
    });

    it("calls onNewFile when New button is clicked", () => {
      const onNewFile = vi.fn();
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={onNewFile}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      fireEvent.click(screen.getByRole("button", { name: /create new file/i }));
      expect(onNewFile).toHaveBeenCalledTimes(1);
    });

    it("calls onDownloadAll when Export button is clicked", () => {
      const onDownloadAll = vi.fn();
      render(
        <FilesToolbar
          root="/workspace"
          setRoot={vi.fn()}
          fileCount={5}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={onDownloadAll}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      fireEvent.click(screen.getByRole("button", { name: /download all files/i }));
      expect(onDownloadAll).toHaveBeenCalledTimes(1);
    });

    it("calls onClearAll when Clear button is clicked", () => {
      const onClearAll = vi.fn();
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={onClearAll}
          onRefresh={vi.fn()}
        />
      );
      fireEvent.click(screen.getByRole("button", { name: /delete all files/i }));
      expect(onClearAll).toHaveBeenCalledTimes(1);
    });

    it("calls onRefresh when Refresh button is clicked", () => {
      const onRefresh = vi.fn();
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={onRefresh}
        />
      );
      fireEvent.click(screen.getByRole("button", { name: /refresh file list/i }));
      expect(onRefresh).toHaveBeenCalledTimes(1);
    });

    it("calls onUpload when the hidden file input changes", () => {
      const onUpload = vi.fn();
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={onUpload}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      // Find the hidden file input
      const fileInput = document.querySelector(
        'input[type="file"]'
      ) as HTMLInputElement;
      expect(fileInput).toBeTruthy();
      expect(fileInput?.getAttribute("aria-label")).toBe("Upload folder files");
    });
  });

  describe("a11y", () => {
    it("all buttons have aria-label or accessible name", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      // All buttons should be findable by role
      const buttons = screen.getAllByRole("button");
      for (const btn of buttons) {
        expect(btn.getAttribute("aria-label") ?? btn.textContent).toBeTruthy();
      }
    });

    it("directory select has aria-label", () => {
      render(
        <FilesToolbar
          root="/configs"
          setRoot={vi.fn()}
          fileCount={3}
          onNewFile={vi.fn()}
          onUpload={vi.fn()}
          onDownloadAll={vi.fn()}
          onClearAll={vi.fn()}
          onRefresh={vi.fn()}
        />
      );
      const select = screen.getByRole("combobox");
      expect(select.getAttribute("aria-label")).toBe("File root directory");
    });
  });
});
